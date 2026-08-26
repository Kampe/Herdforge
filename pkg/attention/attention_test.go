package attention

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/kick"
)

func TestNeedsEyes(t *testing.T) {
	tests := []struct {
		level AttentionLevel
		want  bool
	}{
		{LevelCritical, true},
		{LevelHigh, true},
		{LevelMedium, true},
		{LevelMissing, true},
		{LevelLow, true},
		{LevelNone, false},
	}
	for _, tc := range tests {
		if got := NeedsEyes(tc.level); got != tc.want {
			t.Errorf("NeedsEyes(%s) = %v, want %v", tc.level, got, tc.want)
		}
	}
}

func TestClassifyAgent_Working(t *testing.T) {
	a := kick.AgentEntry{Name: "scout-planner", Status: "working", PaneID: "pane-1"}
	item := ClassifyAgent(a, false, "", false)
	if item.Level != LevelNone {
		t.Fatalf("working agent should be LevelNone, got %s", item.Level)
	}
	if item.Reason != "working" {
		t.Fatalf("reason should be 'working', got %q", item.Reason)
	}
}

func TestClassifyAgent_Starting(t *testing.T) {
	a := kick.AgentEntry{Name: "scout-planner", Status: "starting", PaneID: "pane-1"}
	item := ClassifyAgent(a, false, "", false)
	if item.Level != LevelNone {
		t.Fatalf("starting agent should be LevelNone, got %s", item.Level)
	}
}

func TestClassifyAgent_Done(t *testing.T) {
	a := kick.AgentEntry{Name: "ux-comber", Status: "done", PaneID: "pane-2"}
	item := ClassifyAgent(a, false, "", false)
	if item.Level != LevelHigh {
		t.Fatalf("done agent should be LevelHigh, got %s", item.Level)
	}
	if !strings.Contains(item.Reason, "done") {
		t.Fatalf("reason should mention done, got %q", item.Reason)
	}
}

func TestClassifyAgent_Blocked(t *testing.T) {
	a := kick.AgentEntry{Name: "api-crusader", Status: "blocked", PaneID: "pane-3"}
	item := ClassifyAgent(a, false, "", false)
	if item.Level != LevelCritical {
		t.Fatalf("blocked agent should be LevelCritical, got %s", item.Level)
	}
}

func TestClassifyAgent_Idle(t *testing.T) {
	a := kick.AgentEntry{Name: "herd-smith", Status: "idle", PaneID: "pane-4"}
	item := ClassifyAgent(a, false, "", false)
	if item.Level != LevelMedium {
		t.Fatalf("idle agent should be LevelMedium, got %s", item.Level)
	}
}

func TestClassifyAgent_Unknown(t *testing.T) {
	a := kick.AgentEntry{Name: "qa-sentinel", Status: "unknown", PaneID: "pane-5"}
	item := ClassifyAgent(a, false, "", false)
	if item.Level != LevelMedium {
		t.Fatalf("unknown status should be LevelMedium, got %s", item.Level)
	}
}

func TestClassifyAgent_EmptyStatus(t *testing.T) {
	a := kick.AgentEntry{Name: "qa-sentinel", Status: "", PaneID: "pane-5"}
	item := ClassifyAgent(a, false, "", false)
	if item.Level != LevelMedium {
		t.Fatalf("empty status should be LevelMedium, got %s", item.Level)
	}
}

func TestClassifyAgent_ProviderDeath(t *testing.T) {
	a := kick.AgentEntry{Name: "docs-custodian", Status: "idle", PaneID: "pane-6"}
	item := ClassifyAgent(a, false, "", true)
	if item.Level != LevelCritical {
		t.Fatalf("provider death should be LevelCritical, got %s", item.Level)
	}
	if !strings.Contains(strings.ToLower(item.Reason), "provider") {
		t.Fatalf("reason should mention provider, got %q", item.Reason)
	}
}

func TestClassifyAgent_Held(t *testing.T) {
	a := kick.AgentEntry{Name: "platform-ops", Status: "idle", PaneID: "pane-7"}
	item := ClassifyAgent(a, true, "manual cooldown", false)
	if !item.Held {
		t.Fatal("held should be true")
	}
	if item.HeldReason != "manual cooldown" {
		t.Fatalf("held reason = %q, want %q", item.HeldReason, "manual cooldown")
	}
	if item.Level != LevelLow {
		t.Fatalf("held agent should be LevelLow, got %s", item.Level)
	}
}

