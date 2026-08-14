package confinement

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SentinelRelPath is the repository-relative sentinel file installed inside
// every authenticated task worktree (never under the shared root).
const SentinelRelPath = ".herd/worktree-sentinel"

// SharedRootResidualArtifactRel is the documented boundary marker that must
// remain absent under the shared coordinator checkout. It is deliberately
// ticket-neutral so the production confinement contract does not depend on a
// historical task artifact.
const SharedRootResidualArtifactRel = ".herd/residual-artifact"

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

// CheckSharedRootResidual is a read-only check that the documented residual
// artifact boundary is absent. It does not digest coordinator .herd state
// (launch-claims WAL, mail locks) — that caused false confinement_rejected
// under concurrent launches. The only live shared-root safety signal is
// residual artifact absence.
func CheckSharedRootResidual(sharedRoot string) error {
	if strings.TrimSpace(sharedRoot) == "" {
		return nil
	}
	root, err := filepath.Abs(filepath.Clean(sharedRoot))
	if err != nil {
		return fmt.Errorf("confinement: shared root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("confinement: shared root unreadable: %w", err)
	}
	incident := filepath.Join(root, filepath.FromSlash(SharedRootResidualArtifactRel))
	if _, err := os.Lstat(incident); err == nil {
		return fmt.Errorf("%w: shared-root residual artifact already present: %s", ErrOutsideRoot, SharedRootResidualArtifactRel)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
