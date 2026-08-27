package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/usage"
)

// Built on the existing hermetic testRouter rather than a parallel fixture: a
// second router constructor would drift from the real availability rule, which
// is precisely the class of bug under test.
type liveMode int

const (
	liveBelowCap liveMode = iota
	liveAtCap
	liveUnknown
)

type quotaMode int

const (
	quotaHealthy quotaMode = iota
	quotaExhausted
)

func newStandingTestRouter(t *testing.T, q quotaMode, l liveMode) *SurfaceRouter {
	t.Helper()
	clearRouteEnv(t)

	// Quota must be KNOWN in both cases. An empty map makes quotaState report
	// not-ok, and spent() then returns early on unknown quota -- correctly, since
	// unknown is not exhaustion, but it means concurrency is never consulted.
	// The live incident had known-HEALTHY quota and an occupied slot, so that is
	// what the fixture has to reproduce.
	// Computed is keyed by PROVIDER (aliased), not by pool -- pools nest inside
	// BurnState.Pools. Keying it by pool name silently yields not-ok, which
	// looks exactly like unknown quota and made the first version of this
	// fixture fail against working code.
	computed := map[string]usage.BurnState{}
	if q == quotaExhausted {
		computed["codex"] = usage.BurnState{Available: false, Reason: "weekly-exhausted"}
	} else {
		computed["codex"] = usage.BurnState{Available: true, Reason: "ok"}
	}
	r := testRouter(computed, "codex")

	prev := r.Probes
	r.Probes = &Probes{
		CLIPresent: prev.CLIPresent,
		Now:        prev.Now,
		LiveCount: func(provider, model, pool string) (int, error) {
			switch l {
			case liveUnknown:
				return 0, errors.New("herdr census unreadable")
			case liveAtCap:
				return 99, nil // above any ClassConcurrency cap
			default:
				return 0, nil
			}
		},
	}
	return r
}

// FAC-615: four standing lanes -- platform-ops, api-crusader, chain-indexer,
// nft-data-engineer -- refused with "no healthy launch candidate" while
// `route implementation` and `resolve-lane` for those same lanes selected
// healthy surfaces. Codex weekly quota was AVAILABLE; its concurrency slot was
// OCCUPIED.
//
// standingProviderSpent measured only quota, so it reported "not spent", the
// standing family boundary stayed hard, and the lanes died beside a healthy
// alternate. FAC-684 had already established the boundary holds only while the
// preferred provider can actually take work; quota was the wrong measure of
// that.

// A provider at concurrency cap cannot take work, whatever its quota says.
func TestConcurrencyCapReleasesTheStandingBoundary(t *testing.T) {
	r := newStandingTestRouter(t, quotaHealthy, liveAtCap)

	if !r.standingProviderSpent("codex", "gpt-5.6-luna") {
		t.Fatal("codex at concurrency cap with healthy quota was reported NOT spent; " +
			"the standing boundary stays hard and the lane refuses beside a healthy alternate")
	}
}

// The boundary must still hold when the provider genuinely can take work --
// otherwise a standing lane wanders families for no reason.
func TestAHealthyProviderKeepsTheStandingBoundary(t *testing.T) {
	r := newStandingTestRouter(t, quotaHealthy, liveBelowCap)

	if r.standingProviderSpent("codex", "gpt-5.6-luna") {
		t.Fatal("a provider with healthy quota and a free slot was reported spent; " +
			"the standing preference is being abandoned for no proven reason")
	}
}

// THE safety property, unchanged from the quota rule this extends: unknown is
// not evidence of exhaustion.
func TestUnknownConcurrencyDoesNotReleaseTheBoundary(t *testing.T) {
	r := newStandingTestRouter(t, quotaHealthy, liveUnknown)

	if r.standingProviderSpent("codex", "gpt-5.6-luna") {
		t.Fatal("unreadable live concurrency released the family boundary; " +
			"not knowing is not proof of exhaustion, and widening on ignorance " +
			"lets a standing lane wander families")
	}
}

// Exhausted quota must still release it -- this change adds a second reason,
// it does not replace the first.
func TestExhaustedQuotaStillReleasesTheBoundary(t *testing.T) {
	r := newStandingTestRouter(t, quotaExhausted, liveBelowCap)

	if !r.standingProviderSpent("codex", "gpt-5.6-luna") {
		t.Fatal("exhausted quota no longer releases the boundary; FAC-684 regressed")
	}
}

// The reason string is what an operator reads. "no healthy launch candidate"
// blamed capacity for what was really an occupied slot, and that
// misattribution is why this took a fleet incident to find.
func TestTheConcurrencyReasonIsDistinguishableFromQuota(t *testing.T) {
	r := newStandingTestRouter(t, quotaHealthy, liveAtCap)

	_, reason := r.available("codex", "gpt-5.6-luna", QuotaPoolFor("codex", "gpt-5.6-luna"))
	if !strings.Contains(reason, "concurrency") {
		t.Fatalf("reason %q does not name concurrency; an operator cannot tell an occupied slot from spent quota", reason)
	}
	if strings.Contains(strings.ToLower(reason), "quota") {
		t.Fatalf("reason %q blames quota for a concurrency condition", reason)
	}
}
