package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// ReviewRetirementSelector restricts retirement to an exact matching manifest.
type ReviewRetirementSelector struct {
	Reviewer   string
	Lease      string
	Generation string
	TaskRef    string
}

func (s ReviewRetirementSelector) IsActive() bool {
	return s.Reviewer != "" || s.Lease != "" || s.Generation != "" || s.TaskRef != ""
}

func (s ReviewRetirementSelector) Matches(m herdr.ReviewRetirementManifest) bool {
	if s.Reviewer != "" && m.Reviewer != s.Reviewer {
		return false
	}
	if s.Lease != "" && m.Nonce != s.Lease {
		return false
	}
	if s.Generation != "" && m.Generation != s.Generation {
		return false
	}
	if s.TaskRef != "" && !strings.EqualFold(m.TaskRef, s.TaskRef) {
		return false
	}
	return true
}

// runReviewRetirementCleanup is shared by `herd cleanup` and the acting drain
// edge. Its default is observe-only; callers must explicitly request acting.
func runReviewRetirementCleanup(ctx context.Context, root string, dryRun bool, selector ReviewRetirementSelector) (herdr.ReviewRetirementReport, error) {
	registry := herdr.ReviewRetirementRegistry{Path: herdr.ReviewRetirementRegistryPath(root)}
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
		if selector.IsActive() && !selector.Matches(m) {
			continue
		}
		manifests = append(manifests, m)
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Generation < manifests[j].Generation })
	if selector.IsActive() && len(manifests) == 0 {
		return herdr.ReviewRetirementReport{DryRun: dryRun, Candidates: []herdr.ReviewRetirementCandidate{}},
			fmt.Errorf("review retirement: no manifest matches selector (reviewer=%q lease=%q generation=%q task=%q)", selector.Reviewer, selector.Lease, selector.Generation, selector.TaskRef)
	}
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
	op := &herdr.NativeReviewRetirementOp{Root: root, RepositoryIdentity: repositoryIdentityForLaunch(cfg), Ledger: ledger}
	return herdr.RetireReviewLanesContext(ctx, op, manifests, dryRun)
}
