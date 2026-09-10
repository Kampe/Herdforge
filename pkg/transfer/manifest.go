package transfer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RetentionManifest is the explicit coordinator-owned authority that decides
// which transfer bundles a reclaim pass may consider. Repository location and
// a `.bundle` suffix never prove ownership: only a manifest signed into the
// owned root by the coordinator admits a bundle. Receipts, matrices, logs,
// and unique source bundles are never manifest-eligible (the manifest lists
// exact `.bundle` filenames only).
type RetentionManifest struct {
	Version   int      `json:"version"`
	Authority string   `json:"authority"`
	Bundles   []string `json:"bundles"`
}

// LoadRetentionManifest reads and validates the coordinator's manifest. The
// manifest file must be a regular non-symlink file inside the owned root.
func LoadRetentionManifest(root, manifestPath string) (RetentionManifest, error) {
	var m RetentionManifest
	if strings.TrimSpace(manifestPath) == "" {
		return m, fmt.Errorf("bundle reclaim: explicit retention manifest is required; path suffix and .bundle name never prove ownership")
	}
	absManifest, err := filepath.EvalSymlinks(manifestPath)
	if err != nil {
		return m, fmt.Errorf("bundle reclaim: retention manifest: %w", err)
	}
	absRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return m, fmt.Errorf("bundle reclaim: owned root: %w", err)
	}
	rel, err := filepath.Rel(absRoot, absManifest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return m, fmt.Errorf("bundle reclaim: retention manifest %s is not inside the owned root", manifestPath)
	}
	st, err := os.Lstat(manifestPath)
	if err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
		return m, fmt.Errorf("bundle reclaim: retention manifest must be a regular non-symlink file")
	}
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return m, fmt.Errorf("bundle reclaim: retention manifest: %w", err)
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("bundle reclaim: retention manifest malformed: %w", err)
	}
	if m.Version != 1 {
		return m, fmt.Errorf("bundle reclaim: retention manifest version must be 1, got %d", m.Version)
	}
	if strings.TrimSpace(m.Authority) == "" {
		return m, fmt.Errorf("bundle reclaim: retention manifest must name its coordinator authority")
	}
	if len(m.Bundles) == 0 {
		return m, fmt.Errorf("bundle reclaim: retention manifest lists no bundles")
	}
	seen := make(map[string]bool, len(m.Bundles))
	for _, name := range m.Bundles {
		if !strings.HasSuffix(name, bundleSuffix) || strings.Contains(name, "/") || strings.Contains(name, "\\") || name != filepath.Clean(name) {
			return m, fmt.Errorf("bundle reclaim: retention manifest entry %q must be an exact .bundle filename", name)
		}
		if seen[name] {
			return m, fmt.Errorf("bundle reclaim: retention manifest duplicates %q", name)
		}
		seen[name] = true
	}
	return m, nil
}
