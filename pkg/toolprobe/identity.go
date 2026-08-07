package toolprobe

import (
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/router"
)

// SchemaVersion is the receipt envelope version. Bump when identity or
// artifact proof semantics change so old cache entries cannot admit launches.
const SchemaVersion = 1

// RecipeArtifactWrite is the harmless file-write probe recipe.
const RecipeArtifactWrite = "artifact-write-v1"

// ToolchainV1 is the probe toolchain tag bound into every identity.
const ToolchainV1 = "herd-toolprobe-v1"

// Identity is the exact surface tuple a probe receipt proves. A PASS for one
// identity must never authorize a different provider, model, harness, recipe,
// toolchain, task, or lease generation.
type Identity struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	Harness         string `json:"harness"`
	Recipe          string `json:"recipe"`
	Toolchain       string `json:"toolchain"`
	TaskRef         string `json:"task_ref,omitempty"`
	LeaseGeneration int64  `json:"lease_generation,omitempty"`
}

// Key is the stable cache key (provider|model|harness|recipe|toolchain).
// Task/lease are not part of the cache key so a healthy surface can be reused
// across tasks within the TTL; Admit re-binds task/lease into the launch receipt.
func (id Identity) Key() string {
	return strings.Join([]string{
		norm(id.Provider),
		norm(id.Model),
		norm(id.Harness),
		norm(id.Recipe),
		norm(id.Toolchain),
	}, "|")
}

// Valid requires every surface field; task/lease are optional cache fields.
func (id Identity) Valid() error {
	if strings.TrimSpace(id.Provider) == "" || strings.TrimSpace(id.Model) == "" ||
		strings.TrimSpace(id.Harness) == "" || strings.TrimSpace(id.Recipe) == "" ||
		strings.TrimSpace(id.Toolchain) == "" {
		return fmt.Errorf("toolprobe identity incomplete: provider/model/harness/recipe/toolchain required")
	}
	return nil
}

// Matches reports exact surface equality (not task/lease).
func (id Identity) Matches(other Identity) bool {
	return norm(id.Provider) == norm(other.Provider) &&
		norm(id.Model) == norm(other.Model) &&
		norm(id.Harness) == norm(other.Harness) &&
		norm(id.Recipe) == norm(other.Recipe) &&
		norm(id.Toolchain) == norm(other.Toolchain)
}

// IdentityFromDecision builds the probe identity for a routed LaunchDecision.
// Harness falls back to the router default when unset (should not happen after Decide).
func IdentityFromDecision(d *router.LaunchDecision) (Identity, error) {
	if d == nil {
		return Identity{}, fmt.Errorf("toolprobe: LaunchDecision is required")
	}
	harness := strings.TrimSpace(d.Harness)
	if harness == "" {
		harness = router.PiHarness
	}
	id := Identity{
		Provider:  d.Provider,
		Model:     d.Model,
		Harness:   harness,
		Recipe:    RecipeArtifactWrite,
		Toolchain: ToolchainV1,
		TaskRef:   d.TaskRef,
	}
	if d.LeaseGeneration > 0 {
		id.LeaseGeneration = d.LeaseGeneration
	}
	if err := id.Valid(); err != nil {
		return Identity{}, err
	}
	return id, nil
}

func norm(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
