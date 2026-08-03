package provider

import (
	"fmt"
	"strings"
	"time"
)

// TaskConfig is the production config surface for building a board provider.
// Mirrors config.TaskProvider fields used at activation (FAC-150).
type TaskConfig struct {
	Type      string
	APIURL    string
	ProjectID string
	UseCLI    bool
	// APIKey for HTTP bulk graph fan-out (even when UseCLI is true).
	APIKey string
	// APIKeyTrustedOrigin is operator-controlled (KANEO_API_URL or selected
	// profile origin). It must never be inferred from repository APIURL.
	APIKeyTrustedOrigin string
	// Optional resolved deadline parts (0 = package default).
	Get, List, Mutate, Comment, Readback time.Duration
}

// NewProductionProvider builds the live TaskProvider for herd/daemon/dispatch.
// Each live provider requires explicit credentials; callers that need in-process
// tests use NewMemoryProvider / NewBoundClient directly.
func NewProductionProvider(tc TaskConfig) (TaskProvider, error) {
	dls := DeadlinesFromParts(tc.Get, tc.List, tc.Mutate, tc.Comment, tc.Readback)
	switch tc.Type {
	case "kaneo":
		k := NewKaneoProvider(tc.APIURL, tc.ProjectID, tc.UseCLI)
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
	case "memory":
		// Explicit test/dev type — still bound so timeouts classify uniformly.
		return NewBoundClient(NewMemoryProvider(), dls), nil
	case "":

		return nil, fmt.Errorf("task_provider.type is required")
	default:
		return nil, fmt.Errorf("task_provider.type %q is not activated in this build", tc.Type)
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
