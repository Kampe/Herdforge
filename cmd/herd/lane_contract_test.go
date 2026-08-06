package main

import (
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// The suite was fully green while six of nine lanes launched on a model the
// operator never configured — including a Sonnet->Opus reviewer escalation.
// Nothing tested the lane-config -> decision contract, so this asserts it
// directly over the REAL roster and fails on the next regression of that class.
func TestEveryConfiguredLaneLaunchesOnItsConfiguredModel(t *testing.T) {
	cfg, err := config.LoadConfig("../../.herd/herd.yaml")
	if err != nil {
		// NOT Skipf. A self-disabling gate is the exact class of defect this
		// suite exists to catch — a test that silently stops running when its
		// input moves protects nothing.
		t.Fatalf("real roster unavailable: %v", err)
	}
	if len(cfg.Lanes) == 0 {
		t.Fatal("roster has no lanes")
	}

	// Every surface healthy and present, so any divergence is the routing
	// mechanism rather than quota or a missing CLI.
	healthy := map[string]usage.BurnState{}
	for _, p := range []string{"claude", "codex", "grok", "agy", "ollama", "opencode", "lazer", "kimi"} {
		healthy[p] = usage.BurnState{Available: true, Pressure: 10}
	}

	for _, lane := range cfg.Lanes {
		lane := lane
		t.Run(lane.Name, func(t *testing.T) {
			t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
			r := router.NewRouter(usage.NewQuotaEngine(), healthy)
			r.Probes = &router.Probes{
				CLIPresent: func(string) bool { return true },
				Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
			}

			req := router.LaunchRequest{
				Role:              router.Role(lane.Role),
				Shape:             lane.TaskShape,
				TaskRef:           lane.Name,
				Scope:             router.ScopeLane,
				PreferredProvider: lane.Provider,
				PreferredModel:    lane.Model,
				ProbeResults:      map[string]bool{},
			}
			// Reviewers need author provenance; give them a disjoint family.
			if req.Role == router.RoleReviewer || req.Role == router.RoleAssayer {
				req.AuthorFamily = "xai"
				req.AuthorModel = "grok-4.5"
				req.CandidateSHA = "0123456789abcdef0123456789abcdef01234567"
				req.Scope = router.ScopeCandidate
			}
			// Pass every probe so a probe-gated preference is reachable.
			for _, p := range []string{"codex", "claude", "grok", "agy", "ollama", "opencode", "lazer"} {
				for _, m := range []string{router.ModelFor(p, lane.TaskShape), lane.Model} {
					if m != "" && router.ModelRequiresProbe(m) {
						req.ProbeResults[router.ProbeKey(p, m)] = true
					}
				}
			}

			d, err := r.Decide(req)
			if err != nil {
				// A lane whose configured surface is not in its shape's
				// waterfall cannot be honoured at all; that is a config
				// problem worth naming loudly rather than skipping.
				t.Fatalf("lane %s (%s/%s, shape %s) could not route: %v",
					lane.Name, lane.Provider, lane.Model, lane.TaskShape, err)
			}
			if d.Provider != lane.Provider || d.Model != lane.Model {
				t.Errorf("lane %s configured %s/%s but routes to %s/%s (effort=%s) — "+
					"a healthy lane must launch on what the operator configured",
					lane.Name, lane.Provider, lane.Model, d.Provider, d.Model, d.Effort)
			}
		})
	}
}
