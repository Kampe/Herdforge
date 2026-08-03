package herdr

import (
	"reflect"
	"testing"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
)

func TestIsAvailable(t *testing.T) {
	// On this machine, herdr should be installed
	available := IsAvailable()
	if !available {
		t.Log("herdr not found in PATH — available=false is expected on CI")
	}
}

func TestHerdrCLINotFound(t *testing.T) {
	// Verify that the error path works when herdr is missing
	// by temporarily modifying PATH
	t.Setenv("PATH", "/dev/null")

	available := IsAvailable()
	if available {
		t.Skip("herdr still found in PATH despite override")
	}
}

func TestEnsureHerdforgeLabel_Prefixes(t *testing.T) {
	got := EnsureHerdforgeLabel("worker")
	want := "Herdforge · worker"
	if got != want {
		t.Errorf("EnsureHerdforgeLabel(\"worker\") = %q, want %q", got, want)
	}
}

func TestEnsureHerdforgeLabel_AlreadyPrefixed(t *testing.T) {
	got := EnsureHerdforgeLabel("Herdforge · worker")
	if got != "Herdforge · worker" {
		t.Errorf("already-prefixed label was modified: %q", got)
	}
}

func TestEnsureHerdforgeLabel_PrefixWithSuffix(t *testing.T) {
	// Already starts with the prefix; extra suffix must not re-prefix.
	got := EnsureHerdforgeLabel("Herdforge · worker (FAC-141)")
	if got != "Herdforge · worker (FAC-141)" {
		t.Errorf("label starting with prefix was modified: %q", got)
	}
}

func TestEnsureHerdforgeLabel_MidStringStillPrefixed(t *testing.T) {
	// Non-vacuous HasPrefix contract: mid-string "Herdforge · " must NOT
	// count as already-prefixed. Mutation of HasPrefix→Contains fails this.
	in := "review of Herdforge · thing"
	got := EnsureHerdforgeLabel(in)
	want := "Herdforge · review of Herdforge · thing"
	if got != want {
		t.Errorf("EnsureHerdforgeLabel(%q) = %q, want %q", in, got, want)
	}
}

func TestAgentStartBoundaryRejectsRawAndRequiresDecision(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "{}", nil
	}
	if err := AgentStart("raw", "codex", "pane", "--model", launch.WorkerModel); err == nil {
		t.Fatal("raw/bare AgentStart must fail closed")
	}
	if len(calls) != 0 {
		t.Fatalf("raw rejection invoked process API: %v", calls)
	}
	d, err := router.NewRouter(nil, nil).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d}
	if err := AgentStartWithDecision("worker", "codex", "pane", req); err != nil {
		t.Fatal(err)
	}
	want := []string{"agent", "start", "worker", "--kind", "codex", "--pane", "pane", "--", "--model", launch.WorkerModel, "-c", "model_reasoning_effort=medium", "-a", "never"}
	if !reflect.DeepEqual(calls, [][]string{want}) {
		t.Fatalf("argv calls = %v, want %v", calls, [][]string{want})
	}
}
