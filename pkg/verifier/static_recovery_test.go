package verifier

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type staticModuleImporter struct {
	root, modulePath string
	cache            map[string]*types.Package
	active           map[string]bool
}

func newStaticModuleImporter(root, modulePath string) (*staticModuleImporter, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(modulePath) == "" {
		return nil, errors.New("static importer requires root and module path")
	}
	return &staticModuleImporter{root: filepath.Clean(root), modulePath: strings.TrimSuffix(modulePath, "/"), cache: map[string]*types.Package{}, active: map[string]bool{}}, nil
}

func (i *staticModuleImporter) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if !strings.HasPrefix(path, i.modulePath+"/") {
		return importer.Default().Import(path)
	}
	dir := filepath.Join(i.root, filepath.FromSlash(strings.TrimPrefix(path, i.modulePath+"/")))
	if !pathWithin(i.root, dir) {
		return nil, fmt.Errorf("module import escapes root: %s", path)
	}
	if pkg := i.cache[path]; pkg != nil {
		return pkg, nil
	}
	if i.active[path] {
		return nil, fmt.Errorf("module import cycle at %s", path)
	}
	i.active[path] = true
	defer delete(i.active, path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read module package %s: %w", path, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		ok, err := build.Default.MatchFile(dir, entry.Name())
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		source, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), source, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("module package %s has no buildable Go files", path)
	}
	sort.Slice(files, func(a, b int) bool { return files[a].Name.Name < files[b].Name.Name })
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	pkg, err := (&types.Config{Importer: i}).Check(path, fset, files, info)
	if err != nil {
		return nil, fmt.Errorf("type-check module package %s: %w", path, err)
	}
	i.cache[path] = pkg
	return pkg, nil
}

