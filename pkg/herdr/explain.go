package herdr

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AgentExplain is the structured detection evidence returned by Herdr's
// `agent explain --json` command.
type AgentExplain struct {
	State          string `json:"state,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`
	Warning        string `json:"warning,omitempty"`
	VisibleBlocker bool   `json:"visible_blocker,omitempty"`
	VisibleIdle    bool   `json:"visible_idle,omitempty"`
	VisibleWorking bool   `json:"visible_working,omitempty"`
	MatchedRule    struct {
		ID    string `json:"id,omitempty"`
		State string `json:"state,omitempty"`
	} `json:"matched_rule,omitempty"`
}

// ExplainAgent returns Herdr's detector evidence for one exact agent target.
func ExplainAgent(target string) (AgentExplain, error) {
	if strings.TrimSpace(target) == "" {
		return AgentExplain{}, fmt.Errorf("herdr agent explain: target is required")
	}
	out, err := runHerdr("agent", "explain", "--json", target)
	if err != nil {
		return AgentExplain{}, fmt.Errorf("herdr agent explain %s: %w", target, err)
	}
	var explanation AgentExplain
	if err := json.Unmarshal([]byte(out), &explanation); err != nil {
		return AgentExplain{}, fmt.Errorf("parsing herdr agent explain %s: %w", target, err)
	}
	return explanation, nil
}

// DetectContextWarning extracts explicit provider/request-size warnings from
// pane text. Ordinary token counts are not treated as warnings.
func DetectContextWarning(body string) string {
	lower := strings.ToLower(body)
	markers := []string{"context window", "context limit", "context length", "prompt is too long", "request too large", "maximum context", "token limit", "too many tokens", "input is too long", "payload too large"}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			for _, line := range strings.Split(body, "\n") {
				if strings.Contains(strings.ToLower(line), marker) {
					return strings.TrimSpace(line)
				}
			}
			return marker
		}
	}
	return ""
}
