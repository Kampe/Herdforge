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
// every authenticated task worktree.
const SentinelRelPath = ".herd/worktree-sentinel"

// SharedRootSentinelRelPath is the durable dirty-sentinel under the shared
// coordinator checkout. Unexpected mutation after launch is BLOCKED evidence.
const SharedRootSentinelRelPath = ".herd/shared-root-sentinel"

// InstallSentinel writes the exact sentinel bytes under the worktree if missing
// or verifies an existing sentinel. It never overwrites a divergent file.
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

// EnsureSharedRootSentinel installs or verifies the shared-root dirty sentinel.
func EnsureSharedRootSentinel(sharedRoot string) (path string, digest string, err error) {
	root, err := filepath.Abs(filepath.Clean(sharedRoot))
	if err != nil {
		return "", "", fmt.Errorf("confinement: shared root: %w", err)
	}
	path = filepath.Join(root, filepath.FromSlash(SharedRootSentinelRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", "", err
	}
	if data, err := os.ReadFile(path); err == nil {
		if string(data) != sentinelContents {
			return "", "", ErrInvalidSentinel
		}
		sum := sha256.Sum256(data)
		return path, hex.EncodeToString(sum[:]), nil
	} else if !os.IsNotExist(err) {
		return "", "", err
	}
	if err := os.WriteFile(path, []byte(sentinelContents), 0o600); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(sentinelContents))
	return path, hex.EncodeToString(sum[:]), nil
}

// CheckSharedRootSentinel re-reads the shared-root sentinel and fails when the
// byte content or path identity has drifted since Install.
func CheckSharedRootSentinel(sharedRoot, wantDigest string) error {
	root, err := filepath.Abs(filepath.Clean(sharedRoot))
	if err != nil {
		return err
	}
	path := filepath.Join(root, filepath.FromSlash(SharedRootSentinelRelPath))
	data, err := os.ReadFile(path)
	if err != nil {
		return ErrInvalidSentinel
	}
	if string(data) != sentinelContents {
		return ErrInvalidSentinel
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if wantDigest != "" && !strings.EqualFold(got, wantDigest) {
		return fmt.Errorf("%w: shared-root sentinel digest drift", ErrInvalidSentinel)
	}
	return nil
}
