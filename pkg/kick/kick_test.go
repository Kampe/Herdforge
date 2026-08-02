package kick

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// overrideRoster pins the roster for a test, restoring derivation on cleanup.
func overrideRoster(t *testing.T, ids ...string) {
	t.Helper()
	SetStandingOverride(ids)
	t.Cleanup(func() { SetStandingOverride(nil) })
}

// newRepoFixture builds a temp dir tree that mirrors the repo layout for
// roster derivation tests and cd into it (restored after the test).
func newRepoFixture(t *testing.T, registryJSON, herdYAML string) string {
	t.Helper()
	dir := t.TempDir()
	if registryJSON != "" {
		p := filepath.Join(dir, "docs", "agent")
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "lane-registry.json"), []byte(registryJSON), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if herdYAML != "" {
		if err := os.MkdirAll(filepath.Join(dir, ".herd"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".herd", "herd.yaml"), []byte(herdYAML), 0644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	return dir
}

const testRegistry = `{
  "version": 1,
  "lanes": [
    {"id": "reviewer", "route_shape": "review"},
    {"id": "worker", "route_shape": "code"},
    {"id": "forge-smith", "route_shape": "planning"}
  ]
}`

const testHerdYAML = `version: "1"
project:
  name: test
  default_branch: main
task_provider:
  type: github
lanes:
  - name: alpha
    agent_kind: deepseek
    model: opencode/deepseek-v4
  - name: forge-smith
    agent_kind: deepseek
    model: opencode/deepseek-v4
`

func TestStandingIDs_FromRegistry(t *testing.T) {
	newRepoFixture(t, testRegistry, "")
	got := StandingIDs()
	want := []string{"forge-forge-smith", "forge-reviewer", "forge-worker"}
	if !equalStrings(got, want) {
		t.Fatalf("StandingIDs() = %v, want %v", got, want)
	}
}

func TestStandingIDs_HerdYAMLFallback(t *testing.T) {
	newRepoFixture(t, "", testHerdYAML)
	got := StandingIDs()
	want := []string{"forge-alpha", "forge-forge-smith"}
	if !equalStrings(got, want) {
		t.Fatalf("StandingIDs() = %v, want %v", got, want)
	}
}

func TestStandingIDs_RegistryWinsOverYAML(t *testing.T) {
	newRepoFixture(t, testRegistry, testHerdYAML)
	got := StandingIDs()
	want := []string{"forge-forge-smith", "forge-reviewer", "forge-worker"}
	if !equalStrings(got, want) {
		t.Fatalf("StandingIDs() = %v, want %v", got, want)
	}
}

func TestStandingIDs_EmptyWithoutSources(t *testing.T) {
	newRepoFixture(t, "", "")
	registryPaths = nil
	t.Cleanup(func() { registryPaths = []string{"docs/agent/lane-registry.json", ".herd/lane-registry.json"} })
	if got := StandingIDs(); got != nil && len(got) != 0 {
		t.Fatalf("StandingIDs() = %v, want empty", got)
	}
}

func TestStandingIDs_SortedUnique(t *testing.T) {
	overrideRoster(t, "forge-reviewer", "forge-worker", "forge-reviewer", "forge-worker")
	ids := StandingIDs()
	want := []string{"forge-reviewer", "forge-worker"}
	if !equalStrings(ids, want) {
		t.Fatalf("StandingIDs() = %v, want %v", ids, want)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("StandingIDs() must be sorted")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate standing id: %s", id)
		}
		seen[id] = true
	}
}

func TestSetStandingOverride_RestoresDerivation(t *testing.T) {
	newRepoFixture(t, testRegistry, "")
	SetStandingOverride([]string{"forge-worker"})
	if !equalStrings(StandingIDs(), []string{"forge-worker"}) {
		t.Fatal("override should pin the roster")
	}
	SetStandingOverride(nil)
	if !equalStrings(StandingIDs(), []string{"forge-forge-smith", "forge-reviewer", "forge-worker"}) {
		t.Fatal("nil override should restore derivation from the registry")
	}
}

func TestKickMessage_Selftest(t *testing.T) {
	newRepoFixture(t, testRegistry, "")
	err := Selftest()
	if err != nil {
		t.Fatal(err)
	}
}

func TestKickMessage_Templates(t *testing.T) {
	newRepoFixture(t, testRegistry, "")
	tests := []struct {
		id     string
		wantID bool
	}{
		{"forge-reviewer", true},
		{"forge-worker", true},
		{"forge-forge-smith", true},
		{"unknown-lane", true}, // falls back to the generic template
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			msg := KickMessage(tc.id, "")
			if msg == "" {
				t.Fatal("empty message")
			}
			if tc.wantID && !strings.Contains(msg, tc.id) {
				t.Fatalf("message for %q should contain the id", tc.id)
			}
			if !strings.Contains(msg, "Rapid turn") {
				t.Fatal("message should contain 'Rapid turn' suffix")
			}
			if !strings.Contains(msg, "STANDING KICK") {
				t.Fatal("message should contain 'STANDING KICK' prefix")
			}
		})
	}
}