func TestStaticRecoveryModuleImporter(t *testing.T) {
	root := t.TempDir()
	writeStaticFixture(t, root, "dep", "package dep\nimport \"unsafe\"\nfunc Value() uintptr { return unsafe.Sizeof(uintptr(0)) }\n")
	writeStaticFixture(t, root, "pkg", "package pkg\nimport (\"os\"; \"os/exec\"; \"syscall\"; \"example.test/dep\")\nvar value = dep.Value()\nfunc Safe(p *os.Process) { c := exec.Command(\"true\"); _ = c.Start; _ = p.Kill; _ = syscall.Kill }\n")
	imp, err := newStaticModuleImporter(root, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	source, err := os.ReadFile(filepath.Join(root, "pkg", "pkg.go"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(fset, filepath.Join(root, "pkg", "pkg.go"), source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	if _, err := (&types.Config{Importer: imp}).Check("example.test/pkg", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("module-aware static check failed: %v", err)
	}
	if _, err := imp.Import("unsafe"); err != nil {
		t.Fatal(err)
	}
	if _, err := imp.Import("example.test/missing"); err == nil {
		t.Fatal("missing package must fail closed")
	}
	if !hasForbiddenUse(file, info) {
		t.Fatal("forbidden call was not resolved")
	}
}

func TestStaticRecoveryCrossFileClassesAndControls(t *testing.T) {
	sources := []string{
		"package fixture\nimport (\"os\"; \"os/exec\"; \"syscall\")\nfunc direct(p *os.Process) { p.Signal(syscall.SIGTERM) }\nfunc method(p *os.Process) { f := p.Signal; _ = f }\nfunc alias() { f := syscall.Kill; _ = f }\nfunc safe() { c := exec.Command(\"true\"); _ = c.Start; _ = exec.Command }\n",
		"package fixture\nimport \"os\"\nfunc helper(p *os.Process) { _ = p.Signal }\nfunc cross(p *os.Process) { helper(p) }\n",
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for n, src := range sources {
		file, err := parser.ParseFile(fset, fmt.Sprintf("%d.go", n), src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	if _, err := (&types.Config{Importer: importer.Default()}).Check("fixture", fset, files, info); err != nil {
		t.Fatal(err)
	}
	if !hasForbiddenUse(files[0], info) || !hasForbiddenUse(files[1], info) {
		t.Fatal("direct and cross-file forbidden classes must resolve")
	}
}

func TestStaticRecoveryInitializerGuards(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantErr  bool
		identity string
	}{
		{name: "init", src: "package p\nfunc init() {}\n", wantErr: true, identity: "declares init"},
		{name: "make", src: "package p\nvar slots = make(chan struct{}, 2)\n", wantErr: false},
		{name: "nested-user-call", src: "package p\nfunc inner() int { return 1 }\nfunc outer(int) int { return 1 }\nvar x = outer(inner())\n", wantErr: true, identity: "outer"},
		{name: "selector-call", src: "package p\nvar x = packageName.Call()\n", wantErr: true, identity: "packageName.Call"},
		{name: "function-literal", src: "package p\nvar x = func() int { return 1 }()\n", wantErr: true, identity: "<function literal>"},
		{name: "constant", src: "package p\nvar value = 1\n", wantErr: false},
	}
	for _, tc := range cases {
		path := "fixtures/" + tc.name + ".go"
		file, err := parser.ParseFile(token.NewFileSet(), path, tc.src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		gotErr := validatePreAdmissionInitializers(file, path, map[string]struct{}{})
		if (gotErr != nil) != tc.wantErr {
			t.Fatalf("%s: initializer error=%v want error=%v", tc.name, gotErr, tc.wantErr)
		}
		if tc.wantErr && (!strings.Contains(gotErr.Error(), path) || !strings.Contains(gotErr.Error(), tc.identity)) {
			t.Fatalf("%s: diagnostic=%q missing path=%q or identity=%q", tc.name, gotErr, path, tc.identity)
		}
	}
}

func TestStaticRecoveryGuardsActualOrdinaryVerifierSources(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(testFile)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read verifier source directory: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	filePaths := make(map[*ast.File]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		matches, err := build.Default.MatchFile(root, entry.Name())
		if err != nil {
			t.Fatalf("match verifier test %s: %v", entry.Name(), err)
		}
		if !matches {
			continue
		}
		path := filepath.Join(root, entry.Name())
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read verifier test %s: %v", entry.Name(), err)
		}
		file, err := parser.ParseFile(fset, path, source, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse verifier test %s: %v", entry.Name(), err)
		}
		files = append(files, file)
		filePaths[file] = path
	}
	if len(files) == 0 {
		t.Fatal("no ordinary verifier test sources found")
	}
	declared := make(map[string]struct{})
	for _, file := range files {
		for name := range declarationNames(file) {
			declared[name] = struct{}{}
		}
	}
	for _, file := range files {
		if err := guardOrdinaryVerifierFile(file, filePaths[file], declared); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStaticRecoveryInitializerBuiltinShadowing(t *testing.T) {
	sources := map[string]string{
		"a_test.go": "package p\nfunc make() int { return 1 }\n",
		"b_test.go": "package p\nvar slots = make(chan struct{}, 2)\n",
	}
	files := make(map[string]*ast.File, len(sources))
	declared := make(map[string]struct{})
	for _, path := range []string{"a_test.go", "b_test.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), path, sources[path], parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = file
		for name := range declarationNames(file) {
			declared[name] = struct{}{}
		}
	}
	if err := validatePreAdmissionInitializers(files["b_test.go"], "b_test.go", declared); err == nil ||
		!strings.Contains(err.Error(), "b_test.go") || !strings.Contains(err.Error(), "make") {
		t.Fatalf("shadowed make initializer error = %v, want path and shadowed identity", err)
	}
	unshadowed, err := parser.ParseFile(token.NewFileSet(), "unshadowed_test.go", sources["b_test.go"], parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePreAdmissionInitializers(unshadowed, "unshadowed_test.go", map[string]struct{}{}); err != nil {
		t.Fatalf("unshadowed make initializer rejected: %v", err)
	}
}

func TestFAC151DeclarationAccounting(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(testFile)
	if len(fac151NativeManifest) != 53 {
		t.Fatalf("manifest cardinality = %d, want 53", len(fac151NativeManifest))
	}
	manifestNames := make(map[string][]string, len(fac151NativeManifest))
	for key := range fac151NativeManifest {
		parts := strings.SplitN(key, "::", 2)
		if len(parts) != 2 || parts[1] == "" {
			t.Fatalf("invalid manifest key %q", key)
		}
		manifestNames[parts[1]] = append(manifestNames[parts[1]], key)
	}
	for name, keys := range manifestNames {
		if len(keys) != 1 {
			t.Fatalf("manifest declaration identity %q is ambiguous: %v", name, keys)
		}
	}
	ordinary := make(map[string][]string)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		matches, err := build.Default.MatchFile(root, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !matches {
			continue
		}
		path := filepath.Join(root, entry.Name())
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for name := range topLevelDeclarationNames(t, path, source) {
			ordinary[name] = append(ordinary[name], path)
		}
	}
	if err := rejectManifestNames(manifestNames, ordinary); err != nil {
		t.Fatal(err)
	}
	nativePath := filepath.Join(root, "fac151_native_integration_test.go")
	nativeSource, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	native := topLevelDeclarationNames(t, nativePath, nativeSource)
	nativeDigests := declarationDigests(t, nativePath, nativeSource)
	if len(native) != 53 || len(nativeDigests) != 53 {
		t.Fatalf("tagged native declaration cardinality = names:%d digests:%d, want 53", len(native), len(nativeDigests))
	}
	expected := make(map[string]struct{}, len(fac151NativeManifest))
	for key := range fac151NativeManifest {
		parts := strings.SplitN(key, "::", 2)
		if len(parts) != 2 {
			t.Fatalf("invalid manifest key %q", key)
		}
		expected[parts[1]] = struct{}{}
		if _, exists := native[parts[1]]; !exists {
			t.Fatalf("manifest declaration missing from tagged native source: %s", key)
		}
		if got := nativeDigests[parts[1]]; got != fac151NativeManifest[key] {
			t.Fatalf("native declaration digest changed for %s: got %s want %s", key, got, fac151NativeManifest[key])
		}
	}
	for name := range native {
		if _, exists := expected[name]; !exists {
			t.Fatalf("unmanifested declaration in tagged native source: %s", name)
		}
	}
}

func rejectManifestNames(manifestNames map[string][]string, ordinary map[string][]string) error {
	for name, paths := range ordinary {
		if keys, quarantined := manifestNames[name]; quarantined {
			return fmt.Errorf("manifest declaration %q remains in ordinary sources %v (manifest %v)", name, paths, keys)
		}
		if len(paths) > 1 {
			return fmt.Errorf("ordinary declaration identity %q is ambiguous across %v", name, paths)
		}
	}
	return nil
}

func TestFAC151DeclarationAccountingRejectsRelocationAdversary(t *testing.T) {
	manifestName := "TestExecuteCancellationKillsProcessGroup"
	manifestNames := map[string][]string{manifestName: []string{"pkg/verifier/verifier_test.go::" + manifestName}}
	ordinary, err := indexDeclarationSources(map[string]string{
		"relocated_test.go": "package verifier\nfunc " + manifestName + "() {}\n",
		"safe_test.go":      "package verifier\nfunc TestUnrelatedSafeDeclaration() {}\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rejectManifestNames(manifestNames, ordinary); err == nil {
		t.Fatal("relocated manifest declaration was accepted")
	}
	if locations := ordinary["TestUnrelatedSafeDeclaration"]; len(locations) != 1 || locations[0] != "safe_test.go" {
		t.Fatalf("unrelated safe declaration locations = %v", locations)
	}
}

func indexDeclarationSources(sources map[string]string) (map[string][]string, error) {
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	locations := make(map[string][]string)
	for _, path := range paths {
		file, err := parser.ParseFile(token.NewFileSet(), path, sources[path], parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for name := range declarationNames(file) {
			locations[name] = append(locations[name], path)
		}
	}
	return locations, nil
}

func declarationDigests(t *testing.T, path string, source []byte) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse declaration digest source %s: %v", path, err)
	}
	result := make(map[string]string)
	for _, decl := range file.Decls {
		start := fset.Position(decl.Pos()).Offset
		end := fset.Position(decl.End()).Offset
		digest := fmt.Sprintf("%x", sha256.Sum256(source[start:end]))
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = receiverName(d.Recv.List[0].Type) + "." + name
			}
			result[name] = digest
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					result[s.Name.Name] = digest
				case *ast.ValueSpec:
					for _, name := range s.Names {
						result[name.Name] = digest
					}
				}
			}
		}
	}
	return result
}

func topLevelDeclarationNames(t *testing.T, path string, source []byte) map[string]struct{} {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse declaration source %s: %v", path, err)
	}
	return declarationNames(file)
}

func declarationNames(file *ast.File) map[string]struct{} {
	result := make(map[string]struct{})
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = receiverName(d.Recv.List[0].Type) + "." + name
			}
			result[name] = struct{}{}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					result[s.Name.Name] = struct{}{}
				case *ast.ValueSpec:
					for _, name := range s.Names {
						result[name.Name] = struct{}{}
					}
				}
			}
		}
	}
	return result
}

