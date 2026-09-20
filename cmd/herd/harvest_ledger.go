package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func cliMergeProofContext() (context.Context, context.CancelFunc) {
	return (&mergeadmit.Gate{ProofBudget: cliProofBudget}).ProofContext()
}

// resolveHarvestLedgerPath is strict: discovery failure cannot turn into an
// empty worktree-local ledger. An explicit override retains its existing
// spelling and precedence. HERD_ROOT still names a lane, never the project.
func resolveHarvestLedgerPath(ctx context.Context, startDir string) (string, error) {
	if override := strings.TrimSpace(os.Getenv("HERD_REVIEW_LEDGER")); override != "" {
		return override, nil
	}
	root, _, err := gitroot.ProjectRootWithGit(startDir, mergeadmit.BoundedGit(ctx, startDir))
	if err != nil {
		return "", fmt.Errorf("resolve canonical review ledger: %w", err)
	}
	return reviewledger.PathFor(root), nil
}

func openHarvestLedgerContext(ctx context.Context, repoDir string) (*reviewledger.Ledger, error) {
	path, err := resolveHarvestLedgerPath(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	// Git operations retain the invoking checkout. Only durable review authority
	// is project-rooted; this must not move the proof or its source cleanliness.
	return reviewledger.NewReviewLedger(repoDir, path)
}

func openHarvestLedger(repoDir string) (*reviewledger.Ledger, error) {
	ctx, cancel := cliMergeProofContext()
	defer cancel()
	return openHarvestLedgerContext(ctx, repoDir)
}

func verifyLandedLedgerPath(ctx context.Context, binding verifyLandedBinding) (string, error) {
	if binding.ledgerPath != "" {
		return binding.ledgerPath, nil
	}
	return resolveHarvestLedgerPath(ctx, ".")
}
