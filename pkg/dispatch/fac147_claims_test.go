package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// TestUpdateStatusFenced_UsesLaneRoleNotLaneName is the non-vacuous guard for
// FAC-147 production wiring: ClaimStack board writes resolve ownership via
// TaskOwnershipRole(preferred). Lane names (scout/smith) are not known
// implementation roles; lane.Role (forge-smith/worker) is. Passing laneName
// aborts dispatch after the worktree exists — this test proves the preferred
// argument must be the role, not the name.
func TestUpdateStatusFenced_UsesLaneRoleNotLaneName(t *testing.T) {
	ctx := context.Background()
	mp := provider.NewMemoryProvider()
	now := time.Now().UTC()
	// Unlabeled: preferred must itself be a known implementation role.
	task := &provider.Task{
		ID: "t1", Ref: "FAC-147-role", Title: "x", Status: "to-do",
		ProjectID: "test", UpdatedAt: now, CreatedAt: now,
	}
	mp.AddTask(task)

	stack, err := provider.OpenClaimStack(t.TempDir(), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()

	d := NewDispatcher(testCfg(), mp, nil)
	d.Claims = stack

	// Lane name "smith" is not a known ownership role → fail closed.
	if err := d.updateStatusFenced(ctx, task, "smith", "in-progress"); err == nil {
		t.Fatal("preferred lane name 'smith' must fail TaskOwnershipRole on unlabeled task")
	} else if !strings.Contains(err.Error(), "known preferred ownership role") &&
		!strings.Contains(err.Error(), "no recognized implementation role") {
		t.Fatalf("unexpected error for lane name: %v", err)
	}

	// Lane role "worker" is known → mutation proceeds under live lease.
	if err := d.updateStatusFenced(ctx, task, "worker", "in-progress"); err != nil {
		t.Fatalf("preferred lane role 'worker' must succeed: %v", err)
	}
	got, err := mp.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if provider.NormalizeStatus(got.Status) != provider.StatusInProgress {
		t.Fatalf("status=%s want in-progress", got.Status)
	}
}

// TestUpdateStatusFenced_ScoutLabelNeedsForgeSmithRole proves a card labeled
// only with a lane name cannot use that name as preferred ownership — the
// durable role (forge-smith) must be supplied, matching herd.yaml scout lanes.
func TestUpdateStatusFenced_ScoutLabelNeedsForgeSmithRole(t *testing.T) {
	ctx := context.Background()
	mp := provider.NewMemoryProvider()
	now := time.Now().UTC()
	task := &provider.Task{
		ID: "t2", Ref: "FAC-147-scout", Title: "x", Status: "to-do",
		ProjectID: "test", Labels: []string{"scout"}, UpdatedAt: now, CreatedAt: now,
	}
	mp.AddTask(task)

	stack, err := provider.OpenClaimStack(t.TempDir(), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()

	d := NewDispatcher(testCfg(), mp, nil)
	d.Claims = stack

	if err := d.updateStatusFenced(ctx, task, "scout", "in-progress"); err == nil {
		t.Fatal("preferred 'scout' on scout-only labels must refuse")
	}

	// forge-smith is known preferred; unlabeled-style fallthrough still fails
	// when the only labels are unknown — re-seed with forge-smith label.
	task.Labels = []string{"forge-smith"}
	if err := d.updateStatusFenced(ctx, task, "forge-smith", "in-progress"); err != nil {
		t.Fatalf("forge-smith role on forge-smith card: %v", err)
	}
}