func TestClassifyAgent_HeldOverridesBlocked(t *testing.T) {
	// A held lane is parked by the coordinator — even if it reports blocked,
	// the hold wins because the coordinator already decided to park it.
	a := kick.AgentEntry{Name: "platform-ops", Status: "blocked", PaneID: "pane-7"}
	item := ClassifyAgent(a, true, "cooldown", false)
	if item.Level != LevelLow {
		t.Fatalf("held+blocked should be LevelLow, got %s", item.Level)
	}
}

func TestTriage_MissingStanding(t *testing.T) {
	agents := []kick.AgentEntry{
		{Name: "scout-planner", Status: "working", PaneID: "p1"},
	}
	r := Triage(agents, []string{"scout-planner", "ux-comber"}, noHold, noDeath)
	if len(r.Items) != 1 {
		t.Fatalf("expected 1 item needing eyes (ux-comber missing), got %d", len(r.Items))
	}
	if r.Items[0].Name != "ux-comber" {
		t.Fatalf("expected ux-comber, got %s", r.Items[0].Name)
	}
	if r.Items[0].Level != LevelMissing {
		t.Fatalf("missing agent should be LevelMissing, got %s", r.Items[0].Level)
	}
}

func TestTriage_WorkingExcluded(t *testing.T) {
	agents := []kick.AgentEntry{
		{Name: "scout-planner", Status: "working", PaneID: "p1"},
	}
	r := Triage(agents, []string{"scout-planner"}, noHold, noDeath)
	if len(r.Items) != 0 {
		t.Fatalf("working agent should not need eyes, got %d items", len(r.Items))
	}
	if r.Needing != 0 {
		t.Fatalf("Needing should be 0, got %d", r.Needing)
	}
}

func TestTriage_DoneNeedsEyes(t *testing.T) {
	agents := []kick.AgentEntry{
		{Name: "ux-comber", Status: "done", PaneID: "p2"},
	}
	r := Triage(agents, []string{"ux-comber"}, noHold, noDeath)
	if len(r.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(r.Items))
	}
	if r.Items[0].Level != LevelHigh {
		t.Fatalf("done should be LevelHigh, got %s", r.Items[0].Level)
	}
}

func TestTriage_BlockedCritical(t *testing.T) {
	agents := []kick.AgentEntry{
		{Name: "api-crusader", Status: "blocked", PaneID: "p3"},
	}
	r := Triage(agents, []string{"api-crusader"}, noHold, noDeath)
	if r.Items[0].Level != LevelCritical {
		t.Fatalf("blocked should be LevelCritical, got %s", r.Items[0].Level)
	}
}

func TestTriage_HeldLow(t *testing.T) {
	agents := []kick.AgentEntry{
		{Name: "platform-ops", Status: "idle", PaneID: "p7"},
	}
	held := func(name string) (string, bool) {
		if name == "platform-ops" {
			return "manual cooldown", true
		}
		return "", false
	}
	r := Triage(agents, []string{"platform-ops"}, held, noDeath)
	if r.Items[0].Level != LevelLow {
		t.Fatalf("held should be LevelLow, got %s", r.Items[0].Level)
	}
	if !r.Items[0].Held {
		t.Fatal("Held should be true")
	}
}

func TestTriage_SortedByUrgency(t *testing.T) {
	// critical (blocked) should sort before high (done) before medium (idle)
	// before missing before low (held).
	agents := []kick.AgentEntry{
		{Name: "herd-smith", Status: "idle", PaneID: "p1"},      // medium
		{Name: "ux-comber", Status: "done", PaneID: "p2"},       // high
		{Name: "api-crusader", Status: "blocked", PaneID: "p3"}, // critical
	}
	roster := []string{"api-crusader", "ux-comber", "herd-smith"}
	r := Triage(agents, roster, noHold, noDeath)
	if len(r.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(r.Items))
	}
	if r.Items[0].Level != LevelCritical {
		t.Fatalf("first should be critical, got %s", r.Items[0].Level)
	}
	if r.Items[1].Level != LevelHigh {
		t.Fatalf("second should be high, got %s", r.Items[1].Level)
	}
	if r.Items[2].Level != LevelMedium {
		t.Fatalf("third should be medium, got %s", r.Items[2].Level)
	}
}

