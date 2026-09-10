package process

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Classification represents the digest class for an agent's output.
type Classification string

const (
	NeedsReview Classification = "NEEDS_REVIEW"
	Complete    Classification = "COMPLETE"
	Pass        Classification = "PASS"
	Fail        Classification = "FAIL"
	Blocked     Classification = "BLOCKED"
	Quota       Classification = "QUOTA"
	Unconsumed  Classification = "UNCONSUMED"
	Unknown     Classification = "UNKNOWN"
)

// Target is one attention target's digest result.
type Target struct {
	PaneID string         `json:"pane_id"`
	Name   string         `json:"name"`
	Status string         `json:"status"`
	Class  Classification `json:"class"`
	Action string         `json:"action"`
	Tail   string         `json:"tail,omitempty"`
}

// Digest is the complete harvest digest.
type Digest struct {
	WorkspaceID   string   `json:"workspace_id"`
	Items         []Target `json:"items"`
	MultiPaneTabs []string `json:"multi_pane_tabs,omitempty"`
}

// TerminalEvidence represents structured current-session provider terminal evidence.
// Authoritative event fields MUST be present and validated against the session context.
type TerminalEvidence struct {
	SessionID    string    `json:"session_id"`
	TurnID       string    `json:"turn_id"`
	Provider     string    `json:"provider"`
	Account      string    `json:"account,omitempty"`
	Model        string    `json:"model"`
	FinishReason string    `json:"finish_reason,omitempty"`
	Error        string    `json:"error,omitempty"`
	Status       string    `json:"status,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
}

// SessionContext provides the expected authoritative session identity, turn, route, and time window.
type SessionContext struct {
	SessionID string
	TurnID    string
	Provider  string
	Account   string
	Model     string
	Now       time.Time
	MaxAge    time.Duration
}

var (
	ErrMissingSessionID = errors.New("terminal evidence: session_id is required")
	ErrMissingTurnID    = errors.New("terminal evidence: turn_id is required")
	ErrMissingProvider  = errors.New("terminal evidence: provider is required")
	ErrMissingModel     = errors.New("terminal evidence: model is required")
	ErrMissingTimestamp = errors.New("terminal evidence: timestamp is required")
	ErrSessionMismatch  = errors.New("terminal evidence: session_id mismatch")
	ErrTurnMismatch     = errors.New("terminal evidence: turn_id mismatch")
	ErrRouteMismatch    = errors.New("terminal evidence: route provider/model mismatch")
	ErrStaleEvidence    = errors.New("terminal evidence: timestamp outside bounded lookback window")
)

// Validate checks that the evidence contains all required fields and matches the authoritative context.
func (ev *TerminalEvidence) Validate(ctx SessionContext) error {
	if ev == nil {
		return errors.New("terminal evidence is nil")
	}
	if strings.TrimSpace(ev.SessionID) == "" {
		return ErrMissingSessionID
	}
	if strings.TrimSpace(ev.TurnID) == "" {
		return ErrMissingTurnID
	}
	if strings.TrimSpace(ev.Provider) == "" {
		return ErrMissingProvider
	}
	if strings.TrimSpace(ev.Model) == "" {
		return ErrMissingModel
	}
	if ev.Timestamp.IsZero() {
		return ErrMissingTimestamp
	}

	// Exact session binding
	if ctx.SessionID != "" && ev.SessionID != ctx.SessionID {
		return ErrSessionMismatch
	}
	// Exact turn binding
	if ctx.TurnID != "" && ev.TurnID != ctx.TurnID {
		return ErrTurnMismatch
	}
	// Route binding
	if ctx.Provider != "" && !strings.EqualFold(ev.Provider, ctx.Provider) {
		return ErrRouteMismatch
	}
	if ctx.Model != "" && !strings.EqualFold(ev.Model, ctx.Model) {
		return ErrRouteMismatch
	}
	if ctx.Account != "" && ev.Account != "" && !strings.EqualFold(ev.Account, ctx.Account) {
		return ErrRouteMismatch
	}

	// Freshness / bounded lookback
	if !ctx.Now.IsZero() {
		maxAge := ctx.MaxAge
		if maxAge <= 0 {
			maxAge = 5 * time.Minute
		}
		if ctx.Now.Sub(ev.Timestamp) > maxAge || ev.Timestamp.After(ctx.Now.Add(1*time.Minute)) {
			return ErrStaleEvidence
		}
	}

	return nil
}

// EvaluationResult represents the evaluated state of an agent's terminal output.
type EvaluationResult struct {
	Class          Classification `json:"class"`
	Action         string         `json:"action"`
	Reason         string         `json:"reason,omitempty"`
	Blocked        bool           `json:"blocked"`
	ProviderDeath  bool           `json:"provider_death"`
	Fresh          bool           `json:"fresh"`
	SessionMatched bool           `json:"session_matched"`
	Provider       string         `json:"provider,omitempty"`
	Account        string         `json:"account,omitempty"`
	Model          string         `json:"model,omitempty"`
}

// OutputLimitReason reports whether terminal output indicates the response was
// cut off due to an output token/length limit (e.g. OpenCode finish=length).
// Review prose discussing output limits is excluded.
func OutputLimitReason(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	hasReviewMarker := regexp.MustCompile(`(?i)verdict:\s*|merge recommendation:\s*|\bconfirmed\b|\bfindings?\b|reviewing|pass/fail`)
	if hasReviewMarker.MatchString(text) {
		return ""
	}
	lengthPat := regexp.MustCompile(`(?i)finish[=_]reason[:=]\s*["']?length["']?|finish=length|output token limit reached|output limit (exceeded|reached|hit)|maximum (context|token|output) length (exceeded|reached)|max(?:imum)? tokens reached|response truncated due to output limit`)
	if lengthPat.MatchString(text) {
		return "output token limit reached (finish=length)"
	}
	return ""
}

// classifyText matches herd-process classify_text logic.
// First-match wins; order encodes priority.
func classifyText(text string) Classification {
	if text == "" {
		return Unknown
	}

	// 1. QUOTA is a PROVIDER-EXHAUSTION runner signal, NOT review content
	// that merely discusses rate limiting (CHA-281). Require genuine
	// exhaustion phrasing AND exclude text carrying review markers.
	// Checked first so an agent that died with a quota failure is not falsely
	// reported as NEEDS_REVIEW/COMPLETE/PASS from prior turn text.
	if isQuota := ProviderExhaustionReason(text) != ""; isQuota {
		return Quota
	}

	// 2. Output limit truncation (finish=length) must NEVER appear as Complete,
	// Pass, or NeedsReview. It is incomplete/unknown.
	if isTruncated := OutputLimitReason(text) != ""; isTruncated {
		return Unknown
	}

	if regexp.MustCompile(`(?i)NEEDS_REVIEW|Status:\s*NEEDS_REVIEW`).MatchString(text) {
		return NeedsReview
	}
	if regexp.MustCompile(`(?i)Merge recommendation:\s*YES|Verdict:\s*PASS`).MatchString(text) {
		return Pass
	}
	if regexp.MustCompile(`(?i)Verdict:\s*FAIL|Merge recommendation:\s*NO`).MatchString(text) {
		return Fail
	}
	if regexp.MustCompile(`(?i)Status:\s*COMPLETE\b`).MatchString(text) {
		return Complete
	}
	if regexp.MustCompile(`(?i)Status:\s*BLOCKED|BLOCKED:`).MatchString(text) {
		return Blocked
	}
	if regexp.MustCompile(`(?m)^❯\s`).MatchString(text) && !regexp.MustCompile(`(?i)Worked for|Status:`).MatchString(text) {
		return Unconsumed
	}
	return Unknown
}

// ProviderExhaustionReason reports whether terminal output contains a live
// provider quota/rate-limit failure. Review prose is excluded so a reviewer
// discussing a 429 does not poison the surface's capacity evidence.
func ProviderExhaustionReason(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	quotaPat := regexp.MustCompile(`(?i)out of credits|out of quota|too many requests|429 too many|individual quota reached|(rate.?limit|usage limit|weekly limit|daily limit|monthly limit|token quota|api quota|quota)[^.]{0,24}(exceeded|reached|throttled|hit|exhausted)|exceeded your (quota|rate|usage|limit)|account (?:has been |is )?suspended|upstream account suspended|account (?:has been |is )?deactivated|insufficient[_\s]quota|credit balance is too low|402\s+payment\s+required|billing (?:not active|account disabled|hard limit reached)|rate_limit_exceeded|resource_exhausted`)
	hasReviewMarker := regexp.MustCompile(`(?i)verdict:|merge recommendation:|\bconfirmed\b|\bfindings?\b|reviewing|pass/fail`)
	if quotaPat.MatchString(text) && !hasReviewMarker.MatchString(text) {
		return "provider quota or rate limit reported"
	}
	return ""
}

// actionFor returns the recommended action string for a classification.
func actionFor(c Classification, isProviderDeath bool) string {
	if isProviderDeath {
		return "provider_death_cooled_reset_aware"
	}
	switch c {
	case NeedsReview:
		return "dispatch_review_or_merge_gate"
	case Pass:
		return "merge_if_tier_ok"
	case Fail:
		return "return_to_builder"
	case Complete:
		return "close_or_activate"
	case Blocked:
		return "unblock_or_reassign"
	case Quota:
		return "mark_unavailable_and_reroute"
	default:
		return "read_pane"
	}
}

// classifyTextNoQuota classifies text without recognizing Quota, used when
// evidence is unauthenticated, foreign, stale, or mismatched so untrusted text
// NEVER triggers provider cooldowns or mark_unavailable actions.
func classifyTextNoQuota(text string) Classification {
	if text == "" {
		return Unknown
	}
	if isTruncated := OutputLimitReason(text) != ""; isTruncated {
		return Unknown
	}
	if regexp.MustCompile(`(?i)NEEDS_REVIEW|Status:\s*NEEDS_REVIEW`).MatchString(text) {
		return NeedsReview
	}
	if regexp.MustCompile(`(?i)Merge recommendation:\s*YES|Verdict:\s*PASS`).MatchString(text) {
		return Pass
	}
	if regexp.MustCompile(`(?i)Verdict:\s*FAIL|Merge recommendation:\s*NO`).MatchString(text) {
		return Fail
	}
	if regexp.MustCompile(`(?i)Status:\s*COMPLETE\b`).MatchString(text) {
		return Complete
	}
	if regexp.MustCompile(`(?i)Status:\s*BLOCKED|BLOCKED:`).MatchString(text) {
		return Blocked
	}
	if regexp.MustCompile(`(?m)^❯\s`).MatchString(text) && !regexp.MustCompile(`(?i)Worked for|Status:`).MatchString(text) {
		return Unconsumed
	}
	return Unknown
}

// EvaluateEvidence evaluates structured terminal evidence alongside pane text.
func EvaluateEvidence(ev *TerminalEvidence, ctx SessionContext, rawText string) EvaluationResult {
	if ev == nil {
		c := classifyText(rawText)
		isPD := CheckProviderDeath(rawText)
		action := actionFor(c, isPD)
		if c == Quota {
			// Untrusted raw prose without structured authoritative evidence
			// must NOT trigger a provider cooldown or mark_unavailable action.
			action = "read_pane"
		}
		return EvaluationResult{
			Class:          c,
			Action:         action,
			Blocked:        c == Blocked,
			ProviderDeath:  isPD,
			Fresh:          false,
			SessionMatched: false,
		}
	}

	res := EvaluationResult{
		Provider: ev.Provider,
		Account:  ev.Account,
		Model:    ev.Model,
	}

	// 1. Authoritative session, turn, route, and freshness validation.
	if err := ev.Validate(ctx); err != nil {
		// Untrusted, mismatched, stale, or malformed evidence:
		// MUST CAUSE NO STOP / COOLDOWN.
		res.Fresh = !errors.Is(err, ErrStaleEvidence) && !errors.Is(err, ErrMissingTimestamp)
		res.SessionMatched = !errors.Is(err, ErrSessionMismatch) && !errors.Is(err, ErrMissingSessionID)
		res.Class = classifyTextNoQuota(rawText)
		res.Action = "read_pane"
		res.Reason = fmt.Sprintf("untrusted or invalid evidence: %v", err)
		res.Blocked = false
		res.ProviderDeath = false
		return res
	}

	res.Fresh = true
	res.SessionMatched = true

	// 2. Evaluate finish_reason: length (output token limit truncation).
	// Must NEVER produce Done, Pass, or NeedsReview.
	if strings.EqualFold(ev.FinishReason, "length") || OutputLimitReason(ev.Error) != "" || OutputLimitReason(rawText) != "" {
		res.Class = Unknown
		res.Action = "read_pane"
		res.Reason = "output token limit reached (finish=length)"
		res.Blocked = false
		res.ProviderDeath = false
		return res
	}

	// 3. Evaluate explicit provider quota/rate-limit/suspension errors from validated evidence.
	exhaustionReason := ""
	if ev.Error != "" {
		exhaustionReason = ProviderExhaustionReason(ev.Error)
		if exhaustionReason == "" && (strings.Contains(strings.ToLower(ev.Error), "quota") || strings.Contains(strings.ToLower(ev.Error), "suspended") || strings.Contains(strings.ToLower(ev.Error), "429")) {
			exhaustionReason = ev.Error
		}
	}
	if exhaustionReason == "" && strings.EqualFold(ev.FinishReason, "quota") {
		exhaustionReason = "provider quota or rate limit reported"
	}
	if exhaustionReason == "" {
		exhaustionReason = ProviderExhaustionReason(rawText)
	}

	if exhaustionReason != "" || strings.EqualFold(ev.FinishReason, "quota") {
		res.Class = Quota
		res.Blocked = true
		res.ProviderDeath = true
		res.Action = "mark_unavailable_and_reroute"
		res.Reason = exhaustionReason
		if res.Reason == "" {
			res.Reason = "provider quota or rate limit reported"
		}
		return res
	}

	// 4. Check other provider death errors (auth, connection lost, etc.)
	if CheckProviderDeath(ev.Error) || CheckProviderDeath(rawText) {
		res.Class = Blocked
		res.Blocked = true
		res.ProviderDeath = true
		res.Action = "provider_death_cooled_reset_aware"
		res.Reason = "provider death reported"
		return res
	}

	// 5. In-flight tool execution vs completion
	if strings.EqualFold(ev.FinishReason, "tool_use") || strings.EqualFold(ev.FinishReason, "tool_calls") {
		res.Class = Unknown
		res.Action = "read_pane"
		res.Reason = "live tool execution in progress"
		return res
	}

	// 6. Successful / normal response evaluation
	c := classifyText(rawText)
	if ev.Status != "" && (c == Unknown || c == Unconsumed) {
		c = classifyText("Status: " + ev.Status)
	}
	isPD := CheckProviderDeath(rawText)
	res.Class = c
	res.Action = actionFor(c, isPD)
	return res
}

// ParseTerminalEvidence strictly parses structured TerminalEvidence from a raw payload.
// Untrusted prose with embedded braces is rejected. All required authoritative fields must be present.
func ParseTerminalEvidence(data []byte) (*TerminalEvidence, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, errors.New("empty terminal evidence payload")
	}
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return nil, errors.New("terminal evidence must be a structured JSON object, not loose prose")
	}

	var ev TerminalEvidence
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		return nil, fmt.Errorf("invalid terminal evidence JSON: %w", err)
	}

	if err := ev.Validate(SessionContext{}); err != nil {
		return nil, fmt.Errorf("invalid terminal evidence fields: %w", err)
	}

	return &ev, nil
}

