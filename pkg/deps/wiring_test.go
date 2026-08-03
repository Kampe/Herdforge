package deps_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestProductionLaunchWiring_GateBeforeSideEffects proves non-test production
// sources call RequireTaskLaunch / FencedClaim before worktree/claim side effects.
func TestProductionLaunchWiring_GateBeforeSideEffects(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

	dispatchSrc, err := os.ReadFile(filepath.Join(root, "pkg/dispatch/dispatch.go"))
	if err != nil {
		t.Fatal(err)
	}
	dispatchFn := extractFuncBody(string(dispatchSrc), "func (d *Dispatcher) Dispatch")
	if dispatchFn == "" {
		t.Fatal("Dispatcher.Dispatch not found")
	}
	// Must call RequireTaskLaunch twice (selection + re-read) before worktree.
	countRTL := strings.Count(dispatchFn, "RequireTaskLaunch")
	if countRTL < 2 {
		t.Fatalf("Dispatch must call RequireTaskLaunch at least twice (selection+re-read), got %d", countRTL)
	}
	// Post-check after side effects.
	if !strings.Contains(dispatchFn, "post_dispatch_graph_drift") {
		t.Fatal("Dispatch must post-validate with compensation reason post_dispatch_graph_drift")
	}
	selIdx := strings.Index(dispatchFn, "RequireTaskLaunch")
	wtIdx := strings.Index(dispatchFn, "CreateTaskWorktreeFrom")
	if selIdx < 0 || wtIdx < 0 || selIdx > wtIdx {
		t.Fatalf("RequireTaskLaunch must precede CreateTaskWorktreeFrom (sel=%d wt=%d)", selIdx, wtIdx)
	}

	engineSrc, err := os.ReadFile(filepath.Join(root, "pkg/daemon/engine.go"))
	if err != nil {
		t.Fatal(err)
	}
	runPulse := extractFuncBody(string(engineSrc), "func (e *Engine) RunPulse")
	if runPulse == "" {
		t.Fatal("Engine.RunPulse not found")
	}
	if !strings.Contains(runPulse, "FencedClaim") || !strings.Contains(runPulse, "claimTaskBound") {
		t.Fatal("RunPulse must use FencedClaim wrapping claimTaskBound")
	}
	if !strings.Contains(runPulse, "ClaimExclusive") || !strings.Contains(runPulse, "CompensateIfOwner") {
		t.Fatal("RunPulse must acquire durable claim lease (ClaimExclusive) and generation-fenced CompensateIfOwner")
	}
	if !strings.Contains(dispatchFn, "OpenLeaseOwnership") && !strings.Contains(dispatchFn, "ownershipClaimer") {
		t.Fatal("Dispatch must use ownershipClaimer / OpenLeaseOwnership (not process-local map)")
	}
	if strings.Contains(dispatchFn, "LoadProvenance") || strings.Contains(dispatchFn, "WriteSidecar") || strings.Contains(dispatchFn, "apply-sidecar") {
		t.Fatal("Dispatch must not load sidecar provenance (description fence only)")
	}
	if !strings.Contains(dispatchFn, "ExtractProvenanceFromText") {
		t.Fatal("Dispatch must extract provenance from description text only")
	}
	if strings.Contains(string(mustRead(t, filepath.Join(root, "cmd/herd/deps.go"))), "apply-sidecar") {
		t.Fatal("cmd/herd migrate must not offer apply-sidecar")
	}

	// No theater: blank-identifier assignment of ValidateLaunch must not appear in cmd/herd.
	mainFiles := []string{
		filepath.Join(root, "cmd/herd/main.go"),
		filepath.Join(root, "cmd/herd/deps.go"),
	}
	for _, p := range mainFiles {
		src, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.Contains(string(src), "_ = deps.ValidateLaunch") ||
			strings.Contains(string(src), "assertDepsEntrypoint") {
			t.Fatalf("%s still contains theater gate assignment", p)
		}
	}

	// Causal: if RequireTaskLaunch is removed from Dispatch, this test fails (count).
	_ = countRTL

	// Parse dispatch for real CallExpr to RequireTaskLaunch (not comments).
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "dispatch.go", dispatchSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RequireTaskLaunch" {
			calls++
		}
		return true
	})
	if calls < 2 {
		t.Fatalf("ast: Dispatch must call RequireTaskLaunch >=2 times, got %d", calls)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func extractFuncBody(src, prefix string) string {
	i := strings.Index(src, prefix)
	if i < 0 {
		return ""
	}
	j := strings.Index(src[i:], "{")
	if j < 0 {
		return ""
	}
	start := i + j
	depth := 0
	for k := start; k < len(src); k++ {
		switch src[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : k+1]
			}
		}
	}
	return src[start:]
}
