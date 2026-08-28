package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/reviewroot"
)

// resolvedReviewRoot pairs the review corpus with the repository root it was
// anchored to, because RetainArtifact takes a repo root rather than a review
// root and the two must come from the same resolution.
type resolvedReview struct {
	Paths    reviewroot.Paths
	RepoRoot string
}

// reviewIngestRoots is the one root bundle for a review-ingest run. Its
// project root is resolved once; every mutable review artifact is then derived
// from that exact value.
type reviewIngestRoots struct {
	ProjectRoot          string
	Review               resolvedReview
	ReceiptPath          string
	CanonicalReceiptPath string
	LedgerPath           string
	CanonicalLedgerPath  string
}

// resolveReviewIngestRoots resolves the mandatory project root once through
// gitroot.ProjectRoot, then derives all review-ingest paths from it.
func resolveReviewIngestRoots(startDir string) (reviewIngestRoots, error) {
	projectRoot, laneOverride, err := gitroot.ProjectRoot(context.Background(), startDir)
	if err != nil {
		return reviewIngestRoots{}, err
	}
	review := resolvedReviewRoot(projectRoot, laneOverride)
	return reviewIngestRoots{
		ProjectRoot:          projectRoot,
		Review:               review,
		ReceiptPath:          launch.ReceiptPathFor(projectRoot),
		CanonicalReceiptPath: launch.PathFor(projectRoot),
		LedgerPath:           reviewledger.DefaultPath(projectRoot),
		CanonicalLedgerPath:  reviewledger.PathFor(projectRoot),
	}, nil
}

// requireMutationSafe refuses overrides that would combine provenance from one
// project with a ledger or receipt from another. Read-only paths keep their
// established override behavior; only a write must prove one authority root.
func (r reviewIngestRoots) requireMutationSafe() error {
	for _, target := range []struct {
		name      string
		actual    string
		canonical string
	}{
		{name: "launch receipt", actual: r.ReceiptPath, canonical: r.CanonicalReceiptPath},
		{name: "review ledger", actual: r.LedgerPath, canonical: r.CanonicalLedgerPath},
	} {
		actual, err := filepath.Abs(target.actual)
		if err != nil {
			return fmt.Errorf("resolve %s path %q: %w", target.name, target.actual, err)
		}
		canonical, err := filepath.Abs(target.canonical)
		if err != nil {
			return fmt.Errorf("resolve canonical %s path %q: %w", target.name, target.canonical, err)
		}
		if filepath.Clean(actual) == filepath.Clean(canonical) {
			continue
		}
		return fmt.Errorf("refusing mixed-project review ingest: provenance root %q and %s root %q disagree (path %q; expected %q)",
			r.ProjectRoot, target.name, filepath.Dir(filepath.Dir(actual)), actual, canonical)
	}
	return nil
}

// resolvedReviewRoot derives the canonical review corpus from an already
// resolved project root. It deliberately does not consult HERD_ROOT: that
// value describes a lane, not a project.
//
// FAC-572: the durable handoff mailbox was Git-common-rooted while review
// artifacts were cwd-relative, so a supervisor could ingest a different corpus
// than its queue referred to. One resolver, one anchor.
func resolvedReviewRoot(projectRoot, laneOverride string) resolvedReview {
	projectRoot = strings.TrimSpace(projectRoot)
	if projectRoot == "" {
		return resolvedReview{}
	}
	projectRoot = filepath.Clean(projectRoot)
	return resolvedReview{
		Paths: reviewroot.Paths{
			Root:         filepath.Join(projectRoot, reviewroot.Rel),
			Canonical:    true,
			LaneOverride: laneOverride,
		},
		RepoRoot: projectRoot,
	}
}
