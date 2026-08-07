package launch

import (
	"context"
	"fmt"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolprobe"
)

// MaxProbeFailovers caps deterministic candidate retries after classified
// tool-probe failures (FAC-139).
const MaxProbeFailovers = 12

// DecideWithToolProbe selects a LaunchDecision and proves tool execution for
// the chosen surface. Classified non-PASS results mark that candidate false
// and re-ask the router for the next healthy compatible candidate.
//
// cache may be nil (no persistence). runner is required on cache miss.
func DecideWithToolProbe(
	ctx context.Context,
	r *router.SurfaceRouter,
	req router.LaunchRequest,
	cache toolprobe.Cache,
	runner toolprobe.Runner,
	now time.Time,
) (*router.LaunchDecision, *toolprobe.Receipt, error) {
	if r == nil {
		return nil, nil, fmt.Errorf("launch: SurfaceRouter is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if req.ProbeResults == nil {
		req.ProbeResults = map[string]bool{}
	}
	// Seed request probe map from cache so known INCAPABLE surfaces are skipped
	// before Decide ranks them.
	seedProbeResultsFromCache(req, cache, now)

	var lastReceipt *toolprobe.Receipt
	for attempt := 0; attempt < MaxProbeFailovers; attempt++ {
		d, err := r.Decide(req)
		if err != nil {
			if lastReceipt != nil {
				return nil, lastReceipt, fmt.Errorf("no healthy tool-capable candidate: %w (last probe %s: %s)", err, lastReceipt.Status, lastReceipt.Reason)
			}
			return nil, nil, err
		}
		id, err := toolprobe.IdentityFromDecision(d)
		if err != nil {
			return nil, nil, err
		}
		receipt, err := toolprobe.Ensure(ctx, id, cache, runner, now)
		if err != nil {
			return nil, nil, err
		}
		lastReceipt = &receipt
		key := router.ProbeKey(d.Provider, d.Model)
		if receipt.Passes(now) {
			req.ProbeResults[key] = true
			return d, &receipt, nil
		}
		// Fail closed for this candidate and ask the router for the next one.
		req.ProbeResults[key] = false
		if cache != nil {
			_ = cache.Put(receipt)
		}
	}
	if lastReceipt != nil {
		return nil, lastReceipt, fmt.Errorf("tool-probe failover exhausted after %d attempts (last %s: %s)", MaxProbeFailovers, lastReceipt.Status, lastReceipt.Reason)
	}
	return nil, nil, fmt.Errorf("tool-probe failover exhausted after %d attempts", MaxProbeFailovers)
}

func seedProbeResultsFromCache(req router.LaunchRequest, cache toolprobe.Cache, now time.Time) {
	if cache == nil || req.ProbeResults == nil {
		return
	}
	// We cannot enumerate the whole cache without a List API; callers that know
	// specific keys should pre-fill ProbeResults. Cached INCAPABLE for the
	// requested model is applied when RequestedProvider/Model are set.
	if req.RequestedProvider == "" || req.RequestedModel == "" {
		return
	}
	id := toolprobe.Identity{
		Provider:  req.RequestedProvider,
		Model:     req.RequestedModel,
		Harness:   router.PiHarness,
		Recipe:    toolprobe.RecipeArtifactWrite,
		Toolchain: toolprobe.ToolchainV1,
	}
	if r, ok := toolprobe.LookupFresh(cache, id, now); ok && !r.Status.WriteCapable() {
		req.ProbeResults[router.ProbeKey(id.Provider, id.Model)] = false
	}
}
