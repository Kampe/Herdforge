package router

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/resolve"
	"github.com/Kampe/Herdforge/pkg/usage"
)

func TestAdvertisingProbeBoundsStuckProvider(t *testing.T) {
	resetAdvertisingProbeCacheForTest()
	prev := execProviderProbe
	t.Cleanup(func() {
		execProviderProbe = prev
		resetAdvertisingProbeCacheForTest()
	})
	execProviderProbe = func(ctx context.Context, command string, args []string, stdin string) (string, string, error, bool) {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err(), true
		case <-time.After(30 * time.Second):
			return providerProbeSentinel, "", nil, false
		}
	}

	start := time.Now()
	ok, reason := advertisingProviderProbe("codex", "gpt-5.3-codex-spark")
	elapsed := time.Since(start)
	if ok {
		t.Fatalf("stuck probe must not report available")
	}
	if reason != "provider_probe_deadline" {
		t.Fatalf("reason=%q want provider_probe_deadline", reason)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("advertising probe took %s; must honor ~2s deadline", elapsed)
	}
}

func TestAdvertisingProbeCacheAvoidsRepeatStuckProbe(t *testing.T) {
	resetAdvertisingProbeCacheForTest()
	prev := execProviderProbe
	t.Cleanup(func() {
		execProviderProbe = prev
		resetAdvertisingProbeCacheForTest()
	})
	var calls atomic.Int64
	execProviderProbe = func(ctx context.Context, command string, args []string, stdin string) (string, string, error, bool) {
		calls.Add(1)
		<-ctx.Done()
		return "", "", ctx.Err(), true
	}
	_, _ = advertisingProviderProbe("codex", "gpt-5.3-codex-spark")
	_, _ = advertisingProviderProbe("codex", "gpt-5.3-codex-spark")
	if calls.Load() != 1 {
		t.Fatalf("cache miss count=%d want 1", calls.Load())
	}
}

// CHA-2451 acceptance: stuck live probe must not prevent bounded non-empty
// resolve-lane JSON. ReadPathProbes skips generation probes entirely.
func TestReadPathResolveAllEmitsNonEmptyJSONWhenProbeStuck(t *testing.T) {
	resetAdvertisingProbeCacheForTest()
	prev := execProviderProbe
	t.Cleanup(func() {
		execProviderProbe = prev
		resetAdvertisingProbeCacheForTest()
	})
	var calls atomic.Int64
	execProviderProbe = func(ctx context.Context, command string, args []string, stdin string) (string, string, error, bool) {
		calls.Add(1)
		<-ctx.Done()
		return "", "", ctx.Err(), true
	}

	regJSON := []byte(`{
  "version": 1,
  "provider_constraints": {},
  "risk_classes": {"default": {}},
  "lanes": [
    {"id": "perf-cost-guard", "route_shape": "bounded", "risk_class": "default"},
    {"id": "qa-sentinel", "route_shape": "qa-light", "risk_class": "default"}
  ]
}`)
	reg, err := resolve.ParseRegistry(regJSON)
	if err != nil {
		t.Fatal(err)
	}
	e := usage.NewQuotaEngine()
	sr := NewRouter(e, map[string]usage.BurnState{})
	InstallReadPathProbes(sr)
	sr.Probes.CLIPresent = func(string) bool { return true }

	scorer := &resolve.DefaultAdapter{
		ScoreFn: func(shape string, preferProvider string) *resolve.RouteScore {
			rt, err := sr.Pick(shape, preferProvider, "")
			if err != nil {
				return nil
			}
			return &resolve.RouteScore{
				Provider:  rt.Provider,
				Model:     rt.Model,
				Effort:    rt.Effort,
				QuotaPool: rt.QuotaPool,
			}
		},
	}
	resolver := resolve.New(reg, scorer)
	start := time.Now()
	results := resolver.ResolveAll()
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("ResolveAll took %s; read path must not wait on live probes", elapsed)
	}
	if calls.Load() != 0 {
		t.Fatalf("read path invoked live provider probe %d times; want 0", calls.Load())
	}
	out, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("JSON must be non-empty")
	}
	blob := string(out)
	if !strings.Contains(blob, "perf-cost-guard") || !strings.Contains(blob, "qa-sentinel") {
		t.Fatalf("JSON missing lane ids: %s", blob)
	}
	if len(results) != 2 {
		t.Fatalf("results=%d want 2", len(results))
	}
}