func TestKickMessage_WithReason(t *testing.T) {
	msg := KickMessage("forge-worker", "main advanced; re-scan")
	if !strings.Contains(msg, "Context: main advanced; re-scan") {
		t.Fatal("message should include reason context")
	}
}

func TestKickMessage_EmptyID(t *testing.T) {
	msg := KickMessage("", "")
	if msg == "" {
		t.Fatal("message must not be empty")
	}
	if strings.Contains(msg, "\u0000") {
		t.Fatal("message must not contain NUL bytes")
	}
}

func TestLookupAgent_Found(t *testing.T) {
	agents := []AgentEntry{
		{Name: "forge-worker", Status: "idle", PaneID: "pane-1"},
		{Name: "forge-reviewer", Status: "working", PaneID: "pane-2"},
	}
	_, pane, found := LookupAgent(agents, "forge-worker")
	if !found {
		t.Fatal("should find forge-worker")
	}
	if pane != "pane-1" {
		t.Fatalf("expected pane-1, got %s", pane)
	}
}

func TestLookupAgent_NotFound(t *testing.T) {
	agents := []AgentEntry{
		{Name: "forge-worker", Status: "idle", PaneID: "pane-1"},
	}
	_, _, found := LookupAgent(agents, "missing-agent")
	if found {
		t.Fatal("should not find missing agent")
	}
}

func TestLookupAgent_LabelFallback(t *testing.T) {
	agents := []AgentEntry{
		{Label: "forge-worker", Status: "done", PaneID: "pane-1"},
	}
	_, pane, found := LookupAgent(agents, "forge-worker")
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

	reason, held := LaneHeld("forge-worker")
	if held {
		t.Fatalf("lane should not be held, got reason: %s", reason)
	}
}

func TestLaneHeld_Held(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_HOLD_DIR", dir)

	holdPath := filepath.Join(dir, "forge-worker")
	if err := os.WriteFile(holdPath, []byte("test hold reason\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reason, held := LaneHeld("forge-worker")
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

	holdPath := filepath.Join(dir, "forge-worker")
	if err := os.WriteFile(holdPath, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	reason, held := LaneHeld("forge-worker")
	if !held {
		t.Fatal("lane should be held")
	}
	if !strings.Contains(reason, "held by coordinator") {
		t.Fatalf("expected default reason, got %q", reason)
	}
}

func TestRun_DryRun(t *testing.T) {
	// Use a guaranteed-absent agent name so the dry-run path is exercised
	// deterministically regardless of the live herdr fleet.
	t.Setenv("HERD_HOLD_DIR", t.TempDir())
	newRepoFixture(t, testRegistry, "")
	result, err := Run(Options{
		Names:        []string{"forge-no-such-lane"},
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
	// The target is guaranteed absent, so the missing-agent path runs. With
	// Force=true the agent would be kicked regardless of status; assert the
	// dry-run still reports a kick and never panics.
	t.Setenv("HERD_HOLD_DIR", t.TempDir())
	result, err := Run(Options{
		Names:        []string{"forge-no-such-lane"},
		Force:        true,
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
	t.Logf("force dry-run result: kicked=%d skipped=%d failed=%d",
		result.Kicked, result.Skipped, result.Failed)
}

func TestRun_EmptyQuiet(t *testing.T) {
	newRepoFixture(t, testRegistry, "")
	t.Setenv("HERD_HOLD_DIR", t.TempDir())
	// With Quiet=false and DryRun=true, we should see output but no error.
	result, err := Run(Options{
		Names:        []string{"forge-no-such-lane"},
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
	newRepoFixture(t, testRegistry, "")
	if err := Selftest(); err != nil {
		t.Fatal(err)
	}
}

func TestSortStandingIDs(t *testing.T) {
	overrideRoster(t, "forge-reviewer", "forge-worker", "forge-forge-smith")
	ids := make([]string, len(StandingIDs()))
	copy(ids, StandingIDs())
	sort.Strings(ids)
	for i, id := range ids {
		if StandingIDs()[i] != id {
			t.Fatalf("StandingIDs()[%d] = %s, want %s", i, StandingIDs()[i], id)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
