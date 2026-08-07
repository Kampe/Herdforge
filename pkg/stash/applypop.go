package stash

import (
	"context"
	"fmt"
)

// Apply replays the newest entry without dropping it.
// A conflicting apply always KEEPS the entry.
func (r Repo) ApplyKeep(ctx context.Context) (string, error) {
	return r.apply(ctx, false)
}

// Pop replays the newest entry and drops the ref on success.
// A conflicting apply always KEEPS the entry.
func (r Repo) Pop(ctx context.Context) (string, error) {
	return r.apply(ctx, true)
}

// Apply is the historical API: drop=true means pop, drop=false means apply.
// Prefer ApplyKeep / Pop for new call sites.
func (r Repo) Apply(drop bool) (string, error) {
	return r.apply(context.Background(), drop)
}

func (r Repo) apply(ctx context.Context, drop bool) (string, error) {
	ref, err := r.NewestContext(ctx)
	if err != nil {
		return "", err
	}
	sha, err := r.git(ctx, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	// `git stash apply <commit>` replays without touching refs/stash.
	if _, err := r.git(ctx, "stash", "apply", sha); err != nil {
		return ref, fmt.Errorf("apply of %s hit a conflict; entry KEPT. Resolve, then re-run pop (or git update-ref -d %s)", ref, ref)
	}
	if drop {
		if _, err := r.git(ctx, "update-ref", "-d", ref); err != nil {
			return ref, fmt.Errorf("applied %s but could not drop it: %w", ref, err)
		}
	}
	return ref, nil
}