func TestTriage_DeterministicOrder(t *testing.T) {
	// Two agents at the same level must sort by name ASC for determinism.
	agents := []kick.AgentEntry{
		{Name: "zeta-lane", Status: "done", PaneID: "p1"},
		{Name: "alpha-lane", Status: "done", PaneID: "p2"},
	}
	r1 := Triage(agents, []string{"zeta-lane", "alpha-lane"}, noHold, noDeath)
	r2 := Triage(agents, []string{"alpha-lane", "zeta-lane"}, noHold, noDeath)
	if len(r1.Items) != 2 || len(r2.Items) != 2 {
		t.Fatalf("expected 2 items each, got %d/%d", len(r1.Items), len(r2.Items))
	}
	// Both runs must produce identical order regardless of roster input order.
	if r1.Items[0].Name != r2.Items[0].Name {
		t.Fatalf("non-deterministic: r1[0]=%s r2[0]=%s", r1.Items[0].Name, r2.Items[0].Name)
	}
	// alpha-lane must come before zeta-lane (name ASC).
	if r1.Items[0].Name != "alpha-lane" {
		t.Fatalf("expected alpha-lane first, got %s", r1.Items[0].Name)
	}
}

func TestTriage_Counts(t *testing.T) {
	agents := []kick.AgentEntry{
		{Name: "api-crusader", Status: "blocked", PaneID: "p3"},  // critical
		{Name: "ux-comber", Status: "done", PaneID: "p2"},        // high
		{Name: "herd-smith", Status: "idle", PaneID: "p1"},       // medium
		{Name: "scout-planner", Status: "working", PaneID: "p4"}, // none (excluded)
	}
	roster := []string{"api-crusader", "ux-comber", "herd-smith", "scout-planner"}
	r := Triage(agents, roster, noHold, noDeath)
	if r.Counts[LevelCritical] != 1 {
		t.Fatalf("critical count = %d, want 1", r.Counts[LevelCritical])
	}
	if r.Counts[LevelHigh] != 1 {
		t.Fatalf("high count = %d, want 1", r.Counts[LevelHigh])
	}
	if r.Counts[LevelMedium] != 1 {
		t.Fatalf("medium count = %d, want 1", r.Counts[LevelMedium])
	}
	if r.Counts[LevelNone] != 1 {
		t.Fatalf("none count = %d, want 1", r.Counts[LevelNone])
	}
	if r.Needing != 3 {
		t.Fatalf("Needing = %d, want 3", r.Needing)
	}
	if r.Total != 4 {
		t.Fatalf("Total = %d, want 4", r.Total)
	}
}

func TestTriage_MissingCounted(t *testing.T) {
	agents := []kick.AgentEntry{
		{Name: "scout-planner", Status: "working", PaneID: "p1"},
	}
	r := Triage(agents, []string{"scout-planner", "ux-comber", "docs-custodian"}, noHold, noDeath)
	if r.Counts[LevelMissing] != 2 {
		t.Fatalf("missing count = %d, want 2", r.Counts[LevelMissing])
	}
	if r.Needing != 2 {
		t.Fatalf("Needing = %d, want 2", r.Needing)
	}
}

func TestTriage_ProviderDeathCritical(t *testing.T) {
	agents := []kick.AgentEntry{
		{Name: "docs-custodian", Status: "idle", PaneID: "p6"},
	}
	death := func(name string) bool { return name == "docs-custodian" }
	r := Triage(agents, []string{"docs-custodian"}, noHold, death)
	if r.Items[0].Level != LevelCritical {
		t.Fatalf("provider death should be LevelCritical, got %s", r.Items[0].Level)
	}
}

func TestTriage_EmptyRoster(t *testing.T) {
	r := Triage(nil, nil, noHold, noDeath)
	if r.Total != 0 {
		t.Fatalf("Total = %d, want 0", r.Total)
	}
	if r.Needing != 0 {
		t.Fatalf("Needing = %d, want 0", r.Needing)
	}
	if len(r.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(r.Items))
	}
}