func receiverName(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.StarExpr:
		return "*" + receiverName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return receiverName(x.X)
	case *ast.IndexListExpr:
		return receiverName(x.X)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

type ordinarySourceBindings struct {
	forbiddenPackages map[string]string
	dotForbidden      bool
}

func guardOrdinaryVerifierFile(file *ast.File, path string, declared map[string]struct{}) error {
	bindings := ordinaryBindings(file)
	if bindings == nil {
		return fmt.Errorf("ordinary verifier source %s has unreadable import binding", path)
	}
	if err := validatePreAdmissionInitializers(file, path, declared); err != nil {
		return err
	}
	var guardErr error
	ast.Inspect(file, func(node ast.Node) bool {
		if guardErr != nil {
			return false
		}
		switch n := node.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.SelectorExpr:
			name := n.Sel.Name
			if !suspiciousForbiddenName(name) {
				return true
			}
			if base, ok := n.X.(*ast.Ident); ok {
				if pkgPath, imported := bindings.forbiddenPackages[base.Name]; imported && forbiddenPackageFunction(pkgPath, name) {
					guardErr = fmt.Errorf("ordinary verifier source %s resolves forbidden %s.%s", path, pkgPath, name)
					return false
				}
				if pkgPath, imported := bindings.forbiddenPackages[base.Name]; imported && pkgPath == "syscall" && name == "Signal" {
					return true
				}
			}
			guardErr = fmt.Errorf("ordinary verifier source %s has unresolved suspicious selector .%s", path, name)
			return false
		case *ast.Ident:
			if bindings.dotForbidden && suspiciousForbiddenName(n.Name) {
				guardErr = fmt.Errorf("ordinary verifier source %s has forbidden dot-import use %s", path, n.Name)
				return false
			}
		}
		return true
	})
	return guardErr
}

