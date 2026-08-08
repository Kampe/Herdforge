// Command check-test-hermeticity scans *_test.go files for patterns that make
// tests machine-dependent: they pass on a developer box but fail on a clean CI
// runner. Six CI breaks in one coordinator session were all this class.
//
// Detected signals:
//
//  1. exec.LookPath without t.Skip — fails hard instead of skipping when a
//     binary is absent (FAC-215: TestShotRoutesByFirstArgument required
//     codex/claude/grok to be INSTALLED ON PATH).
//
//  2. exec.Command on a fragile binary without a preceding LookPath+Skip
//     guard in the same function — the binary may not be on a clean CI runner.
//
//  3. Docker "image inspect" with "--platform" — the flag only exists from
//     Docker 28; CI runs an older daemon.
//
//  4. Index-based argv position assertions — X[int_lit] == "--flag" pins a
//     flag to a position that breaks when flags are prepended or reordered
//     (FAC-173: want[1] == "--model" broke when two deny flags were prepended).
//     Suppress with a trailing //hermetic:allow-argv-position comment.
//
// Usage: go run ./scripts/check-test-hermeticity.go [dir...]
//
// Default dirs: pkg/ cmd/ contracts/agentscope/
// Exit 0 if all test files are hermetic; exit 1 with findings otherwise.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// fragileBinaries are not guaranteed on a clean Linux CI runner. git, go,
// sh, bash, echo, rm, true, false, sleep, and cat are always present and
// excluded from this set.
var fragileBinaries = map[string]bool{
	"docker":    true,
	"podman":    true,
	"pg_ctl":    true,
	"psql":      true,
	"initdb":    true,
	"pg_dump":   true,
	"gh":        true,
	"codex":     true,
	"claude":    true,
	"grok":      true,
	"herdr":     true,
	"pi":        true,
	"python3":   true,
	"python":    true,
	"jq":        true,
	"setpriv":   true,
	"kubectl":   true,
	"helm":      true,
	"argocd":    true,
	"colima":    true,
	"stern":     true,
	"node":      true,
	"npm":       true,
	"pnpm":      true,
	"bun":       true,
	"uv":        true,
	"cargo":     true,
	"rustc":     true,
	"playwright": true,
}

// argvLikeNames are variable names that hold command-line argument arrays.
// Asserting a flag at a fixed index in these is the FAC-173 pattern.
var argvLikeNames = map[string]bool{
	"argv":    true,
	"want":    true,
	"gotArgs": true,
}

type finding struct {
	file    string
	line    int
	message string
}

func main() {
	dirs := os.Args[1:]
	if len(dirs) == 0 {
		dirs = []string{"pkg", "cmd", "contracts/agentscope"}
	}

	var findings []finding
	fset := token.NewFileSet()

	for _, root := range dirs {
		rootFindings := scanDir(fset, root)
		findings = append(findings, rootFindings...)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})

	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "%s:%d: %s\n", f.file, f.line, f.message)
	}

	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d hermeticity finding(s). Suppress argv-position findings with //hermetic:allow-argv-position\n", len(findings))
		os.Exit(1)
	}
}

func scanDir(fset *token.FileSet, root string) []finding {
	var findings []finding

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := info.Name()
			if base == "vendor" || base == "node_modules" || base == ".git" || base == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fileFindings := scanFile(fset, path)
		findings = append(findings, fileFindings...)
		return nil
	})

	return findings
}

