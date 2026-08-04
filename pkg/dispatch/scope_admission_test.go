package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/scopefence"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

type testScopeAuthorityVerifier struct{}

func (testScopeAuthorityVerifier) VerifyGraph(context.Context, scopefence.AuthorityReceipt, scopefence.Graph) error {
	return nil
}
func (testScopeAuthorityVerifier) VerifyScope(context.Context, scopefence.AuthorityReceipt, scopefence.Scope) error {
	return nil
}
func (testScopeAuthorityVerifier) VerifyRelease(context.Context, scopefence.ReleaseRequest) error {
	return nil
}

func scopeDispatchConfig() *config.Config {
	return &config.Config{TaskProvider: config.TaskProvider{ProjectID: "test"}, Lanes: []config.LaneDef{{Name: "worker"}}}
}

func TestDispatchAcceptedScopePreWorktreeFailureCompensatesExactlyOnce(t *testing.T) {
	tp := &mockTaskProvider{tasks: []*provider.Task{{ID: "1", Ref: "FAC-206", Title: "scope", Status: "to-do", Description: emptyDepsFence("FAC-206", "1")}}}
	admission := &recordingScopeAdmission{decision: scopefence.Decision{Granted: true}}
	comp := &recordingCompensator{}
	d := withTestLease(t, &Dispatcher{Config: scopeDispatchConfig(), TaskProvider: tp, Worktree: &mockWorktree{err: errors.New("before create")}, Compensator: comp, Herdr: &fakeHerdr{}, ScopeFence: admission})
	if _, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-206", NoLaunch: true}); err == nil {
		t.Fatal("expected pre-worktree failure")
	}
	if admission.releases != 1 || admission.releaseReq.Authority != scopefence.CompensatedNoCandidate {
		t.Fatalf("scope compensation was not exactly one verified no-candidate release: calls=%d req=%+v", admission.releases, admission.releaseReq)
	}
}

func TestDispatchPostWorktreeAmbiguityRetainsScopeOwnership(t *testing.T) {
	root := t.TempDir()
	tp := &statusTrackingProvider{mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{{ID: "1", Ref: "FAC-207", Title: "scope", Status: "to-do", Description: emptyDepsFence("FAC-207", "1")}}}, updateErr: errors.New("board unavailable")}
	admission := &recordingScopeAdmission{decision: scopefence.Decision{Granted: true}}
	comp := &recordingCompensator{}
	d := withTestLease(t, &Dispatcher{Config: scopeDispatchConfig(), TaskProvider: tp, Worktree: &mockWorktree{root: root, info: &worktree.WorktreeInfo{Path: filepath.Join(root, "wt"), Branch: "herd/fac-207", BaseSHA: "base", AnchorRef: "anchor"}}, Compensator: comp, Herdr: &fakeHerdr{}, ScopeFence: admission})
	if _, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-207", NoLaunch: true}); err == nil {
		t.Fatal("expected board failure after worktree creation")
	}
	if admission.releases != 0 {
		t.Fatalf("ambiguous post-worktree failure released scope ownership: calls=%d", admission.releases)
	}
}

func TestProductionDispatchMissingScopeAuthorityFailsBeforeWorktree(t *testing.T) {
	root := t.TempDir()
	wm := worktree.NewWorktreeManager(root)
	tp := &mockTaskProvider{tasks: []*provider.Task{{ID: "1", Ref: "FAC-212", Title: "production scope", Status: "to-do", Description: emptyDepsFence("FAC-212", "1")}}}
	d := NewProductionDispatcher(scopeDispatchConfig(), tp, wm)
	d.Compensator = &recordingCompensator{}
	d.Ownership = &fixedGenerationOwnership{generation: 1}
	d.Worktree = &mockWorktree{root: root, err: errors.New("must not create worktree")}
	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-212", NoLaunch: true})
	if err == nil || d.Worktree.(*mockWorktree).calls != 0 {
		t.Fatalf("missing production scope authority crossed worktree boundary: err=%v calls=%d", err, d.Worktree.(*mockWorktree).calls)
	}
}

