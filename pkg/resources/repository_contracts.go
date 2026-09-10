package resources

import (
	"context"
	"errors"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// Repository-wide path contracts used by resource census, dispatch receipts,
// and operator commands. Their literal definition belongs in one place.
const (
	ReviewPoolPathFragment      = gitroot.ReviewPoolPathFragment
	ManagedWorktreePathFragment = gitroot.ManagedWorktreePathFragment
	LegacyWorktreePathFragment  = gitroot.LegacyWorktreePathFragment
	TaskContextFile             = gitroot.TaskContextFile
)

// GitCommitIsAncestor reports Git reachability while preserving the
// distinction between a proven negative (exit 1) and an unanswerable query.
func GitCommitIsAncestor(ctx context.Context, root, sha, ref string) (bool, error) {
	sha = strings.TrimSpace(sha)
	ref = strings.TrimSpace(ref)
	if sha == "" || ref == "" {
		return false, errors.New("Git ancestry requires a commit and ref")
	}
	return gitroot.GitCommitIsAncestor(ctx, root, sha, ref)
}
