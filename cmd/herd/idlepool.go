package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// reviewRetirementPhaseJournalPath mirrors NativeReviewRetirementOp's own
// default (pkg/herdr/native_review_retirement.go, unexported phasePath())
// -- the authoritative record of whether a manifested retirement generation
// actually completed. Duplicated here rather than exported from pkg/herdr,
// which is out of this repair's scope.
func reviewRetirementPhaseJournalPath(root string) string {
	return filepath.Join(root, ".herd", "review", "retirement-phases.jsonl")
}

// runIdlePool is the small operational surface for bounded, non-current
// pool-root discovery and reclamation. It never scans a worktree's contents
// or history, and it never deletes anything directly -- every removal goes
// through worktree.Pool.GC, the same fail-closed primitive `herd pool gc`
// uses for one named pool.
func runIdlePool() {
	fs := flag.NewFlagSet("idle-pool", flag.ContinueOnError)
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	base := fs.String("base", "origin/main", "base ref a pool slot's HEAD must be reachable from to be reclaimed")
	maxRoots := fs.Int("max-roots", 10, "maximum pool roots to inspect this tick")
	act := fs.Bool("act", false, "reclaim eligible idle pool roots; default is a dry-run report")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "herd idle-pool: %v\n", err)
		os.Exit(2)
	}
	cfg := idlePoolConfigFor(root, *base, *maxRoots)
	ctx := context.Background()
	var (
		result worktree.IdlePoolDiscoveryResult
		err    error
	)
	if *act {
		result, err = worktree.ReclaimIdlePools(ctx, cfg)
	} else {
		result, err = worktree.DiscoverIdlePools(ctx, cfg)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd idle-pool: %v\n", err)
		os.Exit(1)
	}
	for _, d := range result.Dispositions {
		if d.Reason != "" {
			fmt.Printf("%s\t%s\t%s\n", d.Status, d.Root, d.Reason)
			continue
		}
		fmt.Printf("%s\t%s\n", d.Status, d.Root)
	}
	fmt.Printf("scanned=%d eligible=%d retained=%d failed=%d next_cursor=%s\n",
		result.Scanned, result.Eligible, result.Retained, result.Failed, result.NextCursor)
	if result.Failed > 0 {
		os.Exit(1)
	}
}

// idlePoolConfigFor builds the production config: no ProcessInspector
// override, so each constructed Pool keeps its own real native process
// census default (worktree.NewPool). Only test fixtures inject a fake.
func idlePoolConfigFor(root, base string, maxRoots int) worktree.IdlePoolDiscoveryConfig {
	return worktree.IdlePoolDiscoveryConfig{
		RepoRoot:         root,
		DefaultBase:      base,
		MaxRoots:         maxRoots,
		ManifestPath:     herdr.ReviewRetirementRegistryPath(root),
		PhaseJournalPath: reviewRetirementPhaseJournalPath(root),
	}
}

// reclaimIdlePoolsOnPulse is the tiny wiring hunk that puts idle-pool
// discovery on the existing regular maintenance cadence (`herd pulse --act`)
// instead of leaving one-off `herd pool gc` as the only trigger. It is
// deliberately isolated from pulse.Beat/Observation/Actor: a small, bounded
// call whose disposition is reported to the caller, which decides how it
// affects the beat's exit code -- pool housekeeping must not silently
// swallow a genuine failure, but an ordinary retained/leased/protected
// tick, or one that simply lost a lock race with a concurrent tick, is not
// a failure.
//
// The regular daemon path reaches this automatically: `herd daemon` drives
// daemon.RunPulseScheduler on a fixed interval (default 60s,
// cmd/herd/main.go's runDaemon), and every scheduled tick invokes
// runPulseCommandContext(ctx, []string{"--act", ...}, ...) before its own
// dispatch cycle -- so this call fires on every daemon tick, not only a
// manually-run `herd pulse --act`.
func reclaimIdlePoolsOnPulse(ctx context.Context, errOut *os.File) bool {
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	cfg := idlePoolConfigFor(root, "origin/main", 5)
	result, err := worktree.ReclaimIdlePools(ctx, cfg)
	if err != nil {
		// A concurrent tick losing the lock race is expected and benign --
		// the other tick is doing the work instead. Anything else is a
		// genuine failure and must not be silently swallowed.
		if isIdlePoolTickBusy(err) {
			fmt.Fprintf(errOut, "pulse: idle-pool reclaim deferred: %v\n", err)
			return true
		}
		fmt.Fprintf(errOut, "pulse: idle-pool reclaim: %v\n", err)
		return false
	}
	if result.Scanned > 0 {
		fmt.Fprintf(errOut, "pulse: idle-pool reclaim scanned=%d eligible=%d retained=%d failed=%d\n",
			result.Scanned, result.Eligible, result.Retained, result.Failed)
	}
	return result.Failed == 0
}

func isIdlePoolTickBusy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "tick is busy or lock is stale")
}
