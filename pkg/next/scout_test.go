package next

import (
	"context"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/workbroker"
)

func scoutTP(t *testing.T, tasks ...*provider.Task) provider.TaskProvider {
	t.Helper()
	mp := provider.NewMemoryProvider()
	for _, tk := range tasks {
		tk.ProjectID = "p1"
		mp.AddTask(tk)
	}
	return mp
}

func TestScoutQueue_RanksByPriorityThenRef(t *testing.T) {
	tp := scoutTP(t,
		&provider.Task{ID: "1", Ref: "FAC-9", Status: "to-do", Priority: provider.PriorityMedium},
		&provider.Task{ID: "2", Ref: "FAC-3", Status: "to-do", Priority: provider.PriorityUrgent},
		&provider.Task{ID: "3", Ref: "FAC-10", Status: "to-do", Priority: provider.PriorityUrgent},
	)
	claimable, _, err := ScoutQueue(context.Background(), tp, "p1", nil, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	// urgent FAC-3, urgent FAC-10 (numeric > 3), then medium FAC-9
	got := []string{claimable[0].Ref, claimable[1].Ref, claimable[2].Ref}
	want := []string{"FAC-3", "FAC-10", "FAC-9"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank[%d] = %s, want %s (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestScoutQueue_BlockedCardsHeldBack(t *testing.T) {
	tp := scoutTP(t,
		&provider.Task{ID: "1", Ref: "FAC-63", Status: "to-do", Priority: provider.PriorityUrgent},
		&provider.Task{ID: "2", Ref: "FAC-64", Status: "to-do", Priority: provider.PriorityLow},
	)
	// FAC-63 is blocked by FAC-87 which is still open.
	blockers := Blockers{"FAC-63": {"FAC-87"}}
	openRefs := map[string]bool{"FAC-87": true}
	claimable, blocked, err := ScoutQueue(context.Background(), tp, "p1", blockers, openRefs)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimable) != 1 || claimable[0].Ref != "FAC-64" {
		t.Fatalf("only FAC-64 claimable, got %v", claimable)
	}
	if len(blocked) != 1 || blocked[0].Ref != "FAC-63" || blocked[0].BlockedBy[0] != "FAC-87" {
		t.Fatalf("FAC-63 must be blocked by FAC-87, got %v", blocked)
	}
}

func TestScoutBrokerSnapshotWaitsOnOpenBlocker(t *testing.T) {
	claimable, blocked, err := ScoutQueue(context.Background(), scoutTP(t,
		&provider.Task{ID: "1", Ref: "FAC-63", Status: "to-do", Priority: provider.PriorityUrgent},
		&provider.Task{ID: "2", Ref: "FAC-64", Status: "to-do", Priority: provider.PriorityLow},
	), "p1", Blockers{"FAC-63": {"FAC-87"}}, map[string]bool{"FAC-87": true})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := workbroker.DecideBroker(ScoutBrokerSnapshot(claimable, blocked, 9, 3, "work"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Admission != workbroker.AdmissionAdmitBuilder || rec.TaskRef != "FAC-64" {
		t.Fatalf("independent ready builder must win a full review slot, got %+v", rec)
	}
}

func TestScoutQueue_ResolvedBlockerBecomesClaimable(t *testing.T) {
	tp := scoutTP(t,
		&provider.Task{ID: "1", Ref: "FAC-63", Status: "to-do", Priority: provider.PriorityUrgent},
	)
	blockers := Blockers{"FAC-63": {"FAC-87"}}
	// FAC-87 is DONE (not in openRefs) → FAC-63 is now claimable.
	claimable, blocked, err := ScoutQueue(context.Background(), tp, "p1", blockers, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimable) != 1 || claimable[0].Ref != "FAC-63" {
		t.Fatalf("resolved blocker must free FAC-63, got claimable=%v blocked=%v", claimable, blocked)
	}
}