func validatePreAdmissionInitializers(file *ast.File, path string, declared map[string]struct{}) error {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
			return fmt.Errorf("ordinary verifier source %s declares init", path)
		}
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, expr := range value.Values {
				if err := validateInitializerExpr(expr, declared); err != nil {
					return fmt.Errorf("ordinary verifier source %s: %w", path, err)
				}
			}
		}
	}
	return nil
}

func validateInitializerExpr(expr ast.Expr, declared map[string]struct{}) error {
	if selector := firstInitializerSelector(expr); selector != nil {
		return fmt.Errorf("initializer selector %s is not admitted", selectorIdentity(selector))
	}
	call := firstInitializerCall(expr)
	if call == nil {
		return nil
	}
	if ident, ok := call.Fun.(*ast.Ident); ok && (ident.Name == "make" || ident.Name == "new") {
		if _, shadowed := declared[ident.Name]; shadowed {
			return fmt.Errorf("initializer builtin %s is shadowed by ordinary declaration", ident.Name)
		}
	}
	if isBoundedBuiltinCall(call) {
		for _, arg := range call.Args {
			nested := initializerCalls(arg)
			if len(nested) > 0 {
				return fmt.Errorf("nested initializer call %s inside %s is not admitted", callIdentity(nested[0]), callIdentity(call))
			}
		}
		return nil
	}
	return fmt.Errorf("initializer call %s is not admitted", callIdentity(call))
}

