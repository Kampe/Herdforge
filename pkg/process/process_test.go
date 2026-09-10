package process

import (
	"testing"
	"time"
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
		{"quota account suspended", "Fireworks upstream account suspended", Quota},
		{"quota insufficient", "insufficient_quota for project", Quota},
		{"quota 402 payment", "HTTP 402 Payment Required: billing not active", Quota},
		// OpenCode finish=length / output limit truncation: never Complete/Pass/NeedsReview
		{"finish length plain", "OpenCode finish=length", Unknown},
		{"finish length json style", `{"finish_reason": "length"}`, Unknown},
		{"finish length with complete marker", "Status: COMPLETE\nOpenCode finish=length", Unknown},
		{"finish length with needs review", "Status: NEEDS_REVIEW\n[finish_reason: length]", Unknown},
		// CHA-281: review content with quotation of 429/quota must not match
		{"review quoting quota prose has verdict marker", "CONFIRMED: the rate limit exceeded path returns 429", Unknown},
		{"review quoting rate limit prose", "The endpoint enforces a rate limit of 100/s and returns 429", Unknown},
		{"review quoting quota with findings", "Findings: rate limit exceeded", Unknown},
		{"review text with pass/fail marker", "pass/fail analysis shows quota handling", Unknown},
		{"review text with pass verdict discussing length", "Verdict: PASS\nTested finish=length boundary handling in test suite", Pass},
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
	if got := ProviderExhaustionReason("Individual quota reached"); got == "" {
		t.Fatal("Antigravity individual quota exhaustion was not detected")
	}
	if got := ProviderExhaustionReason("Fireworks upstream account suspended"); got == "" {
		t.Fatal("upstream account suspension was not detected")
	}
	if got := ProviderExhaustionReason("HTTP 402 Payment Required"); got == "" {
		t.Fatal("402 payment required was not detected")
	}
	if got := ProviderExhaustionReason("insufficient_quota on deepseek flash route"); got == "" {
		t.Fatal("insufficient_quota was not detected")
	}
	if got := ProviderExhaustionReason("CONFIRMED: the rate limit exceeded path is covered"); got != "" {
		t.Fatalf("review prose classified as provider failure: %q", got)
	}
}

