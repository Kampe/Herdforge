package dispatch

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolprobe"
)

// Production write-capable launch without a tool-probe PASS must fail before
// TabCreateForTask (FAC-139).
func TestLaunchBoundary_ProductionMissingProbeFailsBeforeTab(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(t.TempDir(), "receipts.jsonl"))
	d, err := testRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel,
		RequestedEffort: testWorkerEffort, TaskRef: "FAC-139", LeaseGeneration: 1,
		Scope:        router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	opener := countingOpener{inner: dispatchTabOpener{h: &fakeHerdr{}}, n: &opened}
	_, _, _, err = launch.Open(opener, launch.BoundarySpec{
		Decision: d,
		Request: launch.Request{
			Decision: d, TaskRef: "FAC-139", LeaseGeneration: 1, Scope: router.ScopeTask,
			Repository: "repo", Lane: "worker",
		},
		Probe:     nil,
		Workspace: "w1",
		Label:     "task-fac-139",
		Cwd:       t.TempDir(),
		NoFocus:   true,
	})
	if err == nil {
		t.Fatal("missing probe must fail")
	}
	if opened != 0 {
		t.Fatalf("TabCreate must not run; opened=%d", opened)
	}
	if !strings.Contains(err.Error(), "tool-probe") && !strings.Contains(err.Error(), "probe") {
		t.Fatalf("error should mention probe: %v", err)
	}
}

func TestLaunchBoundary_PlanArgvMatchesDecision(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(t.TempDir(), "receipts.jsonl"))
	now := time.Unix(1_800_000_000, 0).UTC()
	d, err := testRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel,
		RequestedEffort: testWorkerEffort, TaskRef: "FAC-139", LeaseGeneration: 1,
		Scope:        router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := toolprobe.IdentityFromDecision(d)
	if err != nil {
		t.Fatal(err)
	}
	r, err := toolprobe.NewReceipt(id, toolprobe.StatusPASS, "", "sha256:ok", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHerdr{}
	plan, tabID, paneID, err := launch.Open(dispatchTabOpener{h: h}, launch.BoundarySpec{
		Decision: d,
		Request: launch.Request{
			Decision: d, TaskRef: "FAC-139", LeaseGeneration: 1, Scope: router.ScopeTask,
			Repository: "repo", Lane: "worker",
		},
		Probe:     &r,
		Workspace: "w1",
		Label:     "task-fac-139",
		Cwd:       t.TempDir(),
		Env:       []string{"PATH=/wrap"},
		NoFocus:   true,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if tabID == "" || paneID == "" {
		t.Fatal("expected tab/pane ids")
	}
	if plan.Model != d.Model || plan.Effort != d.Effort {
		t.Fatalf("plan model/effort %s/%s want %s/%s", plan.Model, plan.Effort, d.Model, d.Effort)
	}
	if strings.Join(plan.Argv, "\x00") != strings.Join(d.Argv, "\x00") {
		t.Fatalf("plan argv diverged from LaunchDecision")
	}
	if plan.CallbackContract == "" {
		t.Fatal("callback contract required")
	}
}

func TestLaunchBoundary_IncapableProbeNeverDispatched(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(t.TempDir(), "receipts.jsonl"))
	now := time.Unix(1_800_000_000, 0).UTC()
	d, err := testRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel,
		RequestedEffort: testWorkerEffort, TaskRef: "FAC-139", LeaseGeneration: 1,
		Scope:        router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := toolprobe.IdentityFromDecision(d)
	if err != nil {
		t.Fatal(err)
	}
	r, err := toolprobe.NewReceipt(id, toolprobe.StatusINCAPABLE, "no file", "", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHerdr{}
	opened := 0
	// Count TabCreate via wrapper
	opener := dispatchTabOpener{h: h}
	// monkey via direct Open with incapable
	_, _, _, err = launch.Open(countingOpener{inner: opener, n: &opened}, launch.BoundarySpec{
		Decision: d,
		Request: launch.Request{
			Decision: d, TaskRef: "FAC-139", LeaseGeneration: 1, Scope: router.ScopeTask,
			Repository: "repo", Lane: "worker",
		},
		Probe: &r, Workspace: "w1", Label: "x", Cwd: t.TempDir(), NoFocus: true,
		Now: func() time.Time { return now },
	})
	if err == nil {
		t.Fatal("INCAPABLE must block")
	}
	if opened != 0 {
		t.Fatal("tab must not open for INCAPABLE")
	}
}

type countingOpener struct {
	inner launch.TabOpener
	n     *int
}

func (c countingOpener) OpenTab(workspace, label, cwd string, noFocus bool, env ...string) (string, string, error) {
	*c.n++
	return c.inner.OpenTab(workspace, label, cwd, noFocus, env...)
}
