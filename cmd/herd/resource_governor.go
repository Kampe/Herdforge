package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/resources"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

func newResourceGovernor(cfg *config.Config, root string) (*resources.Governor, error) {
	if cfg == nil || !cfg.ResourceGovernor.Enabled() {
		return nil, nil
	}
	if err := cfg.ResourceGovernor.Validate(); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resource governor canonical root: %w", err)
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("resource governor host identity unavailable: %w", err)
	}
	repoID, err := dispatch.AuthenticatedRepositoryIdentity(resolved)
	if err != nil || strings.TrimSpace(repoID) == "" {
		return nil, fmt.Errorf("resource governor repository identity unavailable: %w", err)
	}
	capacity, err := (resources.OSBackend{}).StatFS(resolved)
	if err != nil || strings.TrimSpace(capacity.FilesystemID) == "" {
		return nil, fmt.Errorf("resource governor filesystem identity unavailable: %w", err)
	}
	timeout, retry, err := cfg.ResourceGovernor.Durations()
	if err != nil {
		return nil, err
	}
	orphanTTL, err := cfg.ResourceGovernor.OrphanCacheDuration()
	if err != nil {
		return nil, err
	}
	lockIdentity := sha256.Sum256([]byte(host + "\x00" + capacity.FilesystemID))
	base := strings.TrimSpace(cfg.Project.DefaultBranch)
	if base == "" {
		base = "main"
	}
	policy := resources.GovernorPolicy{
		HostID: host, RepositoryID: repoID, RepositoryRoot: resolved, BaseRef: "origin/" + base,
		LockPath:               filepath.Join(stateDir(), "resource-governor", fmt.Sprintf("%x.lock", lockIdentity[:12])),
		GeneratedDirectories:   append([]string(nil), cfg.ResourceGovernor.GeneratedDirectories...),
		OrphanRoots:            []string{filepath.Join(resolved, ".herd", "worktrees")},
		OrphanDerivedTargets:   append([]string(nil), cfg.ResourceGovernor.OrphanDerivedTargets...),
		OrphanCacheTTL:         orphanTTL,
		OrphanCacheBudgetBytes: cfg.ResourceGovernor.OrphanCacheBudgetBytes,
		PressureBytes:          cfg.ResourceGovernor.PressureBytes, RecoveryBytes: cfg.ResourceGovernor.RecoveryBytes,
		TaskReserveBytes:       cfg.ResourceGovernor.TaskReserveBytes,
		MaxDispatchConcurrency: cfg.ResourceGovernor.MaxDispatchConcurrency,
		ReapBatchLimit:         cfg.ResourceGovernor.ReapBatchLimit, MaxScanEntries: 250000,
		LockTimeout: timeout, LockRetry: retry, AllowApply: cfg.ResourceGovernor.AllowApply,
		ApplyBeforeDispatch: cfg.ResourceGovernor.ApplyBeforeDispatch,
	}
	claimDir, err := provider.CanonicalClaimDir(".", resolved)
	if err != nil {
		return nil, fmt.Errorf("resource governor canonical claim directory: %w", err)
	}
	// One shared per-run process population: the registered census captures
	// it once and the orphan census reuses it instead of rescanning the
	// process population and owner table between stages (FAC-613).
	sharedPopulation := &resources.ProcessPopulation{}
	// One shared target-probe progress holder: the registered census batch
	// records its completed/deferred lsof probe counts so the report states
	// real progress instead of inferring it from scanned counts.
	probeStats := &resources.BatchProbeStats{}
	enumerator := resources.GitWorktreeEnumerator{
		Processes:        resources.LSOFProcessInspector{Timeout: 2 * time.Second, MaxOutputBytes: 1 << 20},
		Now:              time.Now,
		HostID:           host,
		SharedPopulation: sharedPopulation,
		ProbeStats:       probeStats,
		// Recognize squash- and rebase-landed lanes by content. Without this
		// the census judges landing by ancestry, which a replayed range never
		// satisfies, so every such lane stays "unmerged" and keeps its
		// regenerable artifacts forever.
		Landing: governorLandingPredicate(resolved),
		Evidence: resources.SQLiteLifecycleEvidence{
			ClaimsPath:         deps.ResolveLaunchLeasePath(resolved),
			LaunchClaimsPath:   deps.ResolveLaunchLeasePath(resolved),
			RecoveryClaimsPath: lifecycle.CanonicalStatePath(resolved),
			TaskClaimsPath:     filepath.Join(claimDir, "leases.db"),
			LedgerPath:         reviewledger.DefaultPath(resolved), RepoID: repoID, HostID: host,
			SignedTarget: func(_ context.Context, worktreePath string, _ resources.RegisteredWorktree) (resources.SignedTarget, error) {
				tc, readErr := dispatch.ReadTaskContext(worktreePath)
				if readErr != nil {
					return resources.SignedTarget{}, readErr
				}
				verifier, verifyErr := dispatch.LoadVerifier(resolved)
				if verifyErr != nil {
					return resources.SignedTarget{}, verifyErr
				}
				if verifyErr = verifier.Verify(tc); verifyErr != nil {
					return resources.SignedTarget{}, verifyErr
				}
				return resources.SignedTarget{LeaseID: tc.LeaseID, LeaseGeneration: tc.LeaseGeneration, LeaseTaskRef: tc.LeaseTaskRef, Repository: tc.Repository, CandidateSHA: tc.CandidateSHA, Authenticated: true}, nil
			},
		},
	}
	return &resources.Governor{
		Policy: policy, Capacity: resources.OSBackend{}, Measure: resources.OSPhysicalMeasurer{},
		Worktrees: enumerator, SharedPopulation: sharedPopulation, ProbeStats: probeStats,
		Locks: resources.FileLockProvider{}, Now: time.Now,
	}, nil
}

