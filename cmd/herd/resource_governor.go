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
	"github.com/Kampe/Herdforge/pkg/resources"
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
	capacity, err := (resources.OSBackend{}).StatFS(resolved)
	if err != nil || strings.TrimSpace(capacity.FilesystemID) == "" {
		return nil, fmt.Errorf("resource governor filesystem identity unavailable: %w", err)
	}
	timeout, retry, err := cfg.ResourceGovernor.Durations()
	if err != nil {
		return nil, err
	}
	lockIdentity := sha256.Sum256([]byte(host + "\x00" + capacity.FilesystemID))
	base := strings.TrimSpace(cfg.Project.DefaultBranch)
	if base == "" {
		base = "main"
	}
	policy := resources.GovernorPolicy{
		HostID: host, RepositoryRoot: resolved, BaseRef: "origin/" + base,
		LockPath:             filepath.Join(stateDir(), "resource-governor", fmt.Sprintf("%x.lock", lockIdentity[:12])),
		GeneratedDirectories: append([]string(nil), cfg.ResourceGovernor.GeneratedDirectories...),
		PressureBytes:        cfg.ResourceGovernor.PressureBytes, RecoveryBytes: cfg.ResourceGovernor.RecoveryBytes,
		TaskReserveBytes:       cfg.ResourceGovernor.TaskReserveBytes,
		MaxDispatchConcurrency: cfg.ResourceGovernor.MaxDispatchConcurrency,
		ReapBatchLimit:         cfg.ResourceGovernor.ReapBatchLimit, MaxScanEntries: 250000,
		LockTimeout: timeout, LockRetry: retry, AllowApply: cfg.ResourceGovernor.AllowApply,
		ApplyBeforeDispatch: cfg.ResourceGovernor.ApplyBeforeDispatch,
	}
	return &resources.Governor{
		Policy: policy, Capacity: resources.OSBackend{}, Measure: resources.OSPhysicalMeasurer{},
		Worktrees: resources.GitWorktreeEnumerator{
			Processes: resources.LSOFProcessInspector{Timeout: 2 * time.Second, MaxOutputBytes: 1 << 20},
			Now:       time.Now,
		},
		Locks: resources.FileLockProvider{}, Now: time.Now,
	}, nil
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
