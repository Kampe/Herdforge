package process

import (
	"testing"
)

// mockCommitsAhead returns preset commit counts for stall detection tests.
func mockCommitsAhead(counts map[string]int) func(string, string) int {
	return func(agentName, repoRoot string) int {
		if c, ok := counts[agentName]; ok {
			return c
		}
		return -1
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Classification
	}{
		{"needs review explicit", "Status: NEEDS_REVIEW", NeedsReview},
		{"needs review inline", "NEEDS_REVIEW in results", NeedsReview},
		{"pass verdict", "Verdict: PASS", Pass},
		{"pass merge yes", "Merge recommendation: YES", Pass},
		{"fail verdict", "Verdict: FAIL", Fail},
		{"fail merge no", "Merge recommendation: NO", Fail},
		{"complete", "Status: COMPLETE", Complete},
		{"complete at eol", "All work Status: COMPLETE", Complete},
		{"blocked explicit", "Status: BLOCKED", Blocked},
		{"blocked colon", "BLOCKED: timeout", Blocked},
		{"quota weekly", "weekly quota exceeded", Quota},
		{"quota usage limit", "usage limit reached; resets in 3h", Quota},
		{"quota rate hit", "rate limit exceeded", Quota},
		{"quota token quota", "token quota reached", Quota},
		{"quota too many requests", "429 too many requests", Quota},
		// CHA-281: review content with quotation of 429/quota must not match
		{"review quoting quota prose has verdict marker", "CONFIRMED: the rate limit exceeded path returns 429", Unknown},
		{"review quoting rate limit prose", "The endpoint enforces a rate limit of 100/s and returns 429", Unknown},
		{"review quoting quota with findings", "Findings: rate limit exceeded", Unknown},
		{"review text with pass/fail marker", "pass/fail analysis shows quota handling", Unknown},
		{"unconsumed overridden by status", "❯ working...\nStatus: COMPLETE", Complete},
		{"unconsumed with Worked for falls to unknown", "❯ continue\nWorked for: 5m", Unknown},
		// Empty/unusual
		{"empty string", "", Unknown},
		{"garbage", "asdkjhfa sdkjhf asdkjhf", Unknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyText(tt.input)
			if got != tt.want {
				t.Errorf("classifyText(%q) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

func TestProviderExhaustionReason(t *testing.T) {
	if got := ProviderExhaustionReason("OpenCode: weekly quota exceeded; retry later"); got == "" {
		t.Fatal("quota failure was not detected")
	}
	if got := ProviderExhaustionReason("OpenCode: out of quota until reset"); got == "" {
		t.Fatal("out-of-quota failure was not detected")
	}
	if got := ProviderExhaustionReason("CONFIRMED: the rate limit exceeded path is covered"); got != "" {
		t.Fatalf("review prose classified as provider failure: %q", got)
	}
}

func TestActionFor(t *testing.T) {
	tests := []struct {
		c               Classification
		isProviderDeath bool
		want            string
	}{
		{NeedsReview, false, "dispatch_review_or_merge_gate"},
		{Pass, false, "merge_if_tier_ok"},
		{Fail, false, "return_to_builder"},
		{Complete, false, "close_or_activate"},
		{Blocked, false, "unblock_or_reassign"},
		{Quota, false, "mark_unavailable_and_reroute"},
		{Unknown, false, "read_pane"},
		{NeedsReview, true, "provider_death_cooled_reset_aware"},
		{Unknown, true, "provider_death_cooled_reset_aware"},
	}

	for _, tt := range tests {
		t.Run(string(tt.c), func(t *testing.T) {
			got := actionFor(tt.c, tt.isProviderDeath)
			if got != tt.want {
				t.Errorf("actionFor(%s, %v) = %s, want %s", tt.c, tt.isProviderDeath, got, tt.want)
			}
		})
	}
}

func TestCheckProviderDeath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty", "", false},
		{"normal output", "Verdict: PASS\nAll tests passed", false},
		{"connection lost", "connection lost to Claude provider", true},
		{"session closed", "session closed unexpectedly", true},
		{"auth expired", "API token expired", true},
		{"auth denied", "credentials denied: unauthorized", true},
		{"model unavailable", "model unavailable for inference", true},
		{"provider error", "provider error: upstream timeout", true},
		{"harness crash", "herdr fatal: signal terminated", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckProviderDeath(tt.input)
			if got != tt.want {
				t.Errorf("CheckProviderDeath(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSelftest(t *testing.T) {
	if err := Selftest(); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyTarget(t *testing.T) {
	target := ClassifyTarget("pane-1", "agent-foo", "idle",
		"Status: COMPLETE\nTask ID: TASK-123\nEpisode ID: EP-456\nAll work done")

	if target.Class != Complete {
		t.Errorf("class = %s, want COMPLETE", target.Class)
	}
	if target.Action != "close_or_activate" {
		t.Errorf("action = %s, want close_or_activate", target.Action)
	}
	if target.Name != "agent-foo" {
		t.Errorf("name = %s", target.Name)
	}
	if len(target.Tail) == 0 {
		t.Error("tail should not be empty")
	}
}

func TestDigestJSON(t *testing.T) {
	targets := []Target{
		{PaneID: "p1", Name: "a1", Status: "idle", Class: Complete, Action: "close_or_activate"},
	}
	data, err := DigestJSON("ws-1", targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty json")
	}
}

func TestTail(t *testing.T) {
	got := tail("line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10", 8, 220)
	if len(got) == 0 {
		t.Fatal("empty tail")
	}
}

func TestExtractField(t *testing.T) {
	if got := extractField("Task ID: ABC-123\nStatus: OK", "Task ID"); got != "ABC-123" {
		t.Errorf("got %q, want ABC-123", got)
	}
	if got := extractField("no match", "Task ID"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestStalledAgents(t *testing.T) {
	tests := []struct {
		name      string
		agents    []herdrAgentEntry
		repoRoot  string
		ahead     map[string]int
		wantCount int
	}{
		{
			name: "no done agents",
			agents: []herdrAgentEntry{
				{Name: "agent-a", Status: "running"},
				{Name: "agent-b", Status: "starting"},
			},
			ahead:     map[string]int{},
			wantCount: 0,
		},
		{
			name: "done agent with commits is not stalled",
			agents: []herdrAgentEntry{
				{Name: "agent-a", Status: "done"},
			},
			ahead:     map[string]int{"agent-a": 3},
			wantCount: 0,
		},
		{
			name: "done agent with zero commits is stalled",
			agents: []herdrAgentEntry{
				{Name: "agent-a", Status: "done"},
			},
			ahead:     map[string]int{"agent-a": 0},
			wantCount: 1,
		},
		{
			name: "idle agent with zero commits is stalled",
			agents: []herdrAgentEntry{
				{Name: "agent-b", Status: "idle"},
			},
			ahead:     map[string]int{"agent-b": 0},
			wantCount: 1,
		},
		{
			name: "mixed agents only stalled found",
			agents: []herdrAgentEntry{
				{Name: "agent-a", Status: "done"},
				{Name: "agent-b", Status: "done"},
				{Name: "agent-c", Status: "running"},
			},
			ahead:     map[string]int{"agent-a": 0, "agent-b": 5, "agent-c": 0},
			wantCount: 1,
		},
		{
			name: "all stalled",
			agents: []herdrAgentEntry{
				{Name: "agent-x", Status: "done"},
				{Name: "agent-y", Status: "idle"},
			},
			ahead:     map[string]int{"agent-x": 0, "agent-y": 0},
			wantCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := execCommitsAhead
			execCommitsAhead = mockCommitsAhead(tt.ahead)
			defer func() { execCommitsAhead = old }()

			got := StalledAgents(tt.agents, tt.repoRoot)
			if len(got) != tt.wantCount {
				t.Errorf("StalledAgents() returned %d agents (%v), want %d", len(got), got, tt.wantCount)
			}
		})
	}
}