func TestOutputLimitReason(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"OpenCode finish=length", true},
		{`finish_reason: "length"`, true},
		{`finish_reason=length`, true},
		{`output token limit reached`, true},
		{`maximum context length exceeded`, true},
		{`response truncated due to output limit`, true},
		{"normal text without limits", false},
		{"Verdict: PASS\nTested finish=length unit tests", false}, // review marker excludes
		{"CONFIRMED: finish_reason: length is tested", false},   // review marker excludes
	}

	for _, tt := range tests {
		got := OutputLimitReason(tt.input) != ""
		if got != tt.want {
			t.Errorf("OutputLimitReason(%q) match = %v, want %v", tt.input, got, tt.want)
		}
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
		{"upstream account suspended", "Fireworks upstream account suspended", true},
		{"account deactivated", "provider account deactivated", true},
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

func TestEvaluateEvidence_Freshness(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-fresh-1",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	// Fresh event within 1 minute
	freshEv := &TerminalEvidence{
		SessionID:    "sess-fresh-1",
		Provider:     "lazer",
		Model:        "deepseek-v4-flash",
		FinishReason: "quota",
		Error:        "429 Too Many Requests: out of credits",
		Timestamp:    now.Add(-1 * time.Minute),
	}
	resFresh := EvaluateEvidence(freshEv, ctx, "")
	if !resFresh.Fresh {
		t.Errorf("expected fresh event to be Fresh=true")
	}
	if resFresh.Class != Quota || !resFresh.Blocked || !resFresh.ProviderDeath {
		t.Errorf("expected Quota/Blocked/ProviderDeath for fresh quota failure, got %+v", resFresh)
	}

	// Stale event (10 minutes old, MaxAge is 5 minutes)
	staleEv := &TerminalEvidence{
		SessionID:    "sess-fresh-1",
		Provider:     "lazer",
		Model:        "deepseek-v4-flash",
		FinishReason: "quota",
		Error:        "429 Too Many Requests: out of credits",
		Timestamp:    now.Add(-10 * time.Minute),
	}
	resStale := EvaluateEvidence(staleEv, ctx, "Status: COMPLETE")
	if resStale.Fresh {
		t.Errorf("expected 10-minute old event to be Fresh=false")
	}
	// Stale event must NOT cool a provider or override with Quota
	if resStale.Class == Quota {
		t.Errorf("stale quota event must not set Class=QUOTA; got %+v", resStale)
	}
}

func TestEvaluateEvidence_SessionMismatch(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "current-session-abc",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	unrelatedEv := &TerminalEvidence{
		SessionID:    "old-unrelated-session-xyz",
		Provider:     "lazer",
		Model:        "deepseek-v4-flash",
		FinishReason: "quota",
		Error:        "429 Too Many Requests",
		Timestamp:    now,
	}

	res := EvaluateEvidence(unrelatedEv, ctx, "Status: COMPLETE")
	if res.SessionMatched {
		t.Errorf("expected SessionMatched=false for unrelated session ID")
	}
	// Unrelated session must not interrupt lane with Quota
	if res.Class == Quota {
		t.Errorf("unrelated session evidence must not set Class=QUOTA; got %+v", res)
	}
}

func TestEvaluateEvidence_BothFailureKinds(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-1",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	// Kind 1: finish=length (output limit truncation)
	lenEv := &TerminalEvidence{
		SessionID:    "sess-1",
		Provider:     "opencode",
		Model:        "deepseek-v4-flash",
		FinishReason: "length",
		Timestamp:    now,
	}
	resLen := EvaluateEvidence(lenEv, ctx, "Status: COMPLETE")
	if resLen.Class != Unknown {
		t.Errorf("finish=length must be Unknown, got %s", resLen.Class)
	}
	if resLen.Action != "read_pane" {
		t.Errorf("finish=length action should be read_pane, got %s", resLen.Action)
	}

	// Kind 2: quota exhaustion
	quotaEv := &TerminalEvidence{
		SessionID:    "sess-1",
		Provider:     "lazer",
		Account:      "fireworks",
		Model:        "deepseek-v4-flash",
		FinishReason: "error",
		Error:        "Fireworks upstream account suspended",
		Timestamp:    now,
	}
	resQuota := EvaluateEvidence(quotaEv, ctx, "")
	if resQuota.Class != Quota || !resQuota.Blocked || !resQuota.ProviderDeath {
		t.Errorf("quota failure must yield Quota/Blocked/ProviderDeath, got %+v", resQuota)
	}
	if resQuota.Action != "mark_unavailable_and_reroute" {
		t.Errorf("quota action should be mark_unavailable_and_reroute, got %s", resQuota.Action)
	}
	if resQuota.Provider != "lazer" || resQuota.Account != "fireworks" || resQuota.Model != "deepseek-v4-flash" {
		t.Errorf("quota evidence must retain provider/account/model scope, got %+v", resQuota)
	}
}

func TestEvaluateEvidence_SuccessAndToolExecution(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-1",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	// In-flight tool execution: finish_reason = tool_use
	toolEv := &TerminalEvidence{
		SessionID:    "sess-1",
		Provider:     "opencode",
		FinishReason: "tool_use",
		Timestamp:    now,
	}
	resTool := EvaluateEvidence(toolEv, ctx, "Status: COMPLETE")
	if resTool.Class != Unknown {
		t.Errorf("in-flight tool_use must be Unknown (not Complete), got %s", resTool.Class)
	}

	// Genuine completion: finish_reason = stop with Status: COMPLETE
	doneEv := &TerminalEvidence{
		SessionID:    "sess-1",
		Provider:     "opencode",
		FinishReason: "stop",
		Status:       "COMPLETE",
		Timestamp:    now,
	}
	resDone := EvaluateEvidence(doneEv, ctx, "Status: COMPLETE\nAll tests passed")
	if resDone.Class != Complete {
		t.Errorf("genuine stop with COMPLETE must be Complete, got %s", resDone.Class)
	}
	if resDone.Action != "close_or_activate" {
		t.Errorf("action should be close_or_activate, got %s", resDone.Action)
	}
}

func TestEvaluateEvidence_RepeatedObservations(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-repeat",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	quotaEv := &TerminalEvidence{
		SessionID:    "sess-repeat",
		Provider:     "lazer",
		Model:        "deepseek-v4-flash",
		FinishReason: "quota",
		Error:        "429 too many requests",
		Timestamp:    now,
	}

	// Evaluating multiple times must be idempotent and produce identical results
	res1 := EvaluateEvidence(quotaEv, ctx, "")
	res2 := EvaluateEvidence(quotaEv, ctx, "")
	if res1 != res2 {
		t.Errorf("repeated evaluation must be idempotent: res1=%+v, res2=%+v", res1, res2)
	}
}

func TestParseTerminalEvidence(t *testing.T) {
	// Valid JSON
	raw := `{"session_id": "s1", "provider": "lazer", "model": "deepseek-v4-flash", "finish_reason": "length"}`
	ev, err := ParseTerminalEvidence(raw)
	if err != nil {
		t.Fatalf("failed to parse valid terminal evidence: %v", err)
	}
	if ev.SessionID != "s1" || ev.FinishReason != "length" {
		t.Errorf("unexpected parsed evidence: %+v", ev)
	}

	// Embedded in text
	embedded := "Agent output log:\n" + raw + "\nDone"
	ev2, err := ParseTerminalEvidence(embedded)
	if err != nil {
		t.Fatalf("failed to parse embedded evidence: %v", err)
	}
	if ev2.SessionID != "s1" {
		t.Errorf("unexpected parsed embedded evidence: %+v", ev2)
	}

	// Malformed JSON
	_, err = ParseTerminalEvidence("{not valid json")
	if err == nil {
		t.Errorf("expected error on malformed JSON")
	}

	// Empty
	_, err = ParseTerminalEvidence("")
	if err == nil {
		t.Errorf("expected error on empty text")
	}

	// JSON without terminal evidence fields
	_, err = ParseTerminalEvidence(`{"foo": "bar"}`)
	if err == nil {
		t.Errorf("expected error on JSON missing terminal evidence fields")
	}
}

func TestClassifyTargetWithEvidence(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-target-1",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	ev := &TerminalEvidence{
		SessionID:    "sess-target-1",
		Provider:     "lazer",
		FinishReason: "length",
		Timestamp:    now,
	}

	target := ClassifyTargetWithEvidence("p1", "agent-1", "working", "Status: COMPLETE", ev, ctx)
	if target.Class != Unknown {
		t.Errorf("target with finish=length evidence must be Unknown, got %s", target.Class)
	}
	if target.Action != "read_pane" {
		t.Errorf("target action must be read_pane, got %s", target.Action)
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