func attachResourceGovernor(d *dispatch.Dispatcher, cfg *config.Config, root string) error {
	if d == nil {
		return fmt.Errorf("dispatcher is required")
	}
	governor, err := newResourceGovernor(cfg, root)
	if err != nil {
		return err
	}
	if governor != nil {
		d.Resources = governor
	}
	return nil
}

// lifecycleSweepBudget is the operational budget for one full lifecycle
// sweep. It matches the direct resource-governor CLI budget so a lifecycle
// caller can never out-wait that path on the same global lock.
const lifecycleSweepBudget = 45 * time.Second

// runLifecycleGovernorSweep is the shared lifecycle boundary: every cmd/herd
// lifecycle sweep reaches governor.Sweep through it, so the sweep always
// carries a finite deadline. context.WithTimeout clamps to the earliest
// deadline in the chain: an already-shorter caller deadline is preserved
// verbatim, while an unbounded caller (e.g. context.Background() at the
// review pre-refusal census) receives the operational budget. Cancellation
// propagates unchanged and the error is returned fail-closed; the deferred
// cancel releases the timer so no goroutine outlives the sweep.
func runLifecycleGovernorSweep(ctx context.Context, governor *resources.Governor, trigger resources.SweepTrigger) (resources.GovernorReport, error) {
	sweepCtx, cancel := context.WithTimeout(ctx, lifecycleSweepBudget)
	defer cancel()
	return governor.Sweep(sweepCtx, trigger, governor.LifecycleApply())
}

func sweepResourceGovernor(ctx context.Context, cfg *config.Config, root string, trigger resources.SweepTrigger) error {
	governor, err := newResourceGovernor(cfg, root)
	if err != nil {
		return err
	}
	if governor == nil {
		return nil
	}
	_, err = runLifecycleGovernorSweep(ctx, governor, trigger)
	return err
}

