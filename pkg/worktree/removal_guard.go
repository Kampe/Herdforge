package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// RefuseRemovalWithLiveLease is the final coordinator-side removal fence.
// When the claims database exists, every removal must prove BOTH that no
// active, unexpired lease names the target path AND that the path was, at
// some point, actually registered with the lease store. A path with zero
// lease history at all was never dispatched through herd -- most likely a
// manually created `git worktree add`, or something else entirely outside
// herd's tracking -- and this fence has no basis to judge it safe to remove.
// FAC-453: the previous version only checked *active* claims, so "never
// registered" and "legitimately completed and released" were both treated
// as "no active lease, proceed" -- silently unsafe for the former. Relative
// paths in old rows are normalized against root before comparison.
func RefuseRemovalWithLiveLease(ctx context.Context, root, target string) error {
	root = strings.TrimSpace(root)
	target = strings.TrimSpace(target)
	if root == "" || target == "" {
		return errors.New("worktree removal fence: repository root and target are required")
	}
	dbPath := filepath.Join(root, ".herd", "herdforge.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("worktree removal fence: inspect lease database: %w", err)
	}
	store, err := claim.NewSQLiteLeaseStore(dbPath)
	if err != nil {
		return fmt.Errorf("worktree removal fence: open lease database: %w", err)
	}
	defer store.Close()
	want, err := removalPath(root, target)
	if err != nil {
		return err
	}
	claims, err := store.ActiveClaims(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("worktree removal fence: read active leases: %w", err)
	}
	for _, lease := range claims {
		if lease == nil || strings.TrimSpace(lease.WorktreePath) == "" {
			continue
		}
		leased, pathErr := removalPath(root, lease.WorktreePath)
		if pathErr != nil {
			return fmt.Errorf("worktree removal fence: normalize leased path: %w", pathErr)
		}
		if leased == want {
			return fmt.Errorf("worktree removal refused: live lease task=%s generation=%d path=%s", lease.TaskRef, lease.Generation, target)
		}
	}
	known, err := store.DistinctWorktreePaths(ctx)
	if err != nil {
		return fmt.Errorf("worktree removal fence: read lease history: %w", err)
	}
	for _, path := range known {
		if strings.TrimSpace(path) == "" {
			continue
		}
		recorded, pathErr := removalPath(root, path)
		if pathErr != nil {
			return fmt.Errorf("worktree removal fence: normalize recorded path: %w", pathErr)
		}
		if recorded == want {
			return nil
		}
	}
	return fmt.Errorf("worktree removal refused: %q has no lease history in this repository's claim store (unregistered worktree, never dispatched through herd)", target)
}

func removalPath(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("worktree removal fence: resolve path: %w", err)
	}
	return filepath.Clean(path), nil
}
