package kick

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/broadcast"
	"github.com/Kampe/Herdforge/pkg/goalguard"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
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

// testRegistry declares a mix of standing (control-plane) and ephemeral
// (task) lanes so StandingIDs() tests exercise real filtering: only
// "forge-smith" and "worker" are standing here; "reviewer" is an ephemeral
// task role and must NOT be raised by herd standing.
const testRegistry = `{
  "version": 1,
  "lanes": [
    {"id": "reviewer", "route_shape": "review", "standing": false},
    {"id": "worker", "route_shape": "code", "standing": true},
    {"id": "forge-smith", "route_shape": "planning", "standing": true}
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
    prompt: ".herd/prompts/worker.md"
    standing: true
  - name: forge-smith
    agent_kind: deepseek
    model: opencode/deepseek-v4
    prompt: ".herd/prompts/smith.md"
    standing: false
`

func TestStandingIDs_FromRegistry(t *testing.T) {
	newRepoFixture(t, testRegistry, "")
	got := StandingIDs()
	want := []string{"forge-forge-smith", "forge-worker"}
	if !equalStrings(got, want) {
		t.Fatalf("StandingIDs() = %v, want %v", got, want)
	}
}

func TestStandingIDs_HerdYAMLFallback(t *testing.T) {
	newRepoFixture(t, "", testHerdYAML)
	got := StandingIDs()
	want := []string{"forge-alpha"}
	if !equalStrings(got, want) {
		t.Fatalf("StandingIDs() = %v, want %v", got, want)
	}
}

func TestStandingIDs_RegistryWinsOverYAML(t *testing.T) {
	newRepoFixture(t, testRegistry, testHerdYAML)
	got := StandingIDs()
	want := []string{"forge-forge-smith", "forge-worker"}
	if !equalStrings(got, want) {
		t.Fatalf("StandingIDs() = %v, want %v", got, want)
	}
}

