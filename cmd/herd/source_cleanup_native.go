package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/herdr"
)

// runSourceRetirementCleanup is shared by `herd cleanup` and the coordinator
// lifecycle sweep. Its default is observe-only; callers must explicitly request acting.
func runSourceRetirementCleanup(ctx context.Context, root string, dryRun bool) (herdr.SourceRetirementReport, error) {
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return herdr.SourceRetirementReport{}, fmt.Errorf("load herd configuration: %w", err)
	}
	repoIdent := ""
	if id, err := dispatch.AuthenticatedRepositoryIdentity(root); err == nil {
		repoIdent = id
	} else if cfg != nil {
		repoIdent = repositoryIdentityForLaunch(cfg)
	}

	// Automatic enrollment from durable ready handoff reports (persist only when acting)
	newlyEnrolled, err := herdr.EnrollReadySourceManifests(root, repoIdent, !dryRun)
	if err != nil {
		return herdr.SourceRetirementReport{}, fmt.Errorf("enroll ready source manifests: %w", err)
	}

	registry := herdr.SourceRetirementRegistry{Path: herdr.SourceRetirementRegistryPath(root)}
	rows, err := registry.Latest()
	if err != nil {
		return herdr.SourceRetirementReport{}, err
	}
	latest := map[string]herdr.SourceRetirementManifest{}
	for _, m := range rows {
		if err := herdr.ValidateSourceRetirementManifest(m); err != nil {
			return herdr.SourceRetirementReport{}, fmt.Errorf("source retirement manifest %s: %w", m.Generation, err)
		}
		latest[m.Generation] = m
	}
	if dryRun {
		for _, m := range newlyEnrolled {
			latest[m.Generation] = m
		}
	}
	manifests := make([]herdr.SourceRetirementManifest, 0, len(latest))
	for _, m := range latest {
		manifests = append(manifests, m)
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Generation < manifests[j].Generation })
	if len(manifests) == 0 {
		return herdr.SourceRetirementReport{DryRun: dryRun, Candidates: []herdr.SourceRetirementCandidate{}}, nil
	}
	standing := configuredStandingAgentNames(cfg)
	op := &herdr.NativeSourceRetirementOp{
		Root:               root,
		RepositoryIdentity: repoIdent,
		StandingLanes:      standing,
	}
	return herdr.RetireSourceLanesContext(ctx, op, manifests, dryRun)
}
