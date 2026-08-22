package harvest

import (
	"context"
	"fmt"
	"strings"
)

// UnlandedSubjects returns the commit subjects in a worktree that are genuinely
// NOT on the main ref -- neither as an ancestor nor as an equivalent patch.
//
// FAC-576: pulse decided "this lane has work" with `git log origin/main..HEAD`,
// which is ancestry-only. A rebase-merge rewrites the SHA, so a landed commit is
// never an ancestor and the lane looked unlanded forever. Pulse therefore
// re-emitted CHA-2206 after it had already been reviewed, merged as
// patch-equivalent 5ee3900f32bf, and reported already-on-main by herd candidate.
//
// The knowledge was not missing -- it was in ContentMerged, in the review-ingest
// cherry check, and in the worktrees report. Pulse had a fourth, weaker copy.
// This is the one definition; callers must not re-derive it.
//
// git cherry marks a commit "-" when an equivalent patch exists upstream and "+"
// when it does not, which is exactly the question being asked.
func UnlandedSubjects(ctx context.Context, worktreeDir, mainRef string) ([]string, error) {
	if strings.TrimSpace(worktreeDir) == "" {
		return nil, fmt.Errorf("harvest: worktree dir required")
	}
	if strings.TrimSpace(mainRef) == "" {
		mainRef = "origin/main"
	}
	out, err := gitOutput(ctx, worktreeDir, "cherry", "-v", mainRef, "HEAD")
	if err != nil {
		// Fail closed: no evidence of unlanded work rather than assuming some.
		return nil, err
	}
	var subjects []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		// Format: "<+|-> <sha> <subject>". Only "+" is genuinely unlanded.
		if !strings.HasPrefix(line, "+ ") {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) < 3 {
			continue
		}
		subjects = append(subjects, strings.TrimSpace(fields[2]))
	}
	return subjects, nil
}

// SubstantiveSubjects drops bookkeeping commits that are not real work.
// Anchors exist to make a worktree reap-safe; wip markers are explicitly
// provisional. Neither justifies opening a review.
func SubstantiveSubjects(subjects []string) []string {
	var out []string
	for _, s := range subjects {
		t := strings.TrimSpace(s)
		if t == "" || strings.HasPrefix(t, "chore: anchor") || strings.HasPrefix(t, "wip:") {
			continue
		}
		out = append(out, t)
	}
	return out
}
