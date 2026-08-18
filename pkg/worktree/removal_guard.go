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
// When the claims database exists, every removal must prove that no active,
// unexpired lease names the target path. Relative paths in old rows are
// normalized against root before comparison.
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
	claims, err := store.ActiveClaims(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("worktree removal fence: read active leases: %w", err)
	}
	want, err := removalPath(root, target)
	if err != nil {
		return err
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
	return nil
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