// TestStandingIDs_ExcludesEphemeralLanes proves the ephemeral task lane in
// testRegistry ("reviewer", standing:false) is a ephemeral role that never
// gets raised, not merely absent by coincidence: LaneIDs() proves it is a
// declared lane, while StandingIDs() must still omit it.
func TestStandingIDs_ExcludesEphemeralLanes(t *testing.T) {
	newRepoFixture(t, testRegistry, "")
	if lanes := LaneIDs(); !equalStrings(lanes, []string{"forge-smith", "reviewer", "worker"}) {
		t.Fatalf("LaneIDs() = %v, want all 3 declared lanes", lanes)
	}
	standing := StandingIDs()
	for _, id := range standing {
		if id == "forge-reviewer" {
			t.Fatalf("StandingIDs() = %v must not raise the ephemeral reviewer lane", standing)
		}
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
	if !equalStrings(StandingIDs(), []string{"forge-forge-smith", "forge-worker"}) {
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

func TestRun_DryRun(t *testing.T) {
	// Use a guaranteed-absent agent name so the dry-run path is exercised
	// deterministically regardless of the live herdr fleet.
	newRepoFixture(t, testRegistry, "")
	result, err := Run(Options{
		Names:        []string{"forge-no-such-lane"},
		DryRun:       true,
		Quiet:        true,
		RaiseMissing: false,
		HoldReader:   allowAllHolds{}, Identity: testIdentity,
		ActiveTasks: testActiveTasks,
		Generation:  testGeneration,
		FetchAgents: emptyAgentList,
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

func TestRenderKickEnvelopeContainsGrant(t *testing.T) {
	text := renderKickEnvelope(goalguard.AuthorityEnvelope{
		Grantor: "coordinator", PacketPath: ".herd/prompts/worker.md",
		BoundedAutonomy: "continue improving", MutationLimits: "isolated worktree",
		ForbiddenActions: []string{"push"}, StopConditions: []string{"lease loss"},
	})
	for _, marker := range []string{"AUTHORITY ENVELOPE (RESUME)", "grantor:", "packet path:", "bounded autonomy:", "mutation limits:", "forbidden actions:", "stop conditions:"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("resume envelope missing %q: %s", marker, text)
		}
	}
}

func TestRun_CadenceSuppressesSecondKick(t *testing.T) {
	now := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)
	last := map[string]time.Time{}
	opts := Options{
		Names: []string{"forge-worker"}, DryRun: true, Quiet: true, RaiseMissing: false,
		Cadence: 10 * time.Minute, LastKick: last, Now: func() time.Time { return now },
		Freeze:     func() (bool, string, error) { return false, "", nil },
		HoldReader: allowAllHolds{}, Identity: testIdentity, ActiveTasks: testActiveTasks,
		Generation: testGeneration, FetchAgents: emptyAgentList,
	}
	first, err := Run(opts)
	if err != nil || first.Kicked != 1 {
		t.Fatalf("first kick=%+v err=%v", first, err)
	}
	now = now.Add(5 * time.Minute)
	second, err := Run(opts)
	if err != nil || second.Kicked != 0 || second.Skipped != 1 {
		t.Fatalf("second kick=%+v err=%v", second, err)
	}
	if !strings.Contains(second.Entries[0].Reason, "cadence:") {
		t.Fatalf("reason=%q", second.Entries[0].Reason)
	}
}

func TestSaveLoadLastKick_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cadence.json")
	want := map[string]time.Time{
		"forge-worker": time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC),
	}
	if err := SaveLastKick(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadLastKick(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got["forge-worker"].Equal(want["forge-worker"]) {
		t.Fatalf("loaded = %v, want %v", got["forge-worker"], want["forge-worker"])
	}
}

func TestLoadLastKick_MissingFileReturnsEmptyMap(t *testing.T) {
	got, err := LoadLastKick(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("loaded = %v, want empty", got)
	}
}

// TestRun_CadenceSurvivesAcrossSeparateProcessInvocations is the FAC-427
// regression: a bare in-memory LastKick map only throttles kicks within one
// Run call. The real herd-kick CLI is invoked repeatedly as separate
// processes (by pulse/cron), so cadence must survive a save/reload cycle
// between two independent Run calls, not just two calls sharing one map.
func TestRun_CadenceSurvivesAcrossSeparateProcessInvocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cadence.json")
	now := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)

	loaded, err := LoadLastKick(path)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	first, err := Run(Options{
		Names: []string{"forge-worker"}, DryRun: true, Quiet: true, RaiseMissing: false,
		Cadence: 10 * time.Minute, LastKick: loaded, Now: func() time.Time { return now },
		Freeze:     func() (bool, string, error) { return false, "", nil },
		HoldReader: allowAllHolds{}, Identity: testIdentity, ActiveTasks: testActiveTasks,
		Generation: testGeneration, FetchAgents: emptyAgentList,
	})
	if err != nil || first.Kicked != 1 {
		t.Fatalf("first kick=%+v err=%v", first, err)
	}
	if err := SaveLastKick(path, loaded); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Simulate a brand-new `herd kick` process 5 minutes later: fresh Go
	// map, reloaded from the durable state file rather than shared in
	// memory with the first Run call.
	reloaded, err := LoadLastKick(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	now = now.Add(5 * time.Minute)
	second, err := Run(Options{
		Names: []string{"forge-worker"}, DryRun: true, Quiet: true, RaiseMissing: false,
		Cadence: 10 * time.Minute, LastKick: reloaded, Now: func() time.Time { return now },
		Freeze:     func() (bool, string, error) { return false, "", nil },
		HoldReader: allowAllHolds{}, Identity: testIdentity, ActiveTasks: testActiveTasks,
		Generation: testGeneration, FetchAgents: emptyAgentList,
	})
	if err != nil || second.Kicked != 0 || second.Skipped != 1 {
		t.Fatalf("second kick=%+v err=%v, want suppressed by reloaded cadence state", second, err)
	}
}

func TestRun_FreezeSuppressesWorkButAllowsRepair(t *testing.T) {
	frozen := func() (bool, string, error) { return true, "incident-427", nil }
	base := Options{
		Names: []string{"forge-worker"}, DryRun: true, Quiet: true, RaiseMissing: false,
		HoldReader: allowAllHolds{}, Identity: testIdentity, ActiveTasks: testActiveTasks,
		Generation: testGeneration, FetchAgents: emptyAgentList, Freeze: frozen,
	}
	work, err := Run(base)
	if err != nil || work.Kicked != 0 || work.Skipped != 1 {
		t.Fatalf("work kick=%+v err=%v", work, err)
	}
	base.Repair = true
	repair, err := Run(base)
	if err != nil || repair.Kicked != 1 || repair.Skipped != 0 {
		t.Fatalf("repair kick=%+v err=%v", repair, err)
	}
}

type heldHolds struct{}

func (heldHolds) Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error) {
	return lifecycle.HoldDecision{Held: true, Generation: 1, Reason: "maintenance", Code: "operator_hold"}, nil
}

func TestRun_HeldAuthorityStopsBeforeAgentRead(t *testing.T) {
	agentListCalls := 0
	result, err := Run(Options{
		Names: []string{"forge-held", "forge-also-held"}, Quiet: true, RaiseMissing: true,
		HoldReader: heldHolds{}, Identity: testIdentity,
		ActiveTasks: testActiveTasks,
		Generation:  testGeneration,
		FetchAgents: func() ([]AgentEntry, error) {
			agentListCalls++
			return nil, nil
		},
	})
	if err != nil || result == nil || result.Skipped != 2 || len(result.Entries) != 2 || !strings.Contains(result.Entries[0].Reason, "held") || !strings.Contains(result.Entries[1].Reason, "held") {
		t.Fatalf("held kick result=%v err=%v", result, err)
	}
	if agentListCalls != 0 {
		t.Fatalf("held targets must be skipped before agent-list lookup: calls=%d", agentListCalls)
	}
}

type selectiveHolds struct{}

func (selectiveHolds) Check(_ context.Context, id lifecycle.HoldIdentity, _ int64) (lifecycle.HoldDecision, error) {
	if id.Lane == "forge-held" {
		return lifecycle.HoldDecision{Held: true, Reason: "maintenance", Code: "operator_hold"}, nil
	}
	return lifecycle.HoldDecision{}, nil
}

func TestRun_HeldLaneDoesNotFreezeUnheldLane(t *testing.T) {
	result, err := Run(Options{Names: []string{"forge-held", "forge-free"}, DryRun: true, Quiet: true, RaiseMissing: false, HoldReader: selectiveHolds{}, Identity: testIdentity, ActiveTasks: testActiveTasks, Generation: testGeneration, FetchAgents: emptyAgentList})
	if err != nil || result == nil || result.Skipped != 1 || result.Kicked != 1 {
		t.Fatalf("selective kick result=%+v err=%v", result, err)
	}
}

func TestRunUsesInjectedAgentList(t *testing.T) {
	wantErr := errors.New("deterministic agent-list failure")
	_, err := Run(Options{
		Names:        []string{"forge-injected"},
		Quiet:        true,
		RaiseMissing: false,
		HoldReader:   allowAllHolds{},
		Identity:     testIdentity,
		ActiveTasks:  testActiveTasks,
		Generation:   testGeneration,
		FetchAgents: func() ([]AgentEntry, error) {
			return nil, wantErr
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run error=%v, want injected agent-list error", err)
	}
}

type generationFenceReader struct {
	got []struct {
		id  lifecycle.HoldIdentity
		gen int64
	}
}

func (r *generationFenceReader) Check(_ context.Context, id lifecycle.HoldIdentity, gen int64) (lifecycle.HoldDecision, error) {
	r.got = append(r.got, struct {
		id  lifecycle.HoldIdentity
		gen int64
	}{id, gen})
	return lifecycle.HoldDecision{}, nil
}
func TestRunUsesGenerationForExactLaneAndTaskIdentity(t *testing.T) {
	r := &generationFenceReader{}
	genFn := func(_ context.Context, id lifecycle.HoldIdentity) (int64, error) {
		if id.Scope == "lane" {
			return 1, nil
		}
		return 4, nil
	}
	_, runErr := Run(Options{Names: []string{"forge-fenced"}, DryRun: true, Quiet: true, HoldReader: r, Identity: func(string) (lifecycle.HoldIdentity, error) {
		return lifecycle.HoldIdentity{Repository: "repo", Owner: "role", Lane: "lane", Scope: "lane"}, nil
	}, ActiveTasks: func(context.Context, string) ([]lifecycle.HoldIdentity, error) {
		return []lifecycle.HoldIdentity{{Repository: "repo", Owner: "role", Lane: "lane", Task: "FAC-4", Scope: "task"}}, nil
	}, Generation: genFn, FetchAgents: emptyAgentList})
	if runErr != nil || len(r.got) != 2 || r.got[0].gen != 1 || r.got[1].gen != 4 {
		t.Fatalf("err=%v checks=%+v", runErr, r.got)
	}
}

func TestRun_ForceOverridesStatus(t *testing.T) {
	// The target is guaranteed absent, so the missing-agent path runs. With
	// Force=true the agent would be kicked regardless of status; assert the
	// dry-run still reports a kick and never panics.
	result, err := Run(Options{
		Names:        []string{"forge-no-such-lane"},
		Force:        true,
		DryRun:       true,
		Quiet:        true,
		RaiseMissing: false,
		HoldReader:   allowAllHolds{}, Identity: testIdentity,
		ActiveTasks: testActiveTasks,
		Generation:  testGeneration,
		FetchAgents: emptyAgentList,
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
	// With Quiet=false and DryRun=true, we should see output but no error.
	result, err := Run(Options{
		Names:        []string{"forge-no-such-lane"},
		DryRun:       true,
		Quiet:        false,
		RaiseMissing: false,
		HoldReader:   allowAllHolds{}, Identity: testIdentity,
		ActiveTasks: testActiveTasks,
		Generation:  testGeneration,
		FetchAgents: emptyAgentList,
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

type allowAllHolds struct{}

func (allowAllHolds) Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error) {
	return lifecycle.HoldDecision{Generation: 1}, nil
}

func testIdentity(name string) (lifecycle.HoldIdentity, error) {
	return lifecycle.HoldIdentity{Repository: "repo", Owner: name, Lane: name, Scope: "lane"}, nil
}

func testActiveTasks(_ context.Context, lane string) ([]lifecycle.HoldIdentity, error) {
	return []lifecycle.HoldIdentity{{Repository: "repo", Owner: lane, Lane: lane, Task: lane + "-task", Scope: "task"}}, nil
}

func testGeneration(context.Context, lifecycle.HoldIdentity) (int64, error) { return 1, nil }

func emptyAgentList() ([]AgentEntry, error) { return nil, nil }

func TestRun_BroadcastQuarantineMarkersNeverPrompt(t *testing.T) {
	// FAC-187: lifted-hold kick with FAC-151/FAC-172 quarantine markers must
	// not prompt those lanes; eligible lanes still receive exactly one kick.
	agents := []AgentEntry{
		{Name: "forge-worker", Status: "idle", PaneID: "p-w"},
		{Name: "task-fac-151", Status: "idle", PaneID: "p-151"},
		{Name: "p57", Status: "idle", PaneID: "p-172"},
		{Name: "review-assayer-fac-x", Status: "idle", PaneID: "p-r"},
	}
	result, err := Run(Options{
		Names:        []string{"forge-worker", "task-fac-151", "p57", "review-assayer-fac-x"},
		DryRun:       true,
		Quiet:        true,
		RaiseMissing: false,
		HoldReader:   allowAllHolds{},
		Identity:     testIdentity,
		ActiveTasks:  testActiveTasks,
		Generation:   testGeneration,
		FetchAgents:  func() ([]AgentEntry, error) { return agents, nil },
		Markers: map[string][]broadcast.ExclusionKind{
			"task-fac-151": {broadcast.ExcludeQuarantined, broadcast.ExcludeProtected},
			"p57":          {broadcast.ExcludeQuarantined},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]EntryResult{}
	for _, e := range result.Entries {
		byName[e.Name] = e
	}
	if byName["forge-worker"].Result != "dry-run" {
		t.Fatalf("eligible worker: %+v", byName["forge-worker"])
	}
	for _, name := range []string{"task-fac-151", "p57", "review-assayer-fac-x"} {
		if byName[name].Result != "skipped" {
			t.Fatalf("%s must be skipped, got %+v", name, byName[name])
		}
		if !strings.Contains(byName[name].Reason, "broadcast:") && name != "review-assayer-fac-x" {
			// review-assayer is auto-excluded as reviewer
		}
		if !strings.HasPrefix(byName[name].Reason, "broadcast:") {
			t.Fatalf("%s reason = %q want broadcast:…", name, byName[name].Reason)
		}
	}
	if result.Kicked != 1 || result.Skipped != 3 {
		t.Fatalf("kicked=%d skipped=%d want 1/3", result.Kicked, result.Skipped)
	}
}

func TestCompileKickExclusions_EphemeralReviewTabsOnly(t *testing.T) {
	// FAC-187: only ephemeral review-* tabs are auto-excluded. Standing
	// forge-assayer must remain kickable (recovery path).
	ex := compileKickExclusions(
		[]string{"forge-worker", "review-assayer-fac-151", "forge-assayer", "forge-reviewer"},
		nil, nil,
	)
	if _, ok := ex["forge-worker"]; ok {
		t.Fatal("worker must not be auto-excluded")
	}
	if ex["review-assayer-fac-151"] != string(broadcast.ExcludeReviewer) {
		t.Fatalf("review tab: %q", ex["review-assayer-fac-151"])
	}
	if _, ok := ex["forge-assayer"]; ok {
		t.Fatalf("standing forge-assayer must remain kickable, got %q", ex["forge-assayer"])
	}
	if _, ok := ex["forge-reviewer"]; ok {
		t.Fatalf("standing forge-reviewer must remain kickable, got %q", ex["forge-reviewer"])
	}
}

func TestRun_StandingAssayerIsKickable(t *testing.T) {
	// Proved the regression: blanket "assayer" exclusion made
	// herd kick forge-assayer (and --all) skip with broadcast:reviewer.
	agents := []AgentEntry{
		{Name: "forge-assayer", Status: "idle", PaneID: "p-assayer"},
		{Name: "review-assayer-fac-x", Status: "idle", PaneID: "p-ephemeral"},
	}
	for _, force := range []bool{false, true} {
		result, err := Run(Options{
			Names:        []string{"forge-assayer", "review-assayer-fac-x"},
			Force:        force,
			DryRun:       true,
			Quiet:        true,
			RaiseMissing: false,
			HoldReader:   allowAllHolds{},
			Identity:     testIdentity,
			ActiveTasks:  testActiveTasks,
			Generation:   testGeneration,
			FetchAgents:  func() ([]AgentEntry, error) { return agents, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]EntryResult{}
		for _, e := range result.Entries {
			byName[e.Name] = e
		}
		if byName["forge-assayer"].Result != "dry-run" {
			t.Fatalf("force=%v forge-assayer must be kicked, got %+v", force, byName["forge-assayer"])
		}
		if byName["review-assayer-fac-x"].Result != "skipped" {
			t.Fatalf("force=%v ephemeral review tab must stay excluded, got %+v", force, byName["review-assayer-fac-x"])
		}
		if !strings.HasPrefix(byName["review-assayer-fac-x"].Reason, "broadcast:") {
			t.Fatalf("force=%v reason = %q", force, byName["review-assayer-fac-x"].Reason)
		}
	}
}

// FAC-695: kick is the tool that wakes an idle lane, and exact-equality lookup
// meant it could not find a single standing lane. `herd kick` reported every
// one "missing (not live)" and finished kicked=0 while four were live and idle.
// The one command for restarting a stalled fleet was inert.
func TestLookupAgentMatchesDigestSuffixedStandingLane(t *testing.T) {
	agents := []AgentEntry{{Name: "forge-scout-planner-2918de97b5", Status: "idle", PaneID: "wB:p454"}}
	status, pane, found := LookupAgent(agents, "forge-scout-planner")
	if !found {
		t.Fatal("a live standing lane was reported missing; kick would refuse to wake it")
	}
	if status != "idle" || pane != "wB:p454" {
		t.Fatalf("resolved the lane but lost its state: status=%q pane=%q", status, pane)
	}
}

func TestLookupAgentPrefersAnExactMatch(t *testing.T) {
	// Exact must win, or a digest-suffixed sibling could shadow the precise
	// target a non-standing caller asked for.
	agents := []AgentEntry{
		{Name: "forge-review-2918de97b5", Status: "idle", PaneID: "wB:p1"},
		{Name: "forge-review", Status: "working", PaneID: "wB:p2"},
	}
	status, pane, found := LookupAgent(agents, "forge-review")
	if !found || status != "working" || pane != "wB:p2" {
		t.Fatalf("exact match did not win: status=%q pane=%q found=%v", status, pane, found)
	}
}

func TestLookupAgentDoesNotClaimAnUnrelatedLane(t *testing.T) {
	// The fallback must not let any running agent satisfy any roster entry, or
	// a missing lane silently looks healthy and never gets raised.
	agents := []AgentEntry{{Name: "forge-scout-planner-2918de97b5", Status: "idle"}}
	if _, _, found := LookupAgent(agents, "forge-recovery-sentinel"); found {
		t.Fatal("an absent lane was satisfied by an unrelated agent")
	}
}

// FAC-696: a paused goal-driven lane cannot consume a plain prompt. Every
// standing lane on the live fleet sat at "Goal paused (/goal resume)", so kick
// sent a normal message, the agent ignored it, and kick reported
// "FAIL unconsumed prompt" -- correct, and useless.
func TestPausedGoalIsDetectedFromPaneText(t *testing.T) {
	for _, marker := range []string{
		"gpt-5.6-luna high · ~/Personal/scout-planner   Goal paused (/goal resume)",
		"Goal stalled (/goal resume)",
		"Goal achieved",
		"Goal blocked",
	} {
		if !ContainsPausedGoalMarker(marker) {
			t.Fatalf("terminal goal state not detected in %q; kick would send a prompt the lane cannot consume", marker)
		}
	}
}

func TestHealthyPaneIsNotTreatedAsPaused(t *testing.T) {
	// Sending a resume verb into a working lane is its own failure.
	for _, text := range []string{
		"• Ran 5 commands · ctrl + t to view transcript",
		"› Ask Codex to do anything",
		"",
	} {
		if ContainsPausedGoalMarker(text) {
			t.Fatalf("healthy pane text was read as a paused goal: %q", text)
		}
	}
}
