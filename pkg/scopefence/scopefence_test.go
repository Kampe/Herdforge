package scopefence

import (
	"context"
	"sync"
	"testing"
)

type countingStore struct {
	inner Store
	cas   int
}

func (s *countingStore) Read(ctx context.Context) (Snapshot, error) { return s.inner.Read(ctx) }
func (s *countingStore) CompareAndSwap(ctx context.Context, rev string, next []Ownership) (bool, error) {
	s.cas++
	return s.inner.CompareAndSwap(ctx, rev, next)
}

func scope(pkg, file, symbol string) Scope {
	return Scope{Packages: []string{pkg}, Files: []string{file}, Symbols: []string{symbol}}
}
func req(task, branch string, gen int64, s Scope) AcquireRequest {
	return AcquireRequest{Ownership: Ownership{Identity: Identity{Repository: "repo", Branch: branch, Task: task}, Generation: gen, Scope: s, State: Active, GraphRevision: "g1", GraphFiles: 2}, Graph: Graph{Revision: "g1", Nodes: 10, Edges: 20, Files: 2, Flows: 1, Complete: true}, ExpectedGraphRevision: "g1", ExpectedGraphFiles: 2}
}
func fence(store Store) Fence {
	return Fence{Store: store, Verify: func(context.Context, ReleaseRequest) bool { return true }}
}

func TestAcquireBlocksCleanUnadmittedAndNamesOverlap(t *testing.T) {
	store := NewMemoryStore(req("FAC-182", "old", 1, scope("pkg/a", "pkg/a/a.go", "Run")).Ownership)
	storeOwners, _ := store.Read(context.Background())
	storeOwners.Owners[0].State = Clean
	store = NewMemoryStore(storeOwners.Owners[0])
	d, err := fence(store).Acquire(context.Background(), req("FAC-161", "new", 1, scope("pkg/a", "pkg/a/a.go", "Other")))
	if err != nil || d.Granted || d.Evidence.Reason != ReasonScopeOverlap || len(d.Evidence.Packages) != 1 {
		t.Fatalf("unexpected decision: %+v %v", d, err)
	}
}

func TestAllUnadmittedLifecycleStatesRetainOwnership(t *testing.T) {
	for _, state := range []State{Done, Idle, Clean, Audit, Review} {
		owner := req("FAC-182", "old", 1, scope("pkg/a", "pkg/a.go", "Run")).Ownership
		owner.State = state
		store := NewMemoryStore(owner)
		d, err := fence(store).Acquire(context.Background(), req("FAC-161", "new", 1, scope("pkg/a", "pkg/a.go", "Other")))
		if err != nil || d.Granted || d.Evidence.Reason != ReasonScopeOverlap {
			t.Fatalf("state %q was incorrectly released: %+v %v", state, d, err)
		}
	}
}

func TestContainmentAndCanonicalDeclarationOverlap(t *testing.T) {
	owner := req("FAC-183", "owner", 1, Scope{Packages: []string{"pkg/a"}, Files: []string{"pkg/a/outer.go"}, Symbols: []string{"pkg/a/outer.go::Run"}}).Ownership
	store := NewMemoryStore(owner)
	for _, candidate := range []Scope{
		{Packages: []string{"pkg/a/child"}},
		{Files: []string{"pkg/a/outer.go"}},
		{Symbols: []string{"pkg/a/outer.go::Run"}},
		{Symbols: []string{"pkg/a/child.go::Nested"}},
	} {
		d, _ := fence(store).Acquire(context.Background(), req("FAC-184", "candidate", 1, candidate))
		if d.Granted || d.Evidence.Reason != ReasonScopeOverlap {
			t.Fatalf("containment missed for %+v: %+v", candidate, d)
		}
	}
}

func TestExactIdentityIsIdempotentButChangedFenceBlocks(t *testing.T) {
	base := req("FAC-1", "same", 4, scope("pkg/a", "pkg/a.go", "pkg/a.go::Run")).Ownership
	store := &countingStore{inner: NewMemoryStore(base)}
	d, err := (Fence{Store: store}).Acquire(context.Background(), req("FAC-1", "same", 4, scope("pkg/a", "pkg/a.go", "pkg/a.go::Run")))
	if err != nil || !d.Granted || store.cas != 0 {
		t.Fatalf("idempotent acquire used CAS: %+v cas=%d", d, store.cas)
	}
	d, _ = (Fence{Store: store}).Acquire(context.Background(), req("FAC-1", "same", 5, scope("pkg/a", "pkg/a.go", "pkg/a.go::Run")))
	if d.Granted || d.Evidence.Reason != ReasonIdentityConflict {
		t.Fatalf("changed generation was accepted: %+v", d)
	}
}

func TestConflictEvidenceContainsBothOwnersAndGraphBinding(t *testing.T) {
	owner := req("FAC-2", "other", 8, scope("pkg/a", "pkg/a.go", "pkg/a.go::Run")).Ownership
	d, _ := fence(NewMemoryStore(owner)).Acquire(context.Background(), req("FAC-1", "candidate", 3, scope("pkg/a", "pkg/a.go", "pkg/a.go::Run")))
	if d.Granted || d.Evidence.Task != "FAC-1" || d.Evidence.ConflictTask != "FAC-2" || d.Evidence.ConflictBranch != "other" || d.Evidence.ConflictGeneration != 8 || d.Evidence.GraphRevision != "g1" || d.Evidence.GraphFiles != 2 || d.Evidence.ConflictRepository != "repo" {
		t.Fatalf("incomplete evidence: %+v", d.Evidence)
	}
}

