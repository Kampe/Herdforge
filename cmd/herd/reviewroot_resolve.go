package main

import (
	"context"
	"strings"

	"github.com/Kampe/Herdforge/pkg/reviewroot"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// resolvedReviewRoot pairs the review corpus with the repository root it was
// anchored to, because RetainArtifact takes a repo root rather than a review
// root and the two must come from the same resolution.
type resolvedReview struct {
	Paths    reviewroot.Paths
	RepoRoot string
}

// resolvedReviewRoot resolves the canonical review corpus for startDir.
//
// FAC-572: the durable handoff mailbox was Git-common-rooted while review
// artifacts were cwd-relative, so a supervisor could ingest a different corpus
// than its queue referred to. One resolver, one anchor.
func resolvedReviewRoot(startDir string) resolvedReview {
	if strings.TrimSpace(startDir) == "" {
		startDir = "."
	}
	out := resolvedReview{Paths: reviewroot.Resolve(startDir), RepoRoot: startDir}
	if root, err := worktree.ResolveCanonicalRoot(context.Background(), startDir,
		firstEnv("HERD_ROOT", "HERD_REPO_ROOT", "")); err == nil && strings.TrimSpace(root) != "" {
		out.RepoRoot = root
	}
	return out
}
