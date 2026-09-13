package provider

import (
	"fmt"
	"strings"
	"time"
)

// TaskConfig is the production config surface for building a board provider.
// Mirrors config.TaskProvider fields used at activation (FAC-150).
type TaskConfig struct {
	Type          string
	APIURL        string
	ProjectID     string
	UseCLI        bool
	CoreTaskReads bool
	// APIKey for HTTP bulk graph fan-out (even when UseCLI is true).
	APIKey string
	// UserEmail is the account half of a Basic-auth credential (Jira).
	UserEmail string
	// APIKeyTrustedOrigin is operator-controlled (KANEO_API_URL or selected
	// profile origin). It must never be inferred from repository APIURL.
	APIKeyTrustedOrigin string
	// Optional resolved deadline parts (0 = package default).
	Get, List, Mutate, Comment, Readback time.Duration
	// Enabled is the repository's explicit activation policy (FAC-155): the
	// only provider types this repository may activate. Empty means "exactly
	// the declared Type" — never a discovered or inherited default. A Type
	// outside a non-empty Enabled list is a hard error, so editing
	// task_provider.type without also moving the operator policy fails closed
	// instead of silently pointing the fleet at a different board.
	Enabled []string
}

// checkEnabled enforces the activation policy for a normalized provider type.
func checkEnabled(providerType string, enabled []string) error {
	if len(enabled) == 0 {
		// Self-only: the declared type is the policy. No fallback, no probe.
		return nil
	}
	for _, e := range enabled {
		if strings.ToLower(strings.TrimSpace(e)) == providerType {
			return nil
		}
	}
	return fmt.Errorf("task_provider.type %q is not in task_provider.enabled %v for this repository", providerType, enabled)
}

// NewProductionProvider builds the live TaskProvider for herd/daemon/dispatch.
// Each live provider requires explicit credentials; callers that need in-process
// tests use NewMemoryProvider / NewBoundClient directly.
func NewProductionProvider(tc TaskConfig) (TaskProvider, error) {
	// Normalize here, not only in NewFromHerdConfig: this is the single
	// activation choke point, so a direct caller passing "Kaneo" must resolve
	// to the same adapter (and the same policy check) as the config path.
	providerType := strings.ToLower(strings.TrimSpace(tc.Type))
	if providerType == "" {
		return nil, fmt.Errorf("task_provider.type is required")
	}
	if err := checkEnabled(providerType, tc.Enabled); err != nil {
		return nil, err
	}
	if tc.CoreTaskReads && (providerType != "kaneo" || !tc.UseCLI) {
		return nil, fmt.Errorf("core_task_reads requires Kaneo CLI transport")
	}
	dls := DeadlinesFromParts(tc.Get, tc.List, tc.Mutate, tc.Comment, tc.Readback)
	switch providerType {
	case "kaneo":
		k := NewKaneoProvider(tc.APIURL, tc.ProjectID, tc.UseCLI)
		k.CoreTaskReads = tc.CoreTaskReads
		if tc.APIKey != "" {
			k.APIKey = tc.APIKey
			// Always assign, including empty, so a direct/custom key cannot inherit
			// the trust origin resolved for different ambient key material.
			k.KeyTrustedOrigin = tc.APIKeyTrustedOrigin
		}
		ApplyDeadlines(k, dls)
		return NewBoundClient(k, dls), nil
	case "linear":
		if strings.TrimSpace(tc.APIKey) == "" {
			return nil, fmt.Errorf("task_provider.api_key_env is required for linear")
		}
		projectID := strings.TrimSpace(tc.ProjectID)
		if projectID == "" {
			return nil, fmt.Errorf("linear task_provider.project_id is required")
		}
		// Linear's endpoint is fixed. Never accept repository-controlled api_url
		// here: doing so would send the operator's Linear credential to that URL.
		l := NewLinearProvider(strings.TrimSpace(tc.APIKey))
		l.ProjectID = projectID
		ApplyDeadlines(l, dls)
		return NewBoundClient(l, dls), nil
	case "jira":
		// Jira is per-tenant, so unlike Linear the site origin MUST come from
		// config. That makes it the URL the operator credential is sent to, so
		// it is validated here rather than trusted: https only, a real host, and
		// no embedded userinfo that could redirect the Basic-auth header.
		baseURL, err := validateProviderBaseURL("jira", tc.APIURL)
		if err != nil {
			return nil, err
		}
		email := strings.TrimSpace(tc.UserEmail)
		if email == "" {
			return nil, fmt.Errorf("task_provider.user_email is required for jira")
		}
		if strings.TrimSpace(tc.APIKey) == "" {
			return nil, fmt.Errorf("task_provider.api_key_env is required for jira")
		}
		projectKey := strings.TrimSpace(tc.ProjectID)
		if projectKey == "" {
			return nil, fmt.Errorf("jira task_provider.project_id is required")
		}
		jp := NewJiraProvider(baseURL, email, strings.TrimSpace(tc.APIKey))
		jp.ProjectKey = projectKey
		ApplyDeadlines(jp, dls)
		return NewBoundClient(jp, dls), nil
	case "memory":
		// Explicit test/dev type — still bound so timeouts classify uniformly.
		return NewBoundClient(NewMemoryProvider(), dls), nil
	default:
		return nil, fmt.Errorf("task_provider.type %q is not activated in this build", providerType)
	}
}

// MustProductionProvider is for tests; panics on error.
func MustProductionProvider(tc TaskConfig) TaskProvider {
	tp, err := NewProductionProvider(tc)
	if err != nil {
		panic(err)
	}
	return tp
}
