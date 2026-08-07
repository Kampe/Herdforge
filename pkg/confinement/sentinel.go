package confinement

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SentinelRelPath is the repository-relative sentinel file installed inside
// every authenticated task worktree (never under the shared root).
const SentinelRelPath = ".herd/worktree-sentinel"

// SharedRootIncidentRel is the FAC-188 incident-shaped path that must remain
// absent under the shared coordinator checkout.
const SharedRootIncidentRel = ".herd/FAC-188-R2-RESIDUAL.md"

// InstallSentinel writes the exact sentinel bytes under the worktree if missing
// or verifies an existing sentinel. It never overwrites a divergent file and
// never touches the shared root.
func InstallSentinel(worktreeRoot string) (string, error) {
	root, err := filepath.Abs(filepath.Clean(worktreeRoot))
	if err != nil {
		return "", fmt.Errorf("confinement: sentinel root: %w", err)
	}
	path := filepath.Join(root, filepath.FromSlash(SentinelRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("confinement: sentinel dir: %w", err)
	}
	if data, err := os.ReadFile(path); err == nil {
		if string(data) != sentinelContents {
			return "", ErrInvalidSentinel
		}
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.WriteFile(path, []byte(sentinelContents), 0o600); err != nil {
		return "", fmt.Errorf("confinement: write sentinel: %w", err)
	}
	return path, nil
}

// ObserveSharedRoot is a read-only snapshot of shared-root safety. It never
// creates files or directories under sharedRoot (FAC-190 round-3 finding #3).
// Digest covers: absolute path, whether .herd exists, and incident-path absence.
// Presence of the FAC-188 residual path fails closed.
func ObserveSharedRoot(sharedRoot string) (digest string, err error) {
	if strings.TrimSpace(sharedRoot) == "" {
		return "", nil
	}
	root, err := filepath.Abs(filepath.Clean(sharedRoot))
	if err != nil {
		return "", fmt.Errorf("confinement: shared root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("confinement: shared root unreadable: %w", err)
	}
	incident := filepath.Join(root, filepath.FromSlash(SharedRootIncidentRel))
	if _, err := os.Lstat(incident); err == nil {
		return "", fmt.Errorf("%w: shared-root incident path already present: %s", ErrOutsideRoot, SharedRootIncidentRel)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	herdDir := filepath.Join(root, ".herd")
	herdState := "absent"
	if st, err := os.Stat(herdDir); err == nil && st.IsDir() {
		herdState = "present"
		// List names only — no writes. Detect new top-level .herd children later.
		entries, err := os.ReadDir(herdDir)
		if err != nil {
			return "", err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		herdState = "present:" + strings.Join(names, ",")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	sum := sha256.Sum256([]byte(root + "\x00" + herdState + "\x00incident-absent"))
	return hex.EncodeToString(sum[:]), nil
}

// CheckSharedRootObservation re-runs ObserveSharedRoot and requires the same
// digest. Any shared-root .herd mutation or incident file creation fails closed.
func CheckSharedRootObservation(sharedRoot, wantDigest string) error {
	if strings.TrimSpace(sharedRoot) == "" {
		return nil
	}
	got, err := ObserveSharedRoot(sharedRoot)
	if err != nil {
		return err
	}
	if wantDigest != "" && !strings.EqualFold(got, wantDigest) {
		return fmt.Errorf("%w: shared-root observation digest drift", ErrInvalidSentinel)
	}
	return nil
}