func TestProductionDispatcherNilScopeFenceFailsClosedBeforeProvider(t *testing.T) {
	d := &Dispatcher{Production: true, Config: scopeDispatchConfig(), Compensator: &recordingCompensator{}, TaskProvider: &mockTaskProvider{}, Worktree: &mockWorktree{err: errors.New("must not create")}}
	if _, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-215", NoLaunch: true}); err == nil || !strings.Contains(err.Error(), "scope fence is required") {
		t.Fatal("production dispatcher bypassed nil scope fence")
	}
}

func TestProductionConstructorDoesNotSourceAuthorityFromRepoEnvOrDatabase(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_SCOPEFENCE_KEY", "must-not-be-read")
	d := NewProductionDispatcher(scopeDispatchConfig(), &mockTaskProvider{}, worktree.NewWorktreeManager(root))
	if d.ScopeFence != nil || d.scopeFenceErr == nil || !strings.Contains(d.scopeFenceErr.Error(), "protected coordinator/root verifier") {
		t.Fatalf("production constructor installed an unprotected authority: fence=%T err=%v", d.ScopeFence, d.scopeFenceErr)
	}
	if _, err := os.Stat(filepath.Join(root, ".herd", "scopefence.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("production constructor sourced or created local authority database: err=%v", err)
	}
	if _, ok := reflect.TypeOf(*d).FieldByName("SigningKey"); ok {
		t.Fatal("dispatcher exposes signing key material")
	}
	if _, ok := reflect.TypeOf(DispatchOptions{}).FieldByName("SigningKey"); ok {
		t.Fatal("worker packet exposes signing key material")
	}
}

type trackingCloser struct {
	calls int
	err   error
}

func (c *trackingCloser) Close() error { c.calls++; return c.err }

func TestDispatcherClosePropagatesOwnedFenceCloseOnce(t *testing.T) {
	closer := &trackingCloser{err: errors.New("close failed")}
	d := &Dispatcher{scopeCloser: closer}
	if err := d.Close(); !errors.Is(err, closer.err) {
		t.Fatalf("close error not propagated: %v", err)
	}
	if err := d.Close(); !errors.Is(err, closer.err) || closer.calls != 1 {
		t.Fatalf("close was not exactly once: calls=%d err=%v", closer.calls, err)
	}
}

func TestDurableScopeAdmissionTwoOverlappingDispatchesOneWinner(t *testing.T) {
	store, err := scopefence.NewSQLiteStore(filepath.Join(t.TempDir(), "scopefence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	graph := scopefence.Graph{Revision: "graph-dispatch", Nodes: 10, Edges: 20, Files: 2, Flows: 1, Complete: true}
	if err := store.PutGraphSnapshot(context.Background(), "repo", graph); err != nil {
		t.Fatal(err)
	}
	shared := scopefence.Scope{Packages: []string{"pkg/overlap"}}
	for _, task := range []string{"FAC-216", "FAC-217"} {
		if err := store.PutScopeDeclaration(context.Background(), "repo", task, graph.Revision, shared); err != nil {
			t.Fatal(err)
		}
	}
	graphAuthority := scopefence.NewSQLiteGraphAuthority(store, "repo", graph.Revision, graph.Files)
	graphAuthority.Verifier = testScopeAuthorityVerifier{}
	scopeAuthority := scopefence.NewSQLiteScopeAuthority(store)
	scopeAuthority.Verifier = testScopeAuthorityVerifier{}
	admission := durableScopeAdmission{fence: scopefence.ResolvingFence{Fence: scopefence.Fence{Store: store, Graph: graphAuthority}, Authority: scopeAuthority}}
	results := make(chan scopefence.Decision, 2)
	var wg sync.WaitGroup
	for _, task := range []string{"FAC-216", "FAC-217"} {
		wg.Add(1)
		go func(task string) {
			defer wg.Done()
			decision, err := admission.Acquire(context.Background(), scopefence.AcquireRequest{Ownership: scopefence.Ownership{Identity: scopefence.Identity{Repository: "repo", Branch: worktree.TaskBranch(task), Task: task}, Generation: 1, State: scopefence.Active}, ExpectedGraphRevision: graph.Revision})
			if err != nil {
				t.Errorf("admission %s: %v", task, err)
			}
			results <- decision
		}(task)
	}
	wg.Wait()
	close(results)
	wins := 0
	for decision := range results {
		if decision.Granted {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("overlapping durable dispatch admissions won=%d, want exactly one", wins)
	}
}
