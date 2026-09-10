package process

import (
	"errors"
	"fmt"
	"strings"
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

func TestTerminalEvidence_Validation(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-val-1",
		TurnID:    "turn-1",
		Provider:  "lazer",
		Model:     "deepseek-v4-flash",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	valid := TerminalEvidence{
		SessionID: "sess-val-1",
		TurnID:    "turn-1",
		Provider:  "lazer",
		Model:     "deepseek-v4-flash",
		Timestamp: now,
	}

	if err := valid.Validate(ctx); err != nil {
		t.Fatalf("expected valid evidence to pass validation: %v", err)
	}

	// Missing fields
	evNoSess := valid
	evNoSess.SessionID = ""
	if err := evNoSess.Validate(ctx); !errors.Is(err, ErrMissingSessionID) {
		t.Errorf("expected ErrMissingSessionID, got %v", err)
	}

	evNoTurn := valid
	evNoTurn.TurnID = ""
	if err := evNoTurn.Validate(ctx); !errors.Is(err, ErrMissingTurnID) {
		t.Errorf("expected ErrMissingTurnID, got %v", err)
	}

	evNoProv := valid
	evNoProv.Provider = ""
	if err := evNoProv.Validate(ctx); !errors.Is(err, ErrMissingProvider) {
		t.Errorf("expected ErrMissingProvider, got %v", err)
	}

	evNoMod := valid
	evNoMod.Model = ""
	if err := evNoMod.Validate(ctx); !errors.Is(err, ErrMissingModel) {
		t.Errorf("expected ErrMissingModel, got %v", err)
	}

	evNoTime := valid
	evNoTime.Timestamp = time.Time{}
	if err := evNoTime.Validate(ctx); !errors.Is(err, ErrMissingTimestamp) {
		t.Errorf("expected ErrMissingTimestamp, got %v", err)
	}

	// Mismatches
	evSessMismatch := valid
	evSessMismatch.SessionID = "other-session"
	if err := evSessMismatch.Validate(ctx); !errors.Is(err, ErrSessionMismatch) {
		t.Errorf("expected ErrSessionMismatch, got %v", err)
	}

	evTurnMismatch := valid
	evTurnMismatch.TurnID = "turn-2"
	if err := evTurnMismatch.Validate(ctx); !errors.Is(err, ErrTurnMismatch) {
		t.Errorf("expected ErrTurnMismatch, got %v", err)
	}

	evRouteMismatch := valid
	evRouteMismatch.Provider = "anthropic"
	if err := evRouteMismatch.Validate(ctx); !errors.Is(err, ErrRouteMismatch) {
		t.Errorf("expected ErrRouteMismatch, got %v", err)
	}

	// Staleness
	evStale := valid
	evStale.Timestamp = now.Add(-10 * time.Minute)
	if err := evStale.Validate(ctx); !errors.Is(err, ErrStaleEvidence) {
		t.Errorf("expected ErrStaleEvidence for 10m old evidence, got %v", err)
	}
}

func TestEvaluateEvidence_Freshness(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-fresh-1",
		TurnID:    "turn-1",
		Provider:  "lazer",
		Model:     "deepseek-v4-flash",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	// Fresh event within 1 minute
	freshEv := &TerminalEvidence{
		SessionID:    "sess-fresh-1",
		TurnID:       "turn-1",
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
	if resFresh.Action != "mark_unavailable_and_reroute" {
		t.Errorf("action should be mark_unavailable_and_reroute, got %s", resFresh.Action)
	}

	// Stale event (10 minutes old, MaxAge is 5 minutes)
	staleEv := &TerminalEvidence{
		SessionID:    "sess-fresh-1",
		TurnID:       "turn-1",
		Provider:     "lazer",
		Model:        "deepseek-v4-flash",
		FinishReason: "quota",
		Error:        "429 Too Many Requests: out of credits",
		Timestamp:    now.Add(-10 * time.Minute),
	}
	// Stale event with quota text in pane MUST NOT trigger mark_unavailable_and_reroute or Quota class
	resStale := EvaluateEvidence(staleEv, ctx, "429 Too Many Requests: out of credits")
	if resStale.Fresh {
		t.Errorf("expected 10-minute old event to be Fresh=false")
	}
	if resStale.Class == Quota || resStale.Blocked || resStale.ProviderDeath {
		t.Errorf("stale quota event must cause NO stop/cooldown; got %+v", resStale)
	}
	if resStale.Action != "read_pane" {
		t.Errorf("stale event action must be read_pane, got %s", resStale.Action)
	}
}

func TestEvaluateEvidence_SessionMismatch(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "current-session-abc",
		TurnID:    "turn-1",
		Provider:  "lazer",
		Model:     "deepseek-v4-flash",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	unrelatedEv := &TerminalEvidence{
		SessionID:    "old-unrelated-session-xyz",
		TurnID:       "turn-1",
		Provider:     "lazer",
		Model:        "deepseek-v4-flash",
		FinishReason: "quota",
		Error:        "429 Too Many Requests",
		Timestamp:    now,
	}

	// Session mismatch with raw quota text in pane MUST NOT trigger mark_unavailable_and_reroute or Quota class
	res := EvaluateEvidence(unrelatedEv, ctx, "Status: COMPLETE\n429 Too Many Requests")
	if res.SessionMatched {
		t.Errorf("expected SessionMatched=false for unrelated session ID")
	}
	if res.Class == Quota || res.Blocked || res.ProviderDeath {
		t.Errorf("unrelated session evidence must cause NO stop/cooldown; got %+v", res)
	}
	if res.Action != "read_pane" {
		t.Errorf("unrelated session action must be read_pane, got %s", res.Action)
	}
}

func TestEvaluateEvidence_BothFailureKinds(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctxLen := SessionContext{
		SessionID: "sess-1",
		TurnID:    "turn-1",
		Provider:  "opencode",
		Model:     "deepseek-v4-flash",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	// Kind 1: finish=length (output limit truncation)
	lenEv := &TerminalEvidence{
		SessionID:    "sess-1",
		TurnID:       "turn-1",
		Provider:     "opencode",
		Model:        "deepseek-v4-flash",
		FinishReason: "length",
		Timestamp:    now,
	}
	// Even if pane says Status: COMPLETE and Verdict: PASS, finish=length MUST force Unknown / incomplete
	resLen := EvaluateEvidence(lenEv, ctxLen, "Status: COMPLETE\nVerdict: PASS\nMerge recommendation: YES")
	if resLen.Class != Unknown {
		t.Errorf("finish=length must be Unknown, got %s", resLen.Class)
	}
	if resLen.Action != "read_pane" {
		t.Errorf("finish=length action should be read_pane, got %s", resLen.Action)
	}

	// Kind 2: quota exhaustion
	ctxQuota := SessionContext{
		SessionID: "sess-1",
		TurnID:    "turn-1",
		Provider:  "lazer",
		Account:   "fireworks",
		Model:     "deepseek-v4-flash",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}
	quotaEv := &TerminalEvidence{
		SessionID:    "sess-1",
		TurnID:       "turn-1",
		Provider:     "lazer",
		Account:      "fireworks",
		Model:        "deepseek-v4-flash",
		FinishReason: "error",
		Error:        "Fireworks upstream account suspended",
		Timestamp:    now,
	}
	resQuota := EvaluateEvidence(quotaEv, ctxQuota, "")
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
		TurnID:    "turn-1",
		Provider:  "opencode",
		Model:     "deepseek-v4-flash",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	// In-flight tool execution: finish_reason = tool_use
	toolEv := &TerminalEvidence{
		SessionID:    "sess-1",
		TurnID:       "turn-1",
		Provider:     "opencode",
		Model:        "deepseek-v4-flash",
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
		TurnID:       "turn-1",
		Provider:     "opencode",
		Model:        "deepseek-v4-flash",
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

func TestParseTerminalEvidence(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	nowStr := now.Format(time.RFC3339)

	// Valid structured JSON
	raw := fmt.Sprintf(`{"session_id": "s1", "turn_id": "t1", "provider": "lazer", "model": "deepseek-v4-flash", "finish_reason": "length", "timestamp": "%s"}`, nowStr)
	ev, err := ParseTerminalEvidence([]byte(raw))
	if err != nil {
		t.Fatalf("failed to parse valid terminal evidence: %v", err)
	}
	if ev.SessionID != "s1" || ev.TurnID != "t1" || ev.FinishReason != "length" {
		t.Errorf("unexpected parsed evidence: %+v", ev)
	}

	// Loose prose with embedded JSON must be REJECTED
	embedded := "Agent output log:\n" + raw + "\nDone"
	_, err = ParseTerminalEvidence([]byte(embedded))
	if err == nil {
		t.Errorf("expected error on loose prose with embedded JSON")
	}

	// Missing required fields
	missingSess := fmt.Sprintf(`{"turn_id": "t1", "provider": "lazer", "model": "deepseek-v4-flash", "timestamp": "%s"}`, nowStr)
	_, err = ParseTerminalEvidence([]byte(missingSess))
	if err == nil {
		t.Errorf("expected error on JSON missing session_id")
	}

	// Malformed JSON
	_, err = ParseTerminalEvidence([]byte("{not valid json"))
	if err == nil {
		t.Errorf("expected error on malformed JSON")
	}

	// Empty
	_, err = ParseTerminalEvidence([]byte(""))
	if err == nil {
		t.Errorf("expected error on empty payload")
	}
}

func TestQuotaRetryStopController_BoundedAndIdempotent(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-stop-1",
		TurnID:    "turn-1",
		Provider:  "lazer",
		Account:   "fireworks",
		Model:     "deepseek-v4-flash",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	ctrl := NewQuotaRetryStopController()

	quotaEv := &TerminalEvidence{
		SessionID:    "sess-stop-1",
		TurnID:       "turn-1",
		Provider:     "lazer",
		Account:      "fireworks",
		Model:        "deepseek-v4-flash",
		FinishReason: "quota",
		Error:        "429 Too Many Requests: quota exceeded",
		Timestamp:    now,
	}

	// 1. Initial stop recording
	rec1, created1, err := ctrl.RecordQuotaStop(quotaEv, ctx, 15*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error on initial stop recording: %v", err)
	}
	if !created1 {
		t.Errorf("expected created=true on initial stop")
	}
	if rec1.StopCount != 1 {
		t.Errorf("expected StopCount=1, got %d", rec1.StopCount)
	}

	key := QuotaStopKey{Provider: "lazer", Account: "fireworks", Model: "deepseek-v4-flash"}
	stopped, reason := ctrl.IsRouteStopped(key, now)
	if !stopped {
		t.Errorf("expected route to be stopped")
	}
	if !strings.Contains(reason, "quota") {
		t.Errorf("expected quota reason, got %q", reason)
	}

	// 2. Idempotent repeat for exact same session and turn
	rec2, created2, err := ctrl.RecordQuotaStop(quotaEv, ctx, 15*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error on idempotent repeat: %v", err)
	}
	if created2 {
		t.Errorf("expected created=false on duplicate observation")
	}
	if rec2.StopCount != 1 {
		t.Errorf("idempotent duplicate should not increment stop count: got %d", rec2.StopCount)
	}

	// 3. Stale observation is REFUSED (no stop creation / extension)
	staleEv := &TerminalEvidence{
		SessionID:    "sess-stop-1",
		TurnID:       "turn-2",
		Provider:     "lazer",
		Account:      "fireworks",
		Model:        "deepseek-v4-flash",
		FinishReason: "quota",
		Error:        "429 Too Many Requests",
		Timestamp:    now.Add(-20 * time.Minute),
	}
	_, _, err = ctrl.RecordQuotaStop(staleEv, ctx, 15*time.Minute)
	if err == nil {
		t.Errorf("expected error when recording stale quota evidence")
	}

	// 4. Mismatched session is REFUSED
	mismatchedEv := &TerminalEvidence{
		SessionID:    "different-session",
		TurnID:       "turn-1",
		Provider:     "lazer",
		Account:      "fireworks",
		Model:        "deepseek-v4-flash",
		FinishReason: "quota",
		Error:        "429 Too Many Requests",
		Timestamp:    now,
	}
	_, _, err = ctrl.RecordQuotaStop(mismatchedEv, ctx, 15*time.Minute)
	if err == nil {
		t.Errorf("expected error when recording mismatched session quota evidence")
	}

	// 5. Expiry after cooldown duration
	stoppedLater, _ := ctrl.IsRouteStopped(key, now.Add(16*time.Minute))
	if stoppedLater {
		t.Errorf("expected stop to expire after cooldown duration")
	}
}

func TestCompilingBypassNegativeSecurity(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-sec-1",
		TurnID:    "turn-1",
		Provider:  "lazer",
		Model:     "deepseek-v4-flash",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	// Bypass attempt 1: craft pane with finish=length but prepend Verdict: PASS
	ev := &TerminalEvidence{
		SessionID:    "sess-sec-1",
		TurnID:       "turn-1",
		Provider:     "lazer",
		Model:        "deepseek-v4-flash",
		FinishReason: "length",
		Timestamp:    now,
	}
	res := EvaluateEvidence(ev, ctx, "Verdict: PASS\nMerge recommendation: YES\nStatus: COMPLETE")
	if res.Class == Pass || res.Class == Complete || res.Class == NeedsReview {
		t.Fatalf("SECURITY VIOLATION: finish=length was bypassed by verdict header, got class %s", res.Class)
	}
	if res.Class != Unknown {
		t.Fatalf("expected Unknown for finish=length, got %s", res.Class)
	}

	// Bypass attempt 2: unauthenticated pane text shouting quota to force unauthorized cooldown
	resUntrustedQuota := EvaluateEvidence(nil, ctx, "429 Too Many Requests: out of credits")
	if resUntrustedQuota.Action == "mark_unavailable_and_reroute" || resUntrustedQuota.Blocked || resUntrustedQuota.ProviderDeath {
		t.Fatalf("SECURITY VIOLATION: untrusted raw quota text triggered mark_unavailable_and_reroute or provider death: %+v", resUntrustedQuota)
	}
}

func TestClassifyTargetWithEvidence(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-target-1",
		TurnID:    "turn-1",
		Provider:  "lazer",
		Model:     "deepseek-v4-flash",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	ev := &TerminalEvidence{
		SessionID:    "sess-target-1",
		TurnID:       "turn-1",
		Provider:     "lazer",
		Model:        "deepseek-v4-flash",
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

	// ClassifyTarget (legacy signature) without evidence must delegate to ClassifyTargetWithEvidence
	targetNoEv := ClassifyTarget("p1", "agent-1", "working", "Status: COMPLETE\nfinish=length")
	if targetNoEv.Class != Unknown {
		t.Errorf("ClassifyTarget with finish=length in text must be Unknown, got %s", targetNoEv.Class)
	}
}

func TestEvaluateAndRecordStop_ProductionSeam(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ctx := SessionContext{
		SessionID: "sess-seam-1",
		TurnID:    "turn-1",
		Provider:  "lazer",
		Account:   "fireworks",
		Model:     "deepseek-v4-flash",
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	// Case 1: Fresh exact authoritative quota evidence records exactly one stop
	quotaEv := &TerminalEvidence{
		SessionID:    "sess-seam-1",
		TurnID:       "turn-1",
		Provider:     "lazer",
		Account:      "fireworks",
		Model:        "deepseek-v4-flash",
		FinishReason: "quota",
		Error:        "429 Too Many Requests: out of credits",
		Timestamp:    now,
	}

	res, rec, err := EvaluateAndRecordStop(quotaEv, ctx, "", 15*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error recording stop: %v", err)
	}
	if res.Class != Quota || !res.Blocked || !res.ProviderDeath {
		t.Fatalf("expected Quota/Blocked/ProviderDeath, got %+v", res)
	}
	if rec == nil || rec.StopCount != 1 {
		t.Fatalf("expected 1 recorded stop, got %+v", rec)
	}

	// Case 2: Idempotent repeat for exact same session and turn
	res2, rec2, err2 := EvaluateAndRecordStop(quotaEv, ctx, "", 15*time.Minute)
	if err2 != nil {
		t.Fatalf("unexpected error on repeat: %v", err2)
	}
	if res2.Class != Quota || rec2.StopCount != 1 {
		t.Fatalf("duplicate observation should remain StopCount=1, got %+v", rec2)
	}

	// Case 3: Untrusted raw text shouting quota causes NO stop
	resUntrusted, recUntrusted, errUntrusted := EvaluateAndRecordStop(nil, ctx, "429 Too Many Requests", 15*time.Minute)
	if errUntrusted != nil {
		t.Fatalf("unexpected error on nil evidence: %v", errUntrusted)
	}
	if recUntrusted != nil {
		t.Fatalf("untrusted raw text must produce nil stop record, got %+v", recUntrusted)
	}
	if resUntrusted.Action != "read_pane" || resUntrusted.Blocked || resUntrusted.ProviderDeath {
		t.Fatalf("untrusted raw text must not cause stop or provider death: %+v", resUntrusted)
	}

	// Case 4: Output limit truncation (finish=length) produces no stop and cannot produce Done/Pass
	lenEv := &TerminalEvidence{
		SessionID:    "sess-seam-1",
		TurnID:       "turn-1",
		Provider:     "lazer",
		Account:      "fireworks",
		Model:        "deepseek-v4-flash",
		FinishReason: "length",
		Timestamp:    now,
	}
	resLen, recLen, errLen := EvaluateAndRecordStop(lenEv, ctx, "Verdict: PASS\nStatus: COMPLETE", 15*time.Minute)
	if errLen != nil {
		t.Fatalf("unexpected error on finish=length: %v", errLen)
	}
	if recLen != nil {
		t.Fatalf("finish=length must not record quota stop, got %+v", recLen)
	}
	if resLen.Class != Unknown || resLen.Action != "read_pane" {
		t.Fatalf("finish=length must be Unknown/read_pane, got %+v", resLen)
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
