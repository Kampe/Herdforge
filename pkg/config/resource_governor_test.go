package config

import (
	"math"
	"strings"
	"testing"
)

func validResourceGovernor() ResourceGovernor {
	return ResourceGovernor{
		Version: "v1", GeneratedDirectories: []string{"node_modules", "web/dist", "coverage"},
		PressureBytes: 10, RecoveryBytes: 20, TaskReserveBytes: 5,
		MaxDispatchConcurrency: 4, ReapBatchLimit: 2, LockTimeout: "2s", LockRetry: "25ms",
	}
}

func TestResourceGovernorValidate(t *testing.T) {
	if err := (ResourceGovernor{}).Validate(); err != nil {
		t.Fatalf("empty optional policy: %v", err)
	}
	if err := validResourceGovernor().Validate(); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	orphans := validResourceGovernor()
	orphans.OrphanDerivedTargets = []string{"graph.db", "bootstrap-go-mod"}
	orphans.OrphanCacheTTL = "1h"
	orphans.OrphanCacheBudgetBytes = 1024
	if err := orphans.Validate(); err != nil {
		t.Fatalf("supported orphan derived targets rejected: %v", err)
	}
	managedOnly := ResourceGovernor{
		Version: "v1", OrphanDerivedTargets: []string{"bootstrap-go-build"},
		OrphanCacheTTL: "24h", OrphanCacheBudgetBytes: 1 << 30,
		PressureBytes: 10, RecoveryBytes: 20, TaskReserveBytes: 5,
		MaxDispatchConcurrency: 2, ReapBatchLimit: 2, LockTimeout: "2s", LockRetry: "25ms",
	}
	if err := managedOnly.Validate(); err != nil {
		t.Fatalf("managed-only orphan policy rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*ResourceGovernor)
		want string
	}{
		{name: "absolute", edit: func(g *ResourceGovernor) { g.GeneratedDirectories = []string{"/tmp/cache"} }, want: "repository-relative"},
		{name: "parent", edit: func(g *ResourceGovernor) { g.GeneratedDirectories = []string{"../cache"} }, want: "repository-relative"},
		{name: "glob", edit: func(g *ResourceGovernor) { g.GeneratedDirectories = []string{"**/dist"} }, want: "globs"},
		{name: "git", edit: func(g *ResourceGovernor) { g.GeneratedDirectories = []string{".git/objects"} }, want: ".git"},
		{name: "herd", edit: func(g *ResourceGovernor) { g.GeneratedDirectories = []string{".herd/cache"} }, want: ".herd"},
		{name: "overlap", edit: func(g *ResourceGovernor) { g.GeneratedDirectories = []string{"web/dist", "web/dist/cache"} }, want: "overlapping"},
		{name: "noncanonical", edit: func(g *ResourceGovernor) { g.GeneratedDirectories = []string{"./node_modules"} }, want: "canonical"},
		{name: "watermarks", edit: func(g *ResourceGovernor) { g.RecoveryBytes = g.PressureBytes }, want: "recovery_bytes"},
		{name: "reserve", edit: func(g *ResourceGovernor) { g.TaskReserveBytes = 0 }, want: "task_reserve_bytes"},
		{name: "overflow", edit: func(g *ResourceGovernor) { g.RecoveryBytes = math.MaxUint64 }, want: "overflow"},
		{name: "concurrency", edit: func(g *ResourceGovernor) { g.MaxDispatchConcurrency = 0 }, want: "max_dispatch_concurrency"},
		{name: "batch", edit: func(g *ResourceGovernor) { g.ReapBatchLimit = 0 }, want: "reap_batch_limit"},
		{name: "timeout", edit: func(g *ResourceGovernor) { g.LockTimeout = "31s" }, want: "lock_timeout"},
		{name: "apply", edit: func(g *ResourceGovernor) { g.ApplyBeforeDispatch = true }, want: "allow_apply"},
		{name: "orphan absolute", edit: func(g *ResourceGovernor) { g.OrphanDerivedTargets = []string{"/tmp/cache"} }, want: "orphan-relative"},
		{name: "orphan unsupported", edit: func(g *ResourceGovernor) { g.OrphanDerivedTargets = []string{"node_modules"} }, want: "unsupported"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := validResourceGovernor()
			tc.edit(&g)
			if err := g.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() err=%v, want containing %q", err, tc.want)
			}
		})
	}
}
