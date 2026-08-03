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
// sources call ValidateLaunch / FencedClaim before worktree/claim side effects.
// This is a static reachability proof for FAC-159 acceptance criterion 8.
func TestProductionLaunchWiring_GateBeforeSideEffects(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	// pkg/deps -> repo root
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

	type hit struct {
		file string
		fn   string
	}
	var gateHits, sideHits []hit

	scan := func(rel string, wantGate bool) {
		path := filepath.Join(root, rel)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := callName(call)
			switch {
			case strings.Contains(name, "ValidateLaunch") ||
				strings.Contains(name, "ValidateClaim") ||
				strings.Contains(name, "FencedClaim") ||
				strings.Contains(name, "SelectEligibleRefs"):
				if wantGate {
					gateHits = append(gateHits, hit{rel, name})
				}
			case strings.Contains(name, "CreateTaskWorktreeFrom") ||
				strings.Contains(name, "claimTaskBound"):
				sideHits = append(sideHits, hit{rel, name})
			}
			return true
		})
	}

	// Production (non-test) launch wiring.
	scan("pkg/dispatch/dispatch.go", true)
	scan("pkg/daemon/engine.go", true)

	if len(gateHits) == 0 {
		t.Fatal("no production gate calls found in dispatch/daemon")
	}
	// Ensure dispatch.go invokes ValidateLaunch and CreateTaskWorktreeFrom
	// (gate must exist in the same production file as the worktree side effect).
	var hasGate, hasWT bool
	for _, h := range gateHits {
		if h.file == "pkg/dispatch/dispatch.go" && strings.Contains(h.fn, "ValidateLaunch") {
			hasGate = true
		}
	}
	for _, h := range sideHits {
		if h.file == "pkg/dispatch/dispatch.go" && strings.Contains(h.fn, "CreateTaskWorktreeFrom") {
			hasWT = true
		}
	}
	if !hasGate || !hasWT {
		t.Fatalf("dispatch must wire ValidateLaunch + CreateTaskWorktreeFrom in production (gate=%v wt=%v) gates=%v sides=%v",
			hasGate, hasWT, gateHits, sideHits)
	}

	// Order proof inside Dispatch(): ValidateLaunch before CreateTaskWorktreeFrom.
	dispatchSrc, _ := os.ReadFile(filepath.Join(root, "pkg/dispatch/dispatch.go"))
	dispatchFn := extractFuncBody(string(dispatchSrc), "func (d *Dispatcher) Dispatch")
	if dispatchFn == "" {
		t.Fatal("Dispatcher.Dispatch not found")
	}
	gateIdx := strings.Index(dispatchFn, "ValidateLaunch")
	wtIdx := strings.Index(dispatchFn, "CreateTaskWorktreeFrom")
	if gateIdx < 0 || wtIdx < 0 || gateIdx > wtIdx {
		t.Fatalf("ValidateLaunch must appear before CreateTaskWorktreeFrom in Dispatch (gate=%d wt=%d)", gateIdx, wtIdx)
	}

	// Pulse fenced claim before claimTaskBound inside RunPulse.
	engineSrc, _ := os.ReadFile(filepath.Join(root, "pkg/daemon/engine.go"))
	runPulse := extractFuncBody(string(engineSrc), "func (e *Engine) RunPulse")
	if runPulse == "" {
		t.Fatal("Engine.RunPulse not found")
	}
	fenceIdx := strings.Index(runPulse, "FencedClaim")
	// claimTaskBound is only invoked from inside FencedClaim's claimFn closure.
	if fenceIdx < 0 || !strings.Contains(runPulse, "claimTaskBound") {
		t.Fatalf("FencedClaim must wrap claimTaskBound in RunPulse (fence=%d containsClaim=%v)",
			fenceIdx, strings.Contains(runPulse, "claimTaskBound"))
	}
	// claimTaskBound must appear after FencedClaim in the function body.
	claimIdx := strings.Index(runPulse, "claimTaskBound")
	if claimIdx < fenceIdx {
		t.Fatalf("claimTaskBound must be inside FencedClaim after the call site (fence=%d claim=%d)", fenceIdx, claimIdx)
	}
}

// extractFuncBody returns the source of the first function starting with prefix
// through a naive brace match (good enough for our production files).
func extractFuncBody(src, prefix string) string {
	i := strings.Index(src, prefix)
	if i < 0 {
		return ""
	}
	// Find opening brace of function.
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

func callName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	default:
		return ""
	}
}
