package scopefence

import (
	"context"
	"sync"
	"testing"
)

func scope(pkg, file, symbol string) Scope {
	return Scope{Packages: []string{pkg}, Files: []string{file}, Symbols: []string{symbol}}
}
func req(task, branch string, gen int64, s Scope) AcquireRequest {
	return AcquireRequest{Ownership: Ownership{Identity: Identity{Repository: "repo", Branch: branch, Task: task}, Generation: gen, Scope: s, State: Active}, Graph: Graph{Revision: "g1", Nodes: 10, Edges: 20, Files: 2, Flows: 1, Complete: true}}
}
func fence(store Store) Fence {
	return Fence{Store: store, Verify: func(context.Context, ReleaseRequest) bool { return true }}
}

func TestAcquireBlocksCleanUnadmittedAndNamesOverlap(t *testing.T) {
	store := NewMemoryStore(Ownership{Identity: Identity{Repository: "repo", Branch: "old", Task: "FAC-182"}, Generation: 1, Scope: scope("pkg/a", "pkg/a/a.go", "Run"), State: Clean})
	d, err := fence(store).Acquire(context.Background(), req("FAC-161", "new", 1, scope("pkg/a", "pkg/a/a.go", "Other")))
	if err != nil || d.Granted || d.Evidence.Reason != "scope overlap" || len(d.Evidence.Packages) != 1 {
		t.Fatalf("unexpected decision: %+v %v", d, err)
	}
}

func TestAllUnadmittedLifecycleStatesRetainOwnership(t *testing.T) {
	for _, state := range []State{Done, Idle, Clean, Audit, Review} {
		store := NewMemoryStore(Ownership{Identity: Identity{Repository: "repo", Branch: "old", Task: "FAC-182"}, Generation: 1, Scope: scope("pkg/a", "pkg/a.go", "Run"), State: state})
		d, err := fence(store).Acquire(context.Background(), req("FAC-161", "new", 1, scope("pkg/a", "pkg/a.go", "Other")))
		if err != nil || d.Granted || d.Evidence.Reason != "scope overlap" {
			t.Fatalf("state %q was incorrectly released: %+v %v", state, d, err)
		}
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
	if d.Granted || d.Evidence.Reason != "missing scope" {
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

func TestSortCandidates(t *testing.T) {
	got := SortCandidates([]Candidate{{Task: "FAC-9", Priority: 1, TicketNumber: 9}, {Task: "FAC-2", Priority: 2, TicketNumber: 2}, {Task: "FAC-1", Priority: 2, TicketNumber: 1, Excluded: true}})
	if len(got) != 2 || got[0].Task != "FAC-2" || got[1].Task != "FAC-9" {
		t.Fatalf("unexpected order: %+v", got)
	}
}
