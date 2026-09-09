package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// runReviewRetirementCleanup is shared by `herd cleanup` and the acting drain
// edge. Its default is observe-only; callers must explicitly request acting.
func runReviewRetirementCleanup(ctx context.Context, root string, dryRun bool) (herdr.ReviewRetirementReport, error) {
	registry := herdr.ReviewRetirementRegistry{Path: filepath.Join(root, ".herd", "review", "retirement-manifests.jsonl")}
	rows, err := registry.Latest()
	if err != nil {
		return herdr.ReviewRetirementReport{}, err
	}
	latest := map[string]herdr.ReviewRetirementManifest{}
	for _, m := range rows {
		if err := herdr.ValidateReviewRetirementManifest(m); err != nil {
			return herdr.ReviewRetirementReport{}, fmt.Errorf("review retirement manifest %s: %w", m.Generation, err)
		}
		latest[m.Generation] = m
	}
	manifests := make([]herdr.ReviewRetirementManifest, 0, len(latest))
	for _, m := range latest {
		manifests = append(manifests, m)
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Generation < manifests[j].Generation })
	if len(manifests) == 0 {
		return herdr.ReviewRetirementReport{DryRun: dryRun, Candidates: []herdr.ReviewRetirementCandidate{}}, nil
	}
	ledgerPath := reviewLedgerPath()
	if !filepath.IsAbs(ledgerPath) {
		ledgerPath = filepath.Join(root, ledgerPath)
	}
	var ledger *reviewledger.Ledger
	if dryRun {
		ledger, err = reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	} else {
		ledger, err = reviewledger.NewReviewLedger(root, ledgerPath)
	}
	if err != nil {
		return herdr.ReviewRetirementReport{}, err
	}
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		return herdr.ReviewRetirementReport{}, err
	}
	op := &herdr.NativeReviewRetirementOp{Root: root, RepositoryIdentity: repositoryIdentityForLaunch(cfg), Ledger: ledger, Pool: worktree.NewPool(root, filepath.Join(root, ".herd", "pool"), 2)}
	return herdr.RetireReviewLanesContext(ctx, op, manifests, dryRun)
}