func parseForeignTargets(values []string, alertBytes uint64) ([]resources.ForeignTarget, error) {
	targets := make([]resources.ForeignTarget, 0, len(values))
	for _, value := range values {
		parts := strings.SplitN(value, ":", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[2]) == "" {
			return nil, fmt.Errorf("--foreign must be owner:kind:exact-path")
		}
		targets = append(targets, resources.ForeignTarget{
			Owner: strings.TrimSpace(parts[0]), Kind: strings.TrimSpace(parts[1]),
			Path: strings.TrimSpace(parts[2]), AlertBytes: alertBytes,
		})
	}
	return targets, nil
}

func runResourceGovernor(asJSON, apply bool, maxReaps int, foreign []string, foreignAlert string) error {
	cfg, err := config.LoadConfig(config.DefaultConfigPath)
	if err != nil {
		return fmt.Errorf("load repository policy: %w", err)
	}
	root, err := resolveCanonicalRootForGovernor()
	if err != nil {
		return err
	}
	governor, err := newResourceGovernor(cfg, root)
	if err != nil {
		return err
	}
	if governor == nil {
		return fmt.Errorf("repository has no resource_governor.v1 policy; refusing an undeclared generated-data allowlist")
	}
	var alertBytes uint64
	if strings.TrimSpace(foreignAlert) != "" {
		alertBytes, err = strconv.ParseUint(strings.TrimSpace(foreignAlert), 10, 64)
		if err != nil || alertBytes == 0 {
			return fmt.Errorf("--foreign-alert-bytes must be a positive integer")
		}
	}
	foreignTargets, err := parseForeignTargets(foreign, alertBytes)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	report, err := governor.Run(ctx, resources.RunOptions{Apply: apply, BatchLimit: maxReaps, ForeignTargets: foreignTargets})
	if err != nil {
		// A failed run must still emit its structured partial report: the
		// nonzero exit refuses cleanup, while phase timing, scanned and
		// deferred counts, and the causal stage remain actionable (FAC-613).
		if asJSON {
			if data, marshalErr := json.MarshalIndent(report, "", "  "); marshalErr == nil {
				fmt.Println(string(data))
			}
		}
		return fmt.Errorf("%w; report=%s", err, report.JSON())
	}
	if asJSON {
		data, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("resource-governor: host=%s mode=%s free=%d->%d physical-reclaimed=%d concurrency=%d available=%d\n",
		report.HostID, report.Mode, report.CapacityBefore.FreeBytes, report.CapacityAfter.FreeBytes,
		report.ReclaimedBytes, report.CapacityAwareConcurrency, report.AvailableDispatchConcurrency)
	for _, target := range report.Targets {
		fmt.Printf("  %s path=%s physical=%d->%d reason=%s\n", target.Decision, target.Path, target.BeforeBytes, target.AfterBytes, target.Reason)
	}
	for _, item := range report.Foreign {
		fmt.Printf("  foreign owner=%s kind=%s path=%s physical=%d escalate=%t action=%s error=%s\n",
			item.Owner, item.Kind, item.Path, item.AllocatedBytes, item.Escalate, item.Action, item.Error)
	}
	return nil
}

func runResourceGovernorCommand(args []string) error {
	fs := flag.NewFlagSet("resource-governor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit a structured report")
	apply := fs.Bool("apply", false, "remove only admitted generated targets")
	maxReaps := fs.Int("max-reaps", 0, "bound the number of generated targets")
	var foreign []string
	fs.Func("foreign", "observe owner:kind:exact-path telemetry", func(value string) error {
		foreign = append(foreign, value)
		return nil
	})
	foreignAlert := fs.String("foreign-alert-bytes", "", "escalate foreign telemetry at this allocation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runResourceGovernor(*asJSON, *apply, *maxReaps, foreign, *foreignAlert)
}

func resolveCanonicalRootForGovernor() (string, error) {
	root, err := worktree.ResolveCanonicalRoot(context.Background(), ".", firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ""))
	if err != nil {
		return "", fmt.Errorf("resource governor canonical root: %w", err)
	}
	return root, nil
}
