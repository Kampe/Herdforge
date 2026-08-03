package provider

import (
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
)

// NewFromHerdConfig activates the configured task provider with FAC-150
// deadlines. Only Kaneo (and explicit memory) are activated; other types
// error out for FAC-155.
func NewFromHerdConfig(cfg *config.Config) (TaskProvider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	g, l, m, c, r, err := cfg.TaskProvider.Deadlines.Resolved()
	if err != nil {
		return nil, err
	}
	apiKey := ""
	if env := strings.TrimSpace(cfg.TaskProvider.APIKeyEnv); env != "" {
		apiKey = strings.TrimSpace(os.Getenv(env))
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("KANEO_API_KEY"))
	}
	trustedOrigin := ""
	if apiKey != "" {
		trustedOrigin = resolveOperatorTrustedOrigin()
	}
	return NewProductionProvider(TaskConfig{
		Type:                cfg.TaskProvider.Type,
		APIURL:              cfg.TaskProvider.APIURL,
		ProjectID:           cfg.TaskProvider.ProjectID,
		UseCLI:              cfg.TaskProvider.UseCLI,
		APIKey:              apiKey,
		APIKeyTrustedOrigin: trustedOrigin,
		Get:                 g,
		List:                l,
		Mutate:              m,
		Comment:             c,
		Readback:            r,
	})
}
