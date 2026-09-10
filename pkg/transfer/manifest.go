package transfer

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RetentionEntry binds one bundle filename to its exact content identity.
// The deletion proof for that bundle must reproduce this identity under the
// native lock immediately before unlink: same size, same modification time,
// and a streaming SHA-256 of the full content equal to Digest.
type RetentionEntry struct {
	Name            string `json:"name"`
	Digest          string `json:"digest"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
}

// RetentionManifest is the explicit coordinator-owned authority that decides
// which transfer bundles a reclaim pass may consider. Repository location and
// a `.bundle` suffix never prove ownership: only a manifest written into the
// owned root by the coordinator admits a bundle, and only with the bundle's
// exact content identity pinned — a name alone never authorizes deletion.
// Receipts, matrices, logs, and unique source bundles are never
// manifest-eligible (the manifest lists exact `.bundle` filenames only).
type RetentionManifest struct {
	Version   int              `json:"version"`
	Authority string           `json:"authority"`
	Bundles   []RetentionEntry `json:"bundles"`
}

// LoadRetentionManifest reads and validates the coordinator's manifest. The
// manifest file must be a regular non-symlink file inside the owned root,
// and every admitted entry must pin full content identity.
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
	if interleaveHook != nil {
		interleaveHook("manifest-pre-read")
	}
	body, err := readBoundFile(manifestPath, st)
	if err != nil {
		return m, fmt.Errorf("bundle reclaim: retention manifest: %w", err)
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("bundle reclaim: retention manifest malformed: %w", err)
	}
	if m.Version != 2 {
		return m, fmt.Errorf("bundle reclaim: retention manifest version must be 2 (content-identity bound), got %d", m.Version)
	}
	if strings.TrimSpace(m.Authority) == "" {
		return m, fmt.Errorf("bundle reclaim: retention manifest must name its coordinator authority")
	}
	if len(m.Bundles) == 0 {
		return m, fmt.Errorf("bundle reclaim: retention manifest lists no bundles")
	}
	seen := make(map[string]bool, len(m.Bundles))
	for _, entry := range m.Bundles {
		name := entry.Name
		if !strings.HasSuffix(name, bundleSuffix) || strings.Contains(name, "/") || strings.Contains(name, "\\") || name != filepath.Clean(name) {
			return m, fmt.Errorf("bundle reclaim: retention manifest entry %q must be an exact .bundle filename", name)
		}
		if seen[name] {
			return m, fmt.Errorf("bundle reclaim: retention manifest duplicates %q", name)
		}
		seen[name] = true
		if !validContentDigest(entry.Digest) {
			return m, fmt.Errorf("bundle reclaim: retention manifest entry %q must pin a 64-hex sha256 content digest", name)
		}
		if entry.Size <= 0 {
			return m, fmt.Errorf("bundle reclaim: retention manifest entry %q must pin a positive size", name)
		}
		if entry.ModTimeUnixNano <= 0 {
			return m, fmt.Errorf("bundle reclaim: retention manifest entry %q must pin a modification time", name)
		}
	}
	return m, nil
}

func validContentDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, r := range digest {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func manifestEntry(m RetentionManifest, name string) (RetentionEntry, bool) {
	for _, entry := range m.Bundles {
		if entry.Name == name {
			return entry, true
		}
	}
	return RetentionEntry{}, false
}

// interleaveHook, when non-nil, fires between manifest path validation and
// the content read. It is a deterministic test seam for interleaving
// reproduction (bundle-interleaving-repair-2306); production leaves it nil.
var interleaveHook func(stage string)

// readBoundFile opens path, binds the open descriptor to the inode that was
// validated (same inode, via SameFile), and reads the content from that
// descriptor. Bytes can therefore never come from a replacement installed
// between path validation and the read: a swapped inode is refused instead
// of being parsed as manifest authority.
func readBoundFile(path string, validated os.FileInfo) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bound, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(validated, bound) {
		return nil, fmt.Errorf("retention manifest replaced between validation and read")
	}
	return io.ReadAll(f)
}