func isBoundedBuiltinCall(call *ast.CallExpr) bool {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return false
	}
	if ident.Name == "make" || ident.Name == "new" {
		return true
	}
	return ident.Name == "make" || ident.Name == "new"
}

func firstInitializerCall(expr ast.Expr) *ast.CallExpr {
	calls := initializerCalls(expr)
	if len(calls) == 0 {
		return nil
	}
	return calls[0]
}

func initializerCalls(expr ast.Expr) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(expr, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func firstInitializerSelector(expr ast.Expr) *ast.SelectorExpr {
	var found *ast.SelectorExpr
	ast.Inspect(expr, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			found = selector
			return false
		}
		return found == nil
	})
	return found
}

func selectorIdentity(selector *ast.SelectorExpr) string {
	if ident, ok := selector.X.(*ast.Ident); ok {
		return ident.Name + "." + selector.Sel.Name
	}
	return "." + selector.Sel.Name
}

func callIdentity(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return selectorIdentity(fun)
	case *ast.FuncLit:
		return "<function literal>"
	default:
		return "<unresolved call>"
	}
}

func ordinaryBindings(file *ast.File) *ordinarySourceBindings {
	bindings := &ordinarySourceBindings{forbiddenPackages: make(map[string]string)}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil
		}
		if spec.Name != nil && spec.Name.Name == "_" {
			continue
		}
		if spec.Name != nil && spec.Name.Name == "." {
			bindings.dotForbidden = bindings.dotForbidden || forbiddenPackagePath(importPath)
			continue
		}
		alias := filepath.Base(importPath)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if forbiddenPackagePath(importPath) {
			bindings.forbiddenPackages[alias] = importPath
		}
	}
	return bindings
}

func forbiddenPackagePath(path string) bool {
	return path == "os" || path == "syscall" || path == "golang.org/x/sys/unix"
}

func forbiddenPackageFunction(path, name string) bool {
	return (path == "os" && (name == "Kill" || name == "Signal")) ||
		((path == "syscall" || path == "golang.org/x/sys/unix") && (name == "Kill" || strings.HasPrefix(name, "Syscall")))
}

func suspiciousForbiddenName(name string) bool {
	return name == "Kill" || name == "Signal" || strings.HasPrefix(name, "Syscall")
}

func writeStaticFixture(t *testing.T, root, name, src string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasForbiddenCall(file *ast.File, info *types.Info) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		var obj types.Object
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			obj = info.Uses[fun]
		case *ast.SelectorExpr:
			if s := info.Selections[fun]; s != nil {
				obj = s.Obj()
			} else {
				obj = info.Uses[fun.Sel]
			}
		}
		fn, ok := obj.(*types.Func)
		if !ok || fn.Pkg() == nil {
			return true
		}
		path, name := fn.Pkg().Path(), fn.Name()
		if (path == "os" && (name == "Kill" || name == "Signal")) || ((path == "syscall" || path == "golang.org/x/sys/unix") && (name == "Kill" || strings.HasPrefix(name, "Syscall"))) {
			found = true
		}
		return true
	})
	return found
}

func hasForbiddenUse(file *ast.File, info *types.Info) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		var object types.Object
		switch n := node.(type) {
		case *ast.Ident:
			object = info.Uses[n]
		case *ast.SelectorExpr:
			if selection := info.Selections[n]; selection != nil {
				object = selection.Obj()
			} else {
				object = info.Uses[n.Sel]
			}
		}
		fn, ok := object.(*types.Func)
		if !ok || fn.Pkg() == nil {
			return true
		}
		path, name := fn.Pkg().Path(), fn.Name()
		if (path == "os" && (name == "Kill" || name == "Signal")) || ((path == "syscall" || path == "golang.org/x/sys/unix") && (name == "Kill" || strings.HasPrefix(name, "Syscall"))) {
			found = true
		}
		return true
	})
	return found
}
