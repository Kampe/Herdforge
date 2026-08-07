//go:build !fac151_hermetic_integration

package verifier

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Ordinary-profile FAC-151 declaration accounting. These tests require
// fac151NativeManifest (also ordinary-only) and must not compile into the
// fac151_hermetic_integration profile that hermeticDockerRunner.compile builds.

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

