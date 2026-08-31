package router

import (
	"encoding/json"
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
	// FAC-615: a HEALTHY ALTERNATE must exist, or a fallthrough test cannot
	// distinguish "did not fall through" from "fell through and found nothing".
	// The live incident had exactly this shape: codex unable to take work while
	// grok 4.6 was healthy and direct herdr launch used it successfully.
	computed["grok"] = usage.BurnState{Available: true, Reason: "ok"}
	r := testRouter(computed, "codex", "grok")

	prev := r.Probes
	r.Probes = &Probes{
		CLIPresent: prev.CLIPresent,
		Now:        prev.Now,
		LiveCount: func(provider, model, pool string) (int, error) {
			// Only the PREFERRED provider is constrained; the alternate is free.
			if !strings.EqualFold(provider, "codex") {
				return 0, nil
			}
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

func TestMachineRouteRecordsConcurrencySeparatelyFromHealthyQuota(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(map[string]usage.BurnState{
		"codex": {
			Available: true, Reason: "ok", Remaining: 79, Class: usage.BurnOverpace,
			Pools: map[string]usage.BurnState{
				"default": {Available: true, Reason: "ok", Remaining: 79, Class: usage.BurnOverpace},
			},
		},
		"grok": {Available: true, Reason: "ok", Remaining: 100, Class: usage.BurnUnderspent},
	}, "codex", "grok")
	r.Probes.LiveCount = func(provider, _, _ string) (int, error) {
		if provider == "codex" {
			return 3, nil
		}
		return 0, nil
	}

	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("healthy Grok fallback was not selected: %v", err)
	}
	if route.Provider != "grok" {
		t.Fatalf("selected %s, want Grok while Codex concurrency is full", route.Provider)
	}
	raw, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	var machine struct {
		Rejections []struct {
			Provider string `json:"provider"`
			Gate     string `json:"gate"`
			Detail   string `json:"detail"`
		} `json:"rejections"`
	}
	if err := json.Unmarshal(raw, &machine); err != nil {
		t.Fatal(err)
	}
	for _, rejection := range machine.Rejections {
		if rejection.Provider != "codex" {
			continue
		}
		if rejection.Gate != "concurrency" || !strings.Contains(rejection.Detail, "live=3 cap=1") {
			t.Fatalf("Codex rejection did not preserve the concurrency cause: %+v", rejection)
		}
		if strings.Contains(strings.ToLower(rejection.Detail), "quota") {
			t.Fatalf("healthy Codex quota was blamed for its occupied slot: %+v", rejection)
		}
		return
	}
	t.Fatalf("machine route omitted the rejected Codex surface: %s", raw)
}

func TestMachineRouteKeepsSparkPoolEvidenceWhenSharedCodexAccountIsFull(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(map[string]usage.BurnState{
		"codex": {
			Available: false, Reason: "weekly-exhausted",
			Pools: map[string]usage.BurnState{
				"default": {Available: false, Reason: "weekly-exhausted", Class: usage.BurnExhausted},
				"spark":   {Available: true, Reason: "ok", Remaining: 100, Class: usage.BurnUnderspent},
			},
		},
		"grok": {Available: true, Reason: "ok", Remaining: 100, Class: usage.BurnUnderspent},
	}, "codex", "grok")
	r.Probes.LiveCount = func(provider, _, _ string) (int, error) {
		if provider == "codex" {
			return 3, nil
		}
		return 0, nil
	}

	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("healthy Grok fallback was not selected: %v", err)
	}
	if route.Provider != "grok" {
		t.Fatalf("selected %s, want Grok while the shared Codex account is full", route.Provider)
	}
	for _, rejection := range route.Rejections {
		if rejection.Provider != "codex" {
			continue
		}
		if rejection.Model != "gpt-5.3-codex-spark" || rejection.Pool != "spark" {
			t.Fatalf("Codex rejection lost the exact attempted Spark pool: %+v", rejection)
		}
		if rejection.Gate != "concurrency" || !strings.Contains(rejection.Detail, "live=3 cap=3") {
			t.Fatalf("Spark rejection did not preserve shared-account concurrency: %+v", rejection)
		}
		if strings.Contains(strings.ToLower(rejection.Detail), "quota") {
			t.Fatalf("healthy Spark quota was blamed for shared-account occupancy: %+v", rejection)
		}
		return
	}
	t.Fatal("machine route omitted the rejected Codex Spark surface")
}

func TestMachineAvailabilityGatePreservesUnknownAndCooldown(t *testing.T) {
	for _, tc := range []struct {
		detail string
		want   string
	}{
		{detail: "at concurrency cap live=3 cap=1", want: "concurrency"},
		{detail: "live concurrency unknown: herdr census unreadable", want: "unknown"},
		{detail: "global cooldown: quota backoff", want: "cooldown"},
		{detail: "quota exhausted", want: "quota"},
		{detail: "quota handoff unavailable: command not found", want: "quota-handoff"},
	} {
		if got := availabilityGate(tc.detail); got != tc.want {
			t.Errorf("availabilityGate(%q) = %q, want %q", tc.detail, got, tc.want)
		}
	}
}
