package stash

import (
	"context"
	"fmt"
	"strings"
)

// SharedStackConflict reports entries on the SHARED refs/stash stack that were
// recorded against this worktree's current branch ("on <branch>:" markers,
// case-insensitive). Those are exactly the racy entries herd-stash replaces;
// callers must refuse rather than proceed while any remain.
//
// Detached HEAD (empty branch) never matches — no false positive.
func (r Repo) SharedStackConflict() []string {
	return r.SharedStackConflictContext(context.Background())
}

// SharedStackConflictContext is the context-aware form of SharedStackConflict.
func (r Repo) SharedStackConflictContext(ctx context.Context) []string {
	branch := r.BranchContext(ctx)
	if branch == "" {
		return nil
	}
	hits, _ := RefuseSharedStack(ctx, r.Dir, branch)
	return hits
}

// RefuseSharedStack returns the offending git-stash lines if the shared
// refs/stash stack holds entries whose subject matches " on <branch>:"
// (case-insensitive). When hits are non-empty the caller must hard-fail with
// the migrate hint before any verb (push/pop/apply/list).
//
// branch of "" or "HEAD" yields no hits (detached — nothing to match).
func RefuseSharedStack(ctx context.Context, dir, branch string) (hits []string, err error) {
	if branch == "" || branch == "HEAD" {
		return nil, nil
	}
	cmd := execCommandContext(ctx, "git", "stash", "list")
	cmd.Dir = dir
	out, runErr := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if runErr != nil {
		// Empty stash list can still exit non-zero on some git configs; treat
		// parseable stdout as authoritative when present.
		if text == "" {
			return nil, nil
		}
	}
	// Case-insensitive: git writes "On <branch>:" for `stash push` and
	// "WIP on <branch>:" for a bare `git stash`. The zsh port used grep -iF.
	needle := strings.ToLower("on " + branch + ":")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(strings.ToLower(line), needle) {
			hits = append(hits, line)
		}
	}
	return hits, nil
}

// FormatSharedStackRefusal builds the stderr message the CLI prints when the
// shared stack still holds racy entries on the caller's branch.
func FormatSharedStackRefusal(branch string, hits []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "herd-stash: the shared 'git stash' stack holds entries on branch '%s':\n", branch)
	for _, h := range hits {
		fmt.Fprintf(&b, "  %s\n", h)
	}
	b.WriteString("herd-stash: migrate them off the shared stack first (they race across worktrees):\n")
	b.WriteString("  git stash apply <id> && bin/herd-stash push -m migrated   # then: git stash drop <id>\n")
	return b.String()
}
