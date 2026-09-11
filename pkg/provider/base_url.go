package provider

import (
	"fmt"
	"net/url"
	"strings"
)

// validateProviderBaseURL bounds a repository-supplied provider origin before
// any credential is sent to it.
//
// Linear's endpoint is fixed and this repository deliberately refuses a
// configured api_url there, because a repository-controlled URL is a
// credential-exfiltration path. A per-tenant provider like Jira cannot work
// that way — the site IS per-operator — so the origin is accepted but
// constrained instead of trusted: https only, a real host, no embedded
// userinfo (which could retarget the Basic-auth header), no query or fragment,
// and no trailing slash so path joins stay exact.
func validateProviderBaseURL(providerType, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("task_provider.api_url is required for %s", providerType)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("task_provider.api_url for %s is not a valid URL: %w", providerType, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("task_provider.api_url for %s must be https, got %q", providerType, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("task_provider.api_url for %s has no host", providerType)
	}
	if u.User != nil {
		return "", fmt.Errorf("task_provider.api_url for %s must not embed credentials", providerType)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("task_provider.api_url for %s must be an origin, not a query or fragment", providerType)
	}
	return strings.TrimSuffix(trimmed, "/"), nil
}
