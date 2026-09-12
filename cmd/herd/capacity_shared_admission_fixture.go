//go:build herdfixture

package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
	"github.com/Kampe/Herdforge/pkg/resources"
)

// fixtureAdmissionEnv names the readings a test subprocess wants the capacity
// census to decide against.
//
// Tests that exercise something OTHER than resource admission -- contract
// ownership, pool mutation, surface preparation -- were failing on a loaded CI
// runner's real load average before they reached their own assertions. They
// need a known host, not a disabled gate.
const fixtureAdmissionEnv = "HERD_FIXTURE_ADMISSION"

// withSharedAdmission is the FIXTURE twin, compiled only under the herdfixture
// build tag. The shipped binary is built without that tag and therefore has no
// seam, no env var and no branch: this is not a production override that tests
// happen to use, it is code the release does not contain.
//
// The substitution is the OBSERVATION, never the policy. The readings below run
// through the real resources.Decide with the real DefaultLimits, so a fixture
// asking for a saturated host still gets a refusal, and the refusal text a test
// asserts on is the production one.
func withSharedAdmission(o CapacityObservation) CapacityObservation {
	requested := strings.TrimSpace(os.Getenv(fixtureAdmissionEnv))
	if requested == "" {
		admission := resources.Admit(context.Background())
		o.admission = &admission
		return o
	}

	now := time.Now()
	var load resources.CPULoad
	var head resources.MemHeadroom
	switch requested {
	case "healthy":
		load = resources.CPULoad{Load1: 1, CPUs: 8, Normalized: 0.125}
		head = resources.MemHeadroom{Pressure: resources.PressureNormal, FreePct: 80, FreePctGates: true}
	case "cpu-saturated":
		load = resources.CPULoad{Load1: 16, CPUs: 8, Normalized: 2}
		head = resources.MemHeadroom{Pressure: resources.PressureNormal, FreePct: 80, FreePctGates: true}
	case "memory-pressure":
		load = resources.CPULoad{Load1: 1, CPUs: 8, Normalized: 0.125}
		head = resources.MemHeadroom{Pressure: resources.PressureCritical, FreePct: 80, FreePctGates: true}
	default:
		// An unrecognised request is a fixture bug. Fall back to observing the
		// real host rather than inventing a healthy one.
		admission := resources.Admit(context.Background())
		o.admission = &admission
		return o
	}

	decision := resources.Decide(now,
		freshness.Fresh(fixtureAdmissionEnv, now, load),
		freshness.Fresh(fixtureAdmissionEnv, now, head),
		resources.DefaultLimits())
	o.admission = &decision
	return o
}
