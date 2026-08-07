package security

import (
	"fmt"
	"strings"
)

// ExtractExternalURLs pulls http(s) URLs from free-form provider text for
// policy checks (never followed automatically).
func ExtractExternalURLs(text string) []string {
	var out []string
	fields := strings.Fields(text)
	for _, f := range fields {
		f = strings.Trim(f, "<>()[],.\"'")
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			out = append(out, f)
		}
	}
	return out
}

// ProviderTextBundle concatenates title+description for injection scanning.
func ProviderTextBundle(title, description string) string {
	return title + "\n" + description
}

// RequireControlSecret fails closed when the control-plane MAC secret is absent.
// Used by production launch paths (dispatch CLI, forge) — never optional.
func RequireControlSecret(secret string) error {
	if strings.TrimSpace(secret) == "" {
		return ErrMissingControlSecret
	}
	return nil
}

// DetectProviderAuthorityClaims reports whether free-form text attempts to
// claim merge/reviewer/lifecycle control (for eventing; never grants authority).
func DetectProviderAuthorityClaims(text string) []string {
	lower := strings.ToLower(text)
	var claims []string
	checks := map[string]string{
		"merge to main":     "merge",
		"herd approve":      "merge",
		"grant reviewer":    "reviewer",
		"change your role":  "role",
		"set cwd":           "cwd",
		"cd /":              "cwd",
		"lifecycle":         "lifecycle",
		"mark done":         "lifecycle",
		"board-write":       "board",
	}
	for sub, claim := range checks {
		if strings.Contains(lower, sub) {
			claims = append(claims, claim)
		}
	}
	return claims
}

// AssertNoProviderControlMutation ensures a StructuredTask's control fields
// match the lane/worktree binding and were not copied from provider text.
func AssertNoProviderControlMutation(st *StructuredTask, role, cwd string) error {
	if st == nil {
		return fmt.Errorf("%w: nil structured task", ErrUnknownProvenance)
	}
	if err := st.Validate(); err != nil {
		return err
	}
	if !strings.EqualFold(st.Role, role) {
		return fmt.Errorf("%w: structured role %q != lane role %q", ErrProviderAuthority, st.Role, role)
	}
	if st.CWD != cwd {
		return fmt.Errorf("%w: structured cwd mismatch", ErrProviderAuthority)
	}
	if st.MergeAuthority {
		return fmt.Errorf("%w: merge authority set on structured task", ErrProviderAuthority)
	}
	return nil
}
