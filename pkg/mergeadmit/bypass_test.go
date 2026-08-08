package mergeadmit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// FAC-156 acceptance: "All production coordinator/forge-loop/manual merge call
// sites reach the same compiled admission function; a graph test fails if a
// bypass exists."
//
// This is that test. It parses every non-test Go file in the repository and
// enforces three structural invariants. It is deliberately a source-shape
// check rather than a runtime one: a bypass is something a future change ADDS,
// and no runtime test of the current call graph can notice a new edge that
// nobody wired into it.
//
// The RCA these rules encode: reviewledger.Admit, runReviewGate,
// RequireCurrentPassing, ReceiptAdmission and VerifyAndPersist were all
// shipped and then left with ZERO production callers, while the real merge
// path stayed a coordinator shell pipeline. Compiled gates with no callers are
// not gates. These rules keep the reverse from happening too — a caller that
// reaches around the gate.

// repoRoot is this package's directory, two levels below the module root.
const repoRoot = "../.."

// admissionCallers are the ONLY production packages permitted to run
// reviewledger admission. Both are compiled, tested merge gates:
//
//   - pkg/mergeadmit — the coordinator's PR merge authority (this package)
//   - pkg/harvest    — the local worktree cherry-pick integration pipeline
//
// Adding a package here is a deliberate, reviewable act. A new merge path that
// simply calls Admit from somewhere else fails this test.
var admissionCallers = map[string]bool{
	"pkg/mergeadmit": true,
	"pkg/harvest":    true,
}

// receiptMinters are the ONLY production packages permitted to mint a
// completion receipt. A receipt is what pkg/sync.BoardDone accepts as closing
// authority, so minting one IS claiming a card may close.
var receiptMinters = map[string]bool{
	"pkg/mergeadmit": true, // Gate.Complete, after a proved integration
	"pkg/sync":       true, // WriteReceipt itself lives here
}

type goFile struct {
	relPath string // repo-relative, slash-separated
	relDir  string
	file    *ast.File
	fset    *token.FileSet
}

// productionGoFiles parses every non-test Go file in the module.
func productionGoFiles(t *testing.T) []goFile {
	t.Helper()
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	var out []goFile
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata", ".herd":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return nil // not our job to police syntax; the build does that
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		out = append(out, goFile{relPath: rel, relDir: filepath.ToSlash(filepath.Dir(rel)), file: f, fset: fset})
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	// A scan that found nothing would pass every rule below vacuously.
	if len(out) < 100 {
		t.Fatalf("only %d production go files found under %s; the scan is not covering the repository", len(out), root)
	}
	return out
}

// TestNoMergeAdmissionBypass is the anti-bypass invariant. If it fails, a new
// merge path was added that does not route through a compiled gate — fix the
// path, or add the package to the allowlist above and say why in the commit.
func TestNoMergeAdmissionBypass(t *testing.T) {
	files := productionGoFiles(t)

	var admissionSites, receiptSites, ghMergeSites []string

	for _, gf := range files {
		ast.Inspect(gf.file, func(n ast.Node) bool {
			switch node := n.(type) {
			// reviewledger.AdmissionOpts{...} appears at exactly the sites that
			// call Ledger.Admit, and nowhere else. Matching the composite
			// literal avoids needing full type resolution to tell this Admit
			// apart from resources.DiskAdmission.Admit.
			case *ast.CompositeLit:
				if isSelector(node.Type, "reviewledger", "AdmissionOpts") {
					admissionSites = append(admissionSites, gf.relPath+":"+pos(gf, node.Pos()))
				}
			// Any REFERENCE to WriteReceipt, not just a call. Taking the
			// function as a value (`var w = sync.WriteReceipt`) hands the same
			// authority to whoever holds it, so it is caught the same way.
			case *ast.SelectorExpr:
				if node.Sel.Name == "WriteReceipt" {
					receiptSites = append(receiptSites, gf.relPath+":"+pos(gf, node.Pos()))
				}
			case *ast.CallExpr:
				if isGhMerge(node) {
					ghMergeSites = append(ghMergeSites, gf.relPath+":"+pos(gf, node.Pos()))
				}
			}
			return true
		})
	}

	// Rule 1: admission runs only inside an allowlisted compiled gate.
	if len(admissionSites) == 0 {
		t.Fatal("no production caller of reviewledger admission found at all. " +
			"That is the FAC-156 RCA state: a compiled gate that nothing invokes is not a gate.")
	}
	for _, site := range admissionSites {
		if !admissionCallers[pkgDirOf(site)] {
			t.Errorf("merge-admission BYPASS: %s runs reviewledger admission outside a compiled gate.\n"+
				"  Route the merge through pkg/mergeadmit.Gate.Admit, or add %q to admissionCallers with a reason.",
				site, pkgDirOf(site))
		}
	}

	// Rule 2: only a proved integration mints a completion receipt.
	if len(receiptSites) == 0 {
		t.Fatal("no production caller of sync.WriteReceipt found. " +
			"Before FAC-156 that was literally true, and it meant `herd approve` could never close a card " +
			"from evidence — every closure fell back to a manual override.")
	}
	for _, site := range receiptSites {
		if !receiptMinters[pkgDirOf(site)] {
			t.Errorf("closing-authority BYPASS: %s mints a completion receipt outside a proved integration.\n"+
				"  A receipt is what BoardDone accepts as closing authority; mint it from mergeadmit.Gate.Complete.",
				site)
		}
	}

	// Rule 3: no Go code shells out to `gh ... merge ...`. If the merge ever
	// moves into Go it must be behind the gate, not beside it.
	for _, site := range ghMergeSites {
		t.Errorf("merge-authority BYPASS: %s invokes `gh merge` directly. "+
			"The merge must be admitted by mergeadmit.Gate.Admit first.", site)
	}
}

// The allowlists must name real, present packages. A stale entry silently
// widens the gate, and an allowlist nobody notices is how a bypass becomes
// permanent.
func TestBypassAllowlistsAreNotStale(t *testing.T) {
	files := productionGoFiles(t)
	dirs := map[string]bool{}
	for _, gf := range files {
		dirs[gf.relDir] = true
	}
	for _, list := range []map[string]bool{admissionCallers, receiptMinters} {
		for pkg := range list {
			if !dirs[pkg] {
				t.Errorf("allowlisted package %q no longer exists; remove the stale entry rather than "+
					"leaving a permanently-open door", pkg)
			}
		}
	}
}

// isGhMerge matches exec.Command("gh", ..., "merge", ...) — the shape of a
// direct PR merge from Go.
func isGhMerge(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext") {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" {
		return false
	}
	var gh, merge bool
	for _, arg := range call.Args {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			continue
		}
		switch v {
		case "gh":
			gh = true
		case "merge":
			merge = true
		}
	}
	return gh && merge
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// pkgDirOf recovers the package directory from a "dir/file.go:line" site.
func pkgDirOf(site string) string {
	path := site
	if i := strings.LastIndex(site, ".go:"); i >= 0 {
		path = site[:i+3]
	}
	return filepath.ToSlash(filepath.Dir(path))
}

func pos(gf goFile, p token.Pos) string {
	return strconv.Itoa(gf.fset.Position(p).Line)
}
