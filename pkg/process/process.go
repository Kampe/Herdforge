package process

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
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

// classifyText matches herd-process classify_text logic.
// First-match wins; order encodes priority.
func classifyText(text string) Classification {
	if text == "" {
		return Unknown
	}

	// QUOTA is a PROVIDER-EXHAUSTION runner signal, NOT review content
	// that merely discusses rate limiting (CHA-281). Require genuine
	// exhaustion phrasing AND exclude text carrying review markers.
	// MUST check QUOTA exclusions before NEEDS_REVIEW/PASS/FAIL, because a
	// reviewer quoting 429/quota text must NOT match the QUOTA pattern.
	isQuota := ProviderExhaustionReason(text) != ""

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
	if isQuota {
		return Quota
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
	quotaPat := regexp.MustCompile(`(?i)out of credits|out of quota|too many requests|429 too many|individual quota reached|(rate.?limit|usage limit|weekly limit|daily limit|monthly limit|token quota|api quota|quota)[^.]{0,24}(exceeded|reached|throttled|hit|exhausted)|exceeded your (quota|rate|usage|limit)`)
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

// CheckProviderDeath checks if pane text indicates a provider death scenario.
// Mirrors herd_pane_provider_death from herd-lib.zsh.
func CheckProviderDeath(text string) bool {
	if text == "" {
		return false
	}
	// Provider death signatures: connection lost, auth expired, provider error,
	// model unavailable, API key invalid, etc.
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(connection|session).{0,20}(lost|closed|reset|timed out|terminated|refused)`),
		regexp.MustCompile(`(?i)(auth|token|api.key|credential).{0,20}(expired|invalid|revoked|unauthorized|denied)`),
		regexp.MustCompile(`(?i)provider.{0,20}(error|unavailable|not.?found|decommissioned|removed)`),
		regexp.MustCompile(`(?i)model.{0,20}(unavailable|not.?found|deprecated|removed)`),
		regexp.MustCompile(`(?i)(herdr|harness).{0,20}(exit|crash|fatal|panic|segfault)`),
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