func TestGraphExpectationsAndDeepCopy(t *testing.T) {
	r := req("FAC-1", "one", 1, scope("pkg/a", "pkg/a.go", "pkg/a.go::Run"))
	r.ExpectedGraphRevision = "g2"
	d, _ := fence(NewMemoryStore()).Acquire(context.Background(), r)
	if d.Granted || d.Evidence.Reason != ReasonGraphInvalid {
		t.Fatalf("stale graph accepted: %+v", d)
	}
	input := req("FAC-1", "one", 1, scope("pkg/a", "pkg/a.go", "pkg/a.go::Run")).Ownership
	store := NewMemoryStore(input)
	input.Scope.Packages[0] = "pkg/changed"
	snap, _ := store.Read(context.Background())
	snap.Owners[0].Scope.Files[0] = "pkg/changed.go"
	snap2, _ := store.Read(context.Background())
	if snap2.Owners[0].Scope.Packages[0] != "pkg/a" || snap2.Owners[0].Scope.Files[0] != "pkg/a.go" {
		t.Fatal("store exposed nested slice aliases")
	}
}

func TestDisjointAndGraphValidation(t *testing.T) {
	d, _ := fence(NewMemoryStore()).Acquire(context.Background(), req("FAC-183", "one", 1, scope("pkg/a", "pkg/a.go", "A")))
	if !d.Granted {
		t.Fatal("disjoint scope should acquire")
	}
	r := req("FAC-184", "two", 1, scope("pkg/b", "pkg/b.go", "B"))
	r.Graph.Complete = false
	d, _ = fence(NewMemoryStore()).Acquire(context.Background(), r)
	if d.Granted || d.Evidence.Reason == "" {
		t.Fatal("incomplete graph must block")
	}
	r = req("FAC-185", "three", 1, Scope{})
	d, _ = fence(NewMemoryStore()).Acquire(context.Background(), r)
	if d.Granted || d.Evidence.Reason != ReasonMissingScope {
		t.Fatalf("missing scope was not blocked: %+v", d)
	}
	r = req("FAC-186", "four", 1, scope("pkg/c", "pkg/c.go", "C"))
	r.Graph.Flows = 0
	d, _ = fence(NewMemoryStore()).Acquire(context.Background(), r)
	if d.Granted || d.Evidence.Reason == "" {
		t.Fatal("zero-flow graph must be blocked")
	}
}

func TestRaceHasExactlyOneWinner(t *testing.T) {
	store := NewMemoryStore()
	f := fence(store)
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for _, task := range []string{"FAC-1", "FAC-2"} {
		wg.Add(1)
		go func(task string) {
			defer wg.Done()
			d, _ := f.Acquire(context.Background(), req(task, task, 1, scope("pkg/a", "pkg/a.go", "A")))
			results <- d.Granted
		}(task)
	}
	wg.Wait()
	close(results)
	wins := 0
	for v := range results {
		if v {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("wins=%d", wins)
	}
}

func TestStoreRejectsStaleCAS(t *testing.T) {
	store := NewMemoryStore()
	if won, err := store.CompareAndSwap(context.Background(), "stale", nil); err != nil || won {
		t.Fatalf("stale CAS was accepted: won=%v err=%v", won, err)
	}
}

func TestReleaseIsFencedAndAuthenticated(t *testing.T) {
	store := NewMemoryStore(req("FAC-1", "one", 7, scope("pkg/a", "pkg/a.go", "A")).Ownership)
	f := Fence{Store: store, Verify: func(_ context.Context, r ReleaseRequest) bool { return r.Proof == "root-token" }}
	r := ReleaseRequest{Ownership: req("FAC-1", "one", 6, scope("pkg/a", "pkg/a.go", "A")).Ownership, Authority: RootAdmittedMerge, Proof: "root-token"}
	if f.Release(context.Background(), r) == nil {
		t.Fatal("stale generation released ownership")
	}
	r.Generation = 7
	r.Scope = scope("pkg/a", "pkg/a.go", "A")
	r.Proof = "bad"
	if f.Release(context.Background(), r) == nil {
		t.Fatal("unauthenticated release")
	}
	r.Proof = "root-token"
	if err := f.Release(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseRequiresExactCanonicalOwnershipAndGraph(t *testing.T) {
	owner := req("FAC-9", "nine", 2, scope("pkg/a", "pkg/a.go", "pkg/a.go::Run")).Ownership
	store := NewMemoryStore(owner)
	f := fence(store)
	bad := ReleaseRequest{Ownership: owner, Authority: RootAdmittedMerge, Proof: "ok"}
	bad.Scope.Files[0] = "pkg/other.go"
	if f.Release(context.Background(), bad) == nil {
		t.Fatal("release accepted different scope")
	}
	bad.Scope = owner.Scope
	bad.GraphRevision = "old"
	if f.Release(context.Background(), bad) == nil {
		t.Fatal("release accepted different graph binding")
	}
}

func TestCanceledAcquireNeverCallsCAS(t *testing.T) {
	store := &countingStore{inner: NewMemoryStore()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err := (Fence{Store: store}).Acquire(ctx, req("FAC-1", "one", 1, scope("pkg/a", "pkg/a.go", "pkg/a.go::Run")))
	if err == nil || d.Evidence.Reason != ReasonContextCanceled || store.cas != 0 {
		t.Fatalf("canceled acquire crossed CAS: %+v err=%v cas=%d", d, err, store.cas)
	}
}

func TestSortCandidates(t *testing.T) {
	got := SortCandidates([]Candidate{{Task: "FAC-9", Priority: 1, TicketNumber: 9}, {Task: "FAC-2", Priority: 2, TicketNumber: 2}, {Task: "FAC-1", Priority: 2, TicketNumber: 1, Excluded: true}})
	if len(got) != 2 || got[0].Task != "FAC-2" || got[1].Task != "FAC-9" {
		t.Fatalf("unexpected order: %+v", got)
	}
}
