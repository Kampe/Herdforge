package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PrepareRuntimeDefaults resolves the repo-local claim fence for a normal
// single-repo invocation. Fleet deployments may still provide HERD_CLAIM_DIR
// and HERD_FENCE_VOLUME_ID explicitly; local users should not have to copy a
// SQLite seal into every shell before running herd.
func PrepareRuntimeDefaults(startDir string) error {
	if strings.TrimSpace(os.Getenv("HERD_CLAIM_DIR")) == "" {
		dir, err := CanonicalClaimDir(startDir, "")
		if err != nil {
			return err
		}
		if err := os.Setenv("HERD_CLAIM_DIR", dir); err != nil {
			return fmt.Errorf("provider: set HERD_CLAIM_DIR: %w", err)
		}
	}
	if strings.TrimSpace(os.Getenv("HERD_FENCE_VOLUME_ID")) != "" {
		return nil
	}
	dir := strings.TrimSpace(os.Getenv("HERD_CLAIM_DIR"))
	seal, err := readDBVolumeSeal(filepath.Join(dir, fencesDBLeaf))
	if err != nil {
		return fmt.Errorf("provider: resolve local fence seal: %w", err)
	}
	if seal == "" {
		return fmt.Errorf("provider: local fence store is not sealed; run herd fence-provision once")
	}
	if err := os.Setenv("HERD_FENCE_VOLUME_ID", seal); err != nil {
		return fmt.Errorf("provider: set HERD_FENCE_VOLUME_ID: %w", err)
	}
	return nil
}
