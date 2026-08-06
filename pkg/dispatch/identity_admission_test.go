package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

func TestOwnershipClaimerRepositoryIdentityFailureFailsBeforeOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(string) (string, error)
		want error
	}{
		{name: "error", fn: func(string) (string, error) { return "", errors.New("repository authentication failed") }},
		{name: "empty", fn: func(string) (string, error) { return "", nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := authenticatedRepositoryIdentity
			authenticatedRepositoryIdentity = tc.fn
			defer func() { authenticatedRepositoryIdentity = previous }()

			d := &Dispatcher{Config: &config.Config{}, Production: true}
			if _, err := d.ownershipClaimer(); err == nil {
				t.Fatal("repository identity failure unexpectedly opened ownership")
			}
		})
	}
}

func TestLaunchRepositoryIdentityFailureHasZeroHerdrAndCompensatorEffects(t *testing.T) {
	receiptPath := filepath.Join(t.TempDir(), "receipts.jsonl")
	t.Setenv("HERD_LAUNCH_RECEIPTS", receiptPath)
	repo, wm := initDispatchRepo(t)
	d := NewProductionDispatcher(testCfg(), nil, wm)
	fh := &fakeHerdr{available: true, workspace: "workspace", tabID: "tab"}
	comp := &recordingCompensator{}
	d.Herdr = fh
	d.Compensator = comp
	previous := authenticatedRepositoryIdentity
	repositoryErr := errors.New("repository unavailable")
	authenticatedRepositoryIdentity = func(string) (string, error) { return "", repositoryErr }
	defer func() { authenticatedRepositoryIdentity = previous }()

	task := baseTask("FAC-3")
	lane := &d.Config.Lanes[0]
	result := &DispatchResult{}
	decision, err := testRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: task.Ref, LeaseGeneration: 1, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	err = d.launch(context.Background(), DispatchOptions{Decision: decision}, task, lane, &worktree.WorktreeInfo{
		Path: filepath.Join(repo, "isolated"), Branch: "herd/fac-identity", BaseSHA: "base", AnchorRef: "anchor",
	}, "herd/fac-identity", "packet", result, nil)
	if err == nil {
		t.Fatal("repository identity failure unexpectedly launched")
	}
	if !errors.Is(err, repositoryErr) {
		t.Fatalf("failure occurred before repository identity boundary: %v", err)
	}
	if fh.tabCwd != "" || fh.startCalls != 0 || len(comp.compsCopy()) != 0 {
		t.Fatalf("identity failure reached side effects: tab=%q starts=%d compensations=%v", fh.tabCwd, fh.startCalls, comp.compsCopy())
	}
	if _, err := os.Stat(receiptPath); err == nil {
		t.Fatal("repository identity failure wrote a launch receipt before admission")
	}
}
