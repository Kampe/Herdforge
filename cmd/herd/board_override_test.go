package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// overrideFixture builds a repo with NO canonical launch receipt and a board
// carrying one merged card. This is the exact reported shape: work landed
// outside the fleet integration path, so no receipt exists.
func overrideFixture(t *testing.T) (*config.Config, *provider.MemoryProvider, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Project.Name = "chainseer"
	cfg.TaskProvider.ProjectID = "proj"

	mp := provider.NewMemoryProvider()
	if _, err := mp.CreateTask(context.Background(), &provider.Task{
		ID: "task-2165", Ref: "CHA-2165", Status: provider.StatusInReview,
		ProjectID: "proj", Title: "externally merged work",
		// A real card declares its acceptance contract. The override replaces
		// proof of INTEGRATION, never proof that the work is what was asked, so
		// this block is still required -- by explicit design in DoneRequest.
		Description: "## Outcome\nland it\n\n```herd-acceptance-v1\n" +
			`{"commands":[{"command":"pnpm test --filter adapters","context":"packages/adapters"}]}` +
			"\n```\n",
	}); err != nil {
		t.Fatal(err)
	}
	return cfg, mp, root
}

// TestOverrideClosesCardWithoutLaunchReceipt is the FAC-563 regression.
//
// The documented override flags could never work: approveOne called
// boundBoardProvider first, which requires a launch receipt, so the override
// failed with "no usable launch receipt" before authorization was reached --
// unusable for precisely the pre-receipt and legacy cards it exists to close.
func TestOverrideClosesCardWithoutLaunchReceipt(t *testing.T) {
	cfg, mp, root := overrideFixture(t)

	res, err := approveByOverrideWithAcceptance(context.Background(), cfg, mp, root, "CHA-2165",
		"$ pnpm test --filter adapters\n  42 passed in packages/adapters\n",
		&hsync.OverrideRequest{
			Policy:   "operator-external-merge",
			Actor:    "coordinator",
			Reason:   "PR #3083 rebase merged and activated outside the fleet path",
			Evidence: "cross-family PASS ingested; shared-main rebuild and live readiness proof",
		})
	if err != nil {
		t.Fatalf("an attributable override must not require the receipt it replaces: %v", err)
	}
	if res == nil {
		t.Fatal("override close must return a result")
	}

	// The attributable record must exist, naming policy, actor, reason, evidence.
	log, err := hsync.ReadDoneLog(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) == 0 {
		t.Fatal("override close must write an attributable done-log record")
	}
	rec := log[len(log)-1]
	blob := strings.ToLower(rec.Ref + " " + describeOverride(rec))
	for _, want := range []string{"cha-2165", "operator-external-merge", "coordinator"} {
		if !strings.Contains(blob, want) {
			t.Fatalf("done-log record must carry %q, got %+v", want, rec)
		}
	}
}

// describeOverride flattens the override attribution for assertion.
func describeOverride(rec hsync.DoneRecord) string {
	if rec.Override == nil {
		return ""
	}
	return rec.Override.Policy + " " + rec.Override.Actor + " " +
		rec.Override.Reason + " " + rec.Override.Evidence
}

// Authorization must remain fail-closed: the override route is not a bypass of
// policy, only of the launch receipt.
func TestOverrideRouteStaysFailClosed(t *testing.T) {
	cfg, mp, root := overrideFixture(t)

	for name, req := range map[string]*hsync.OverrideRequest{
		"unknown policy": {Policy: "make-it-green", Actor: "a", Reason: "r", Evidence: "e"},
		"missing actor":  {Policy: "operator-external-merge", Reason: "r", Evidence: "e"},
		"missing reason": {Policy: "operator-external-merge", Actor: "a", Evidence: "e"},
		"no evidence":    {Policy: "operator-external-merge", Actor: "a", Reason: "r"},
	} {
		if _, err := approveByOverride(context.Background(), cfg, mp, root, "CHA-2165", req); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	if _, err := approveByOverride(context.Background(), cfg, mp, root, "CHA-2165", nil); err == nil {
		t.Fatal("a nil override must be refused")
	}
}