func scanFile(fset *token.FileSet, path string) []finding {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil
	}

	var findings []finding

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if !isTestFunc(fn) {
			continue
		}

		// Pre-compute function-level facts.
		hasSkip := funcContainsSkip(fn)
		hasAnyLookPath := funcHasAnyLookPath(fn)
		lookPathBinaries := funcLookPathBinaries(fn)

		// Check 1: exec.LookPath without t.Skip in the same function.
		if hasAnyLookPath && !hasSkip {
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if isExecLookPath(call) {
					pos := fset.Position(call.Pos())
					findings = append(findings, finding{
						file:    path,
						line:    pos.Line,
						message: "exec.LookPath without t.Skip: test will FAIL (not skip) when the binary is absent on CI",
					})
				}
				return true
			})
		}

		// Check 2: exec.Command on a fragile binary without LookPath+Skip guard.
		// If the function has t.Skip AND any exec.LookPath call (even with a
		// loop variable), consider all fragile binaries guarded — the common
		// pattern checks all binaries up front then uses them throughout.
		fragileGuarded := hasSkip && hasAnyLookPath
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			bin := execCommandBinary(call)
			if bin == "" || !fragileBinaries[bin] {
				return true
			}
			if fragileGuarded || lookPathBinaries[bin] {
				return true
			}
			pos := fset.Position(call.Pos())
			findings = append(findings, finding{
				file:    path,
				line:    pos.Line,
				message: fmt.Sprintf("exec.Command(%q) without exec.LookPath+t.Skip guard: %q may not be on a clean CI runner", bin, bin),
			})
			return true
		})

		// Check 3: Docker image inspect --platform (string literals).
		ast.Inspect(fn, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if strings.Contains(val, "image inspect") && strings.Contains(val, "--platform") {
				pos := fset.Position(lit.Pos())
				findings = append(findings, finding{
					file:    path,
					line:    pos.Line,
					message: "docker 'image inspect' with '--platform': flag is Docker 28+ only, CI runs older",
				})
			}
			return true
		})
	}

	// Check 4: Index-based argv position assertions.
	suppressed := parseSuppressions(fset, src)
	ast.Inspect(f, func(n ast.Node) bool {
		binexpr, ok := n.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		if binexpr.Op != token.EQL && binexpr.Op != token.NEQ {
			return true
		}
		idx, str := extractIndexStringPair(binexpr)
		if idx == nil || str == "" {
			return true
		}
		// Only flag literal indices > 0.
		lit, ok := idx.Index.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return true
		}
		pos, err := strconv.Atoi(lit.Value)
		if err != nil || pos <= 0 {
			return true
		}
		// Only flag when the string starts with '-' (a flag) OR the variable
		// name is argv-like. Flag names like "--model", "-c" are the FAC-173
		// pattern. Non-flag strings at argv positions (like "2", "/bin/cat")
		// are only flagged when the variable is argv-like.
		isFlag := strings.HasPrefix(str, "-")
		ident, ok := idx.X.(*ast.Ident)
		if !ok {
			return true
		}
		isArgvLike := argvLikeNames[ident.Name]
		if !isFlag && !isArgvLike {
			return true
		}

		line := fset.Position(binexpr.Pos()).Line
		if suppressed[line] {
			return true
		}

		findings = append(findings, finding{
			file:    path,
			line:    line,
			message: fmt.Sprintf("index-based argv assertion: %s[%s] %s %q pins position %s; search for the flag instead of indexing. Suppress with //hermetic:allow-argv-position", ident.Name, lit.Value, binexpr.Op, str, lit.Value),
		})
		return true
	})

	return findings
}

func isTestFunc(fn *ast.FuncDecl) bool {
	if fn == nil || fn.Name == nil {
		return false
	}
	name := fn.Name.Name
	return strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark") || strings.HasPrefix(name, "Fuzz")
}

func isExecLookPath(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "LookPath" {
		return false
	}
	// Must be exec.LookPath, not a method call like cfg.LookPath().
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == "exec"
}

func execCommandBinary(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
		return ""
	}
	// Must be exec.Command, not a method call like cmd.Command().
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "exec" {
		return ""
	}
	// For CommandContext, the first arg is ctx; the binary is the second.
	argIdx := 0
	if sel.Sel.Name == "CommandContext" {
		argIdx = 1
	}
	if len(call.Args) <= argIdx {
		return ""
	}
	lit, ok := call.Args[argIdx].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	val, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return val
}

func funcContainsSkip(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Skip" || sel.Sel.Name == "Skipf" || sel.Sel.Name == "SkipNow" {
			found = true
			return false
		}
		return true
	})
	return found
}

func funcHasAnyLookPath(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isExecLookPath(call) {
			found = true
			return false
		}
		return true
	})
	return found
}

func funcLookPathBinaries(fn *ast.FuncDecl) map[string]bool {
	bins := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isExecLookPath(call) {
			return true
		}
		if len(call.Args) < 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		bins[val] = true
		return true
	})
	return bins
}

// extractIndexStringPair returns the index expression and string literal from
// a binary comparison. Either side may be the index; the other must be a
// string literal.
func extractIndexStringPair(b *ast.BinaryExpr) (*ast.IndexExpr, string) {
	// Left is index, right is string.
	if idx, ok := b.X.(*ast.IndexExpr); ok {
		if lit, ok := b.Y.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if val, err := strconv.Unquote(lit.Value); err == nil {
				return idx, val
			}
		}
	}
	// Right is index, left is string.
	if idx, ok := b.Y.(*ast.IndexExpr); ok {
		if lit, ok := b.X.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if val, err := strconv.Unquote(lit.Value); err == nil {
				return idx, val
			}
		}
	}
	return nil, ""
}

// parseSuppressions returns a set of line numbers that carry a
// //hermetic:allow-argv-position suppression comment.
func parseSuppressions(fset *token.FileSet, src []byte) map[int]bool {
	suppressed := map[int]bool{}
	lines := strings.Split(string(src), "\n")
	for i, line := range lines {
		if strings.Contains(line, "hermetic:allow-argv-position") {
			suppressed[i+1] = true // 1-indexed line numbers
		}
	}
	return suppressed
}
