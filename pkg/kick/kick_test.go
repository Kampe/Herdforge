package kick

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestStandingIDs(t *testing.T) {
	if len(StandingIDs) == 0 {
		t.Fatal("StandingIDs must not be empty")
	}
	// Verify no duplicates.
	seen := make(map[string]bool)
	for _, id := range StandingIDs {
		if seen[id] {
			t.Fatalf("duplicate standing id: %s", id)
		}
		seen[id] = true
	}
	// Verify sorted.
	sorted := sort.StringsAreSorted(StandingIDs)
	if !sorted {
		t.Fatal("StandingIDs must be sorted")
	}
}

func TestKickMessage_Selftest(t *testing.T) {
	err := Selftest()
	if err != nil {
		t.Fatal(err)
	}
}

func TestKickMessage_Templates(t *testing.T) {
	tests := []struct {
		id     string
		wantID bool
	}{
		{"scout-planner", true},
		{"ux-comber", true},
		{"docs-custodian", true},
		{"platform-ops", true},
		{"security-sentinel", true},
		{"defi-crusader", true},
		{"herd-smith", true},
		{"api-crusader", true},
		{"chain-indexer", true},
		{"nft-data-engineer", true},
		{"qa-sentinel", true},
		{"perf-cost-guard", true},
		{"review-harvest-supervisor", true},
		{"unknown-lane", true}, // uses default template
		{"", true},
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			msg := KickMessage(tc.id, "")
			if msg == "" {
				t.Fatal("empty message")
			}
			if tc.wantID && tc.id != "" && !strings.Contains(msg, tc.id) {
				t.Fatalf("message for %q should contain the id", tc.id)
			}
			if !strings.Contains(msg, "Rapid turn") {
				t.Fatal("message should contain 'Rapid turn' suffix")
			}
		})
	}
}

func TestKickMessage_WithReason(t *testing.T) {
	msg := KickMessage("scout-planner", "main advanced; re-scan")
	if !strings.Contains(msg, "Context: main advanced; re-scan") {
		t.Fatal("message should include reason context")
	}
}

func TestKickMessage_EmptyID(t *testing.T) {
	msg := KickMessage("", "")
	if !strings.Contains(msg, "STANDING KICK ()") {
		t.Fatal("default template should work with empty id")
	}
}

func TestLookupAgent_Found(t *testing.T) {
	agents := []AgentEntry{
		{Name: "scout-planner", Status: "idle", PaneID: "pane-1"},
		{Name: "ux-comber", Status: "working", PaneID: "pane-2"},
	}
	_, pane, found := LookupAgent(agents, "scout-planner")
	if !found {
		t.Fatal("should find scout-planner")
	}
	if pane != "pane-1" {
		t.Fatalf("expected pane-1, got %s", pane)
	}
}

func TestLookupAgent_NotFound(t *testing.T) {
	agents := []AgentEntry{
		{Name: "scout-planner", Status: "idle", PaneID: "pane-1"},
	}
	_, _, found := LookupAgent(agents, "missing-agent")
	if found {
		t.Fatal("should not find missing agent")
	}
}

func TestLookupAgent_LabelFallback(t *testing.T) {
	agents := []AgentEntry{
		{Label: "scout-planner", Status: "done", PaneID: "pane-1"},
	}
	_, pane, found := LookupAgent(agents, "scout-planner")
	if !found {
		t.Fatal("should find by label when name is empty")
	}
	if pane != "pane-1" {
		t.Fatalf("expected pane-1, got %s", pane)
	}
}

func TestLaneHeld_NotHeld(t *testing.T) {
	// Use a temp dir that definitely has no hold file.
	dir := t.TempDir()
	t.Setenv("HERD_HOLD_DIR", dir)

	reason, held := LaneHeld("test-lane")
	if held {
		t.Fatalf("lane should not be held, got reason: %s", reason)
	}
}

func TestLaneHeld_Held(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_HOLD_DIR", dir)

	holdPath := filepath.Join(dir, "test-lane")
	if err := os.WriteFile(holdPath, []byte("test hold reason\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reason, held := LaneHeld("test-lane")
	if !held {
		t.Fatal("lane should be held")
	}
	if reason != "test hold reason" {
		t.Fatalf("expected 'test hold reason', got %q", reason)
	}
}

func TestLaneHeld_EmptyReason(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_HOLD_DIR", dir)

	holdPath := filepath.Join(dir, "test-lane")
	if err := os.WriteFile(holdPath, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	reason, held := LaneHeld("test-lane")
	if !held {
		t.Fatal("lane should be held")
	}
	if !strings.Contains(reason, "held by coordinator") {
		t.Fatalf("expected default reason, got %q", reason)
	}
}

func TestRun_DryRun(t *testing.T) {
	result, err := Run(Options{
		Names:        []string{"scout-planner"},
		DryRun:       true,
		Quiet:        true,
		RaiseMissing: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Kicked != 1 {
		t.Fatalf("expected 1 kicked (dry), got %d", result.Kicked)
	}
	if result.Skipped != 0 {
		t.Fatalf("expected 0 skipped, got %d", result.Skipped)
	}
	if result.Failed != 0 {
		t.Fatalf("expected 0 failed, got %d", result.Failed)
	}
	if len(result.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result.Entries))
	}
	if result.Entries[0].Result != "dry-run" {
		t.Fatalf("expected dry-run result, got %s", result.Entries[0].Result)
	}
}

func TestRun_ForceOverridesStatus(t *testing.T) {
	result, err := Run(Options{
		Names:        []string{"scout-planner"},
		Force:        true,
		DryRun:       true,
		Quiet:        true,
		RaiseMissing: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// With --all (Force=true), we still kick regardless of status.
	// Since no agents are actually live, it'll try to raise and fail.
	// But with RaiseMissing=false and DryRun=true, the agent won't
	// be found (no actual herdr), so it should fail.
	// This is testing that the code path doesn't panic.
	if result == nil {
		t.Fatal("result should not be nil")
	}
	t.Logf("force dry-run result: kicked=%d skipped=%d failed=%d",
		result.Kicked, result.Skipped, result.Failed)
}

func TestRun_EmptyQuiet(t *testing.T) {
	// With Quiet=false and DryRun=true, we should see output
	// but no error. The agent won't be found, so it should fail.
	result, err := Run(Options{
		Names:        []string{"scout-planner"},
		DryRun:       true,
		Quiet:        false,
		RaiseMissing: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("result should not be nil")
	}
}

func TestSelftest_AllLanes(t *testing.T) {
	err := Selftest()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSortStandingIDs(t *testing.T) {
	// Verify that StandingIDs is sorted (important for deterministic behavior).
	ids := make([]string, len(StandingIDs))
	copy(ids, StandingIDs)
	sort.Strings(ids)
	for i, id := range ids {
		if StandingIDs[i] != id {
			t.Fatalf("StandingIDs[%d] = %s, want %s", i, StandingIDs[i], id)
		}
	}
}