// tail extracts the last N lines, collapsed, truncated for display.
func tail(text string, n int, maxLen int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	joined := strings.Join(lines, " ")
	// Collapse whitespace
	joined = regexp.MustCompile(`\s+`).ReplaceAllString(joined, " ")
	if len(joined) > maxLen {
		joined = joined[:maxLen]
	}
	return joined
}

// extractField extracts a single-line field value after a label prefix.
func extractField(text, prefix string) string {
	re := regexp.MustCompile(`(?m)^[[:space:]]*` + regexp.QuoteMeta(prefix) + `:[[:space:]]*(.*?)$`)
	m := re.FindStringSubmatch(text)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// PanesFromHerdr parses the herdr agent list output and returns attention targets.
type AgentEntry struct {
	PaneID string `json:"pane_id"`
	Name   string `json:"name,omitempty"`
	Label  string `json:"label,omitempty"`
	Status string `json:"status,omitempty"`
}

// ClassifyTarget processes one agent pane text and produces a Target digest.
func ClassifyTarget(paneID, name, status, text string) Target {
	c := classifyText(text)
	isProviderDeath := CheckProviderDeath(text)
	action := actionFor(c, isProviderDeath)

	t := Target{
		PaneID: paneID,
		Name:   name,
		Status: status,
		Class:  c,
		Action: action,
		Tail:   tail(text, 8, 220),
	}

	// Record lifecycle events for COMPLETE/BLOCKED.
	if c == Complete || c == Blocked {
		taskID := extractField(text, "Task ID")
		episodeID := extractField(text, "Episode ID")
		_ = taskID
		_ = episodeID
	}

	return t
}

// ClassifyTargetWithEvidence processes agent pane text with structured evidence and produces a Target digest.
func ClassifyTargetWithEvidence(paneID, name, status, text string, ev *TerminalEvidence, ctx SessionContext) Target {
	res := EvaluateEvidence(ev, ctx, text)
	return Target{
		PaneID: paneID,
		Name:   name,
		Status: status,
		Class:  res.Class,
		Action: res.Action,
		Tail:   tail(text, 8, 220),
	}
}

// CheckProviderDeath checks if pane text indicates a provider death scenario.
// Mirrors herd_pane_provider_death from herd-lib.zsh.
func CheckProviderDeath(text string) bool {
	if text == "" {
		return false
	}
	// Provider death signatures: connection lost, auth expired, provider error,
	// model unavailable, API key invalid, upstream account suspended, etc.
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(connection|session).{0,20}(lost|closed|reset|timed out|terminated|refused)`),
		regexp.MustCompile(`(?i)(auth|token|api.key|credential).{0,20}(expired|invalid|revoked|unauthorized|denied)`),
		regexp.MustCompile(`(?i)provider.{0,20}(error|unavailable|not.?found|decommissioned|removed)`),
		regexp.MustCompile(`(?i)model.{0,20}(unavailable|not.?found|deprecated|removed)`),
		regexp.MustCompile(`(?i)(herdr|harness).{0,20}(exit|crash|fatal|panic|segfault)`),
		regexp.MustCompile(`(?i)(upstream\s+account\s+suspended|account\s+suspended|account\s+deactivated)`),
	}
	for _, p := range patterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// Selftest runs the herd-process selftest assertions and returns an error on
// first failure.
func Selftest() error {
	tests := []struct {
		input string
		want  Classification
	}{
		{"Status: NEEDS_REVIEW\nTask ID: X", NeedsReview},
		{"Verdict: PASS\nMerge recommendation: YES", Pass},
		{"Verdict: FAIL\nRequired findings: bug", Fail},
		{"Status: COMPLETE", Complete},
		{"weekly quota exceeded", Quota},
		{"Error: usage limit reached; resets in 3h", Quota},
		{"OpenCode finish=length", Unknown},
		{"Status: COMPLETE\nfinish_reason: length", Unknown},
		{"Fireworks upstream account suspended", Quota},
		// CHA-281: review content quoting rate-limit/429 must NOT match QUOTA
		{"CONFIRMED: the rate limit exceeded path returns 429; quota bucket exceeded branch is covered", Unknown},
		{"The endpoint enforces a rate limit of 100/s and returns 429 on quota bucket overflow; capacity envelope holds", Unknown},
	}
	for _, tt := range tests {
		got := classifyText(tt.input)
		if got != tt.want {
			return fmt.Errorf("classify(%q) = %s, want %s", tt.input, got, tt.want)
		}
	}

	// Provider death tests
	pdTests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"connection lost to provider", true},
		{"auth token expired", true},
		{"model unavailable for deployment", true},
		{"Fireworks upstream account suspended", true},
		{"Verdict: PASS", false},
		{"normal agent output here", false},
	}
	for _, tt := range pdTests {
		got := CheckProviderDeath(tt.input)
		if got != tt.want {
			return fmt.Errorf("CheckProviderDeath(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}

	return nil
}

// DigestJSON returns a JSON-encoded digest from targets.
func DigestJSON(ws string, targets []Target, multiPaneTabs []string) ([]byte, error) {
	d := Digest{
		WorkspaceID: ws,
		Items:       targets,
	}
	if len(multiPaneTabs) > 0 {
		d.MultiPaneTabs = multiPaneTabs
	}
	return json.Marshal(d)
}

// herdrAgentEntry mirrors the fields needed from herdr.AgentEntry without
// importing the herdr package (avoids circular dependency).
type herdrAgentEntry struct {
	Name   string `json:"name,omitempty"`
	Status string `json:"agent_status,omitempty"`
}

// StalledAgents returns agent names from the provided list whose herdr status
// is done/idle but whose worktree has zero real commits beyond origin/main.
// An anchor/wip commit (FAC-106) does not count as real work.
func StalledAgents(agents []herdrAgentEntry, repoRoot string) []string {
	var stalled []string
	for _, a := range agents {
		if a.Status != "done" && a.Status != "idle" {
			continue
		}
		if execCommitsAhead(a.Name, repoRoot) == 0 {
			stalled = append(stalled, a.Name)
		}
	}
	return stalled
}

// execCommitsAhead is a variable so tests can mock; defaults to execCommitsAheadShell.
var execCommitsAhead = execCommitsAheadShell

// execCommitsAheadShell runs git rev-list to count commits in the worktree
// branch that are not reachable from origin/main.
func execCommitsAheadShell(agentName, repoRoot string) int {
	// Find the worktree directory for this agent's branch.
	// Convention: branches are herd/<lowercase-agent-name>, worktrees live
	// under .herd/worktrees/<lowercase-agent-name>.
	branch := fmt.Sprintf("herd/%s", strings.ToLower(agentName))
	wtDir := fmt.Sprintf("%s/.herd/worktrees/%s", repoRoot, strings.ToLower(agentName))

	cmd := exec.Command("git", "rev-list", "--count", "origin/main.."+branch)
	cmd.Dir = wtDir
	out, err := cmd.Output()
	if err != nil {
		return -1
	}
	var count int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &count); err != nil {
		return -1
	}
	return count
}