func TestTriage_LabelFallback(t *testing.T) {
	// herdr sometimes reports label instead of name — attention must match it.
	agents := []kick.AgentEntry{
		{Label: "ux-comber", Status: "done", PaneID: "p2"},
	}
	r := Triage(agents, []string{"ux-comber"}, noHold, noDeath)
	if len(r.Items) != 1 {
		t.Fatalf("expected 1 item via label fallback, got %d", len(r.Items))
	}
	if r.Items[0].Name != "ux-comber" {
		t.Fatalf("expected ux-comber, got %s", r.Items[0].Name)
	}
}

func TestSummary(t *testing.T) {
	r := Result{
		Items: []Item{
			{Name: "api-crusader", Level: LevelCritical, Reason: "blocked"},
			{Name: "ux-comber", Level: LevelHigh, Reason: "done"},
		},
		Counts: map[AttentionLevel]int{
			LevelCritical: 1,
			LevelHigh:     1,
		},
		Total:   2,
		Needing: 2,
	}
	s := Summary(r)
	if !strings.Contains(s, "2") {
		t.Fatalf("summary should mention count, got %q", s)
	}
}

func TestSummary_Nothing(t *testing.T) {
	r := Result{}
	s := Summary(r)
	if !strings.Contains(strings.ToLower(s), "no") {
		t.Fatalf("empty summary should say nothing needs eyes, got %q", s)
	}
}

func TestFormatItem(t *testing.T) {
	item := Item{Name: "ux-comber", Status: "done", Level: LevelHigh, Reason: "done — awaiting review", PaneID: "p2"}
	s := FormatItem(item)
	if !strings.Contains(s, "ux-comber") {
		t.Fatalf("format should contain name, got %q", s)
	}
	if !strings.Contains(s, "high") {
		t.Fatalf("format should contain level, got %q", s)
	}
}

func TestResultJSON(t *testing.T) {
	r := Result{
		Items: []Item{
			{Name: "api-crusader", Status: "blocked", Level: LevelCritical, Reason: "blocked", PaneID: "p3"},
		},
		Counts: map[AttentionLevel]int{LevelCritical: 1},
		Total:  1, Needing: 1,
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "api-crusader") {
		t.Fatal("json should contain name")
	}
	if !strings.Contains(string(data), "critical") {
		t.Fatal("json should contain level")
	}
}

func TestSelftest(t *testing.T) {
	if err := Selftest(); err != nil {
		t.Fatalf("selftest should pass: %v", err)
	}
}

func TestUrgencyRank_Ordered(t *testing.T) {
	// Sanity: critical must rank higher (more urgent) than high, etc.
	if urgencyRank(LevelCritical) <= urgencyRank(LevelHigh) {
		t.Fatal("critical must outrank high")
	}
	if urgencyRank(LevelHigh) <= urgencyRank(LevelMedium) {
		t.Fatal("high must outrank medium")
	}
	if urgencyRank(LevelMedium) <= urgencyRank(LevelMissing) {
		t.Fatal("medium must outrank missing")
	}
	if urgencyRank(LevelMissing) <= urgencyRank(LevelLow) {
		t.Fatal("missing must outrank low")
	}
	if urgencyRank(LevelLow) <= urgencyRank(LevelNone) {
		t.Fatal("low must outrank none")
	}
}

// FAC-660: the roster and the fleet never spell a lane the same way, so exact
// equality found no standing lane at all. attention reported state=UNKNOWN with
// a full fleet running, while pulse counted the same agents and reported nine
// busy. Two numbers describing one fleet at one instant is the tell.
func TestFindAttentionAgentResolvesTheLiveSpellingOfALane(t *testing.T) {
	agents := []kick.AgentEntry{
		{Name: "forge-herd-smith-2918de97b5"},
		{Name: "review-cha-2796-abc"},
	}
	got, ok := findAttentionAgent(agents, "forge-herd-smith")
	if !ok {
		t.Fatal("a running standing lane must be found under its repository-qualified name")
	}
	if got.Name != "forge-herd-smith-2918de97b5" {
		t.Errorf("resolved the wrong agent: %q", got.Name)
	}
}

// A roster entry with no live agent must still report absent, or attention would
// claim coverage it does not have.
func TestFindAttentionAgentStillReportsAGenuinelyAbsentLane(t *testing.T) {
	agents := []kick.AgentEntry{{Name: "review-cha-2796-abc"}}
	if _, ok := findAttentionAgent(agents, "forge-herd-smith"); ok {
		t.Fatal("a lane with no live agent must report absent")
	}
}
