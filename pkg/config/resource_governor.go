package config

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"
)

// ResourceGovernor is the repository-owned policy for host-local capacity
// admission and derived-data reclamation. It is deliberately opt-in: an empty
// policy preserves the existing disk gate, while a declared policy must name
// every generated directory that Herdforge may consider.
type ResourceGovernor struct {
	Version                string   `yaml:"version,omitempty"`
	GeneratedDirectories   []string `yaml:"generated_directories,omitempty"`
	OrphanDerivedTargets   []string `yaml:"orphan_derived_targets,omitempty"`
	OrphanCacheTTL         string   `yaml:"orphan_cache_ttl,omitempty"`
	OrphanCacheBudgetBytes uint64   `yaml:"orphan_cache_budget_bytes,omitempty"`
	PressureBytes          uint64   `yaml:"pressure_bytes,omitempty"`
	RecoveryBytes          uint64   `yaml:"recovery_bytes,omitempty"`
	TaskReserveBytes       uint64   `yaml:"task_reserve_bytes,omitempty"`
	MaxDispatchConcurrency int      `yaml:"max_dispatch_concurrency,omitempty"`
	ReapBatchLimit         int      `yaml:"reap_batch_limit,omitempty"`
	LockTimeout            string   `yaml:"lock_timeout,omitempty"`
	LockRetry              string   `yaml:"lock_retry,omitempty"`
	AllowApply             bool     `yaml:"allow_apply,omitempty"`
	ApplyBeforeDispatch    bool     `yaml:"apply_before_dispatch,omitempty"`
}

func (g ResourceGovernor) Enabled() bool {
	return g.Version != "" || len(g.GeneratedDirectories) != 0 || len(g.OrphanDerivedTargets) != 0 || g.PressureBytes != 0 ||
		g.RecoveryBytes != 0 || g.TaskReserveBytes != 0 || g.MaxDispatchConcurrency != 0 ||
		g.ReapBatchLimit != 0 || g.LockTimeout != "" || g.LockRetry != "" ||
		g.AllowApply || g.ApplyBeforeDispatch
}

// Durations returns the already-validated bounded lock timings.
func (g ResourceGovernor) Durations() (time.Duration, time.Duration, error) {
	timeout, err := time.ParseDuration(strings.TrimSpace(g.LockTimeout))
	if err != nil || timeout <= 0 || timeout > 30*time.Second {
		return 0, 0, fmt.Errorf("resource_governor.lock_timeout must be a positive duration no greater than 30s: %q", g.LockTimeout)
	}
	retry, err := time.ParseDuration(strings.TrimSpace(g.LockRetry))
	if err != nil || retry <= 0 || retry > timeout {
		return 0, 0, fmt.Errorf("resource_governor.lock_retry must be positive and no greater than lock_timeout: %q", g.LockRetry)
	}
	return timeout, retry, nil
}

func (g ResourceGovernor) OrphanCacheDuration() (time.Duration, error) {
	if strings.TrimSpace(g.OrphanCacheTTL) == "" {
		return 0, nil
	}
	ttl, err := time.ParseDuration(strings.TrimSpace(g.OrphanCacheTTL))
	if err != nil || ttl <= 0 || ttl > 30*24*time.Hour {
		return 0, fmt.Errorf("resource_governor.orphan_cache_ttl must be a positive duration no greater than 720h")
	}
	return ttl, nil
}

func (g ResourceGovernor) Validate() error {
	if !g.Enabled() {
		return nil
	}
	if g.Version != "v1" {
		return fmt.Errorf("resource_governor.version: unsupported version %q", g.Version)
	}
	if len(g.GeneratedDirectories) == 0 {
		return fmt.Errorf("resource_governor.generated_directories: at least one exact repository-relative directory is required")
	}
	seenOrphan := make(map[string]struct{}, len(g.OrphanDerivedTargets))
	for i, raw := range g.OrphanDerivedTargets {
		clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(raw)))
		if clean != raw || clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("resource_governor.orphan_derived_targets[%d]: must be an exact orphan-relative path", i)
		}
		if clean != "graph.db" && clean != "bootstrap-go-mod" {
			return fmt.Errorf("resource_governor.orphan_derived_targets[%d]: unsupported derived target %q", i, raw)
		}
		if _, ok := seenOrphan[clean]; ok {
			return fmt.Errorf("resource_governor.orphan_derived_targets[%d]: duplicate target %q", i, raw)
		}
		seenOrphan[clean] = struct{}{}
	}
	if len(g.OrphanDerivedTargets) != 0 {
		if _, err := g.OrphanCacheDuration(); err != nil {
			return err
		}
		if g.OrphanCacheBudgetBytes == 0 {
			return fmt.Errorf("resource_governor.orphan_cache_budget_bytes must be nonzero when orphan targets are enabled")
		}
	}
	seen := make(map[string]struct{}, len(g.GeneratedDirectories))
	for i, raw := range g.GeneratedDirectories {
		trimmed := strings.TrimSpace(raw)
		path := filepath.Clean(trimmed)
		if path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
			return fmt.Errorf("resource_governor.generated_directories[%d]: must be an exact repository-relative directory", i)
		}
		if raw != filepath.ToSlash(path) {
			return fmt.Errorf("resource_governor.generated_directories[%d]: path must be canonical", i)
		}
		if strings.ContainsAny(raw, "*?[]{}") {
			return fmt.Errorf("resource_governor.generated_directories[%d]: globs are not allowed", i)
		}
		for _, component := range strings.Split(filepath.ToSlash(path), "/") {
			if component == ".git" || component == ".herd" {
				return fmt.Errorf("resource_governor.generated_directories[%d]: canonical %s state is never generated data", i, component)
			}
		}
		key := filepath.ToSlash(path)
		for existing := range seen {
			if key == existing || strings.HasPrefix(key, existing+"/") || strings.HasPrefix(existing, key+"/") {
				return fmt.Errorf("resource_governor.generated_directories[%d]: duplicate or overlapping path %q", i, key)
			}
		}
		seen[key] = struct{}{}
	}
	if g.PressureBytes == 0 || g.RecoveryBytes <= g.PressureBytes {
		return fmt.Errorf("resource_governor.recovery_bytes must be greater than nonzero pressure_bytes")
	}
	if g.TaskReserveBytes == 0 {
		return fmt.Errorf("resource_governor.task_reserve_bytes must be nonzero")
	}
	if g.PressureBytes > math.MaxUint64-g.TaskReserveBytes || g.RecoveryBytes > math.MaxUint64-g.TaskReserveBytes {
		return fmt.Errorf("resource_governor watermarks plus task_reserve_bytes overflow")
	}
	if g.MaxDispatchConcurrency <= 0 || g.MaxDispatchConcurrency > 256 {
		return fmt.Errorf("resource_governor.max_dispatch_concurrency must be between 1 and 256")
	}
	if g.ReapBatchLimit <= 0 || g.ReapBatchLimit > 64 {
		return fmt.Errorf("resource_governor.reap_batch_limit must be between 1 and 64")
	}
	if _, _, err := g.Durations(); err != nil {
		return err
	}
	if g.ApplyBeforeDispatch && !g.AllowApply {
		return fmt.Errorf("resource_governor.apply_before_dispatch requires allow_apply: true")
	}
	return nil
}
