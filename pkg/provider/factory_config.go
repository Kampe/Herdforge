package provider

import (
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
)

// NewFromHerdConfig activates the configured task provider with FAC-150
// deadlines. Linear credentials are read only from its configured api_key_env;
// it never falls back to Kaneo's ambient credential.
func NewFromHerdConfig(cfg *config.Config) (TaskProvider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	g, l, m, c, r, err := cfg.TaskProvider.Deadlines.Resolved()
	if err != nil {
		return nil, err
	}

	providerType := strings.ToLower(strings.TrimSpace(cfg.TaskProvider.Type))
	// Policy first: a disabled provider must not even reach credential
	// resolution, let alone a constructor.
	if err := checkEnabled(providerType, cfg.TaskProvider.Enabled); err != nil {
		return nil, err
	}
	apiKey := ""
	configuredAPIURL := strings.TrimSpace(cfg.TaskProvider.APIURL)
	apiURL := configuredAPIURL
	useCLI := cfg.TaskProvider.UseCLI
	trustedOrigin := ""
	switch providerType {
	case "linear":
		env := strings.TrimSpace(cfg.TaskProvider.APIKeyEnv)
		if env == "" {
			return nil, fmt.Errorf("linear task_provider.api_key_env is required")
		}
		apiKey = strings.TrimSpace(os.Getenv(env))
		if apiKey == "" {
			return nil, fmt.Errorf("linear task provider credential is missing from %s", env)
		}
	case "kaneo":
		// The operator-selected Kaneo profile is intentionally external to the
		// repository; use its explicit environment origin when herd.yaml omits
		// one. This preserves a repository-relative config while avoiding an
		// ambient/default HTTP client with an empty origin.
		if apiURL == "" {
			apiURL = strings.TrimSpace(os.Getenv("KANEO_API_URL"))
			// A profile-only Kaneo setup has no repository-controlled endpoint
			// contract. Use the operator CLI, which owns its compatible API route
			// and credential lookup, rather than guessing an HTTP path here.
			if apiURL != "" {
				useCLI = true
			}
		}
		if env := strings.TrimSpace(cfg.TaskProvider.APIKeyEnv); env != "" {
			apiKey = strings.TrimSpace(os.Getenv(env))
		}
		if apiKey == "" {
			apiKey = strings.TrimSpace(os.Getenv("KANEO_API_KEY"))
		}
		if apiKey != "" {
			trustedOrigin = resolveOperatorTrustedOrigin()
		} else {
			// Reuse the authenticated Kaneo CLI profile when its origin matches
			// the independently configured provider origin. Never scan arbitrary
			// profiles or trust the repository URL as an authority.
			profile := ResolveKaneoProfileCred()
			configuredOrigin, originErr := canonicalizeHTTPOrigin(cfg.TaskProvider.APIURL)
			if profile.Key != "" && originErr == nil && profile.TrustedOrigin == configuredOrigin {
				apiKey = profile.Key
				trustedOrigin = profile.TrustedOrigin
			}
		}
	}
	return NewProductionProvider(TaskConfig{
		Type:                providerType,
		APIURL:              apiURL,
		ProjectID:           cfg.TaskProvider.ProjectID,
		UseCLI:              useCLI,
		APIKey:              apiKey,
		APIKeyTrustedOrigin: trustedOrigin,
		Enabled:             cfg.TaskProvider.Enabled,
		Get:                 g,
		List:                l,
		Mutate:              m,
		Comment:             c,
		Readback:            r,
	})
}
