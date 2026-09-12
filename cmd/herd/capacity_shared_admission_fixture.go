//go:build herdfixture

package main

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
	"github.com/Kampe/Herdforge/pkg/resources"
)

// fixtureAdmissionEnv names the host readings a test subprocess decides against.
const fixtureAdmissionEnv = "HERD_FIXTURE_ADMISSION"

// withSharedAdmission is the fixture twin of the shipped attachment in
// capacity_shared_admission.go, compiled ONLY under the herdfixture tag. A
// release build carries neither this file nor any environment override.
//
// The contract: it substitutes the OBSERVATION and never the policy. The
// readings below run through the real resources.Decide with the real
// DefaultLimits, so a saturated or pressured request still refuses, with the
// production refusal text. An absent or unrecognised request refuses outright
// rather than falling back to the host, so a fixture typo cannot silently
// inherit whatever the runner happened to be doing.
func withSharedAdmission(o CapacityObservation) CapacityObservation {
	now := time.Now()
	var load resources.CPULoad
	var head resources.MemHeadroom

	switch strings.TrimSpace(os.Getenv(fixtureAdmissionEnv)) {
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
		// Deterministic refusal. An unset or misspelled request is a fixture
		// bug, and answering it from the live host would make the test's
		// outcome depend on CI load, which is the failure this seam exists to
		// remove.
		decision := resources.Decide(now,
			unknownFixtureReading[resources.CPULoad](),
			unknownFixtureReading[resources.MemHeadroom](),
			resources.DefaultLimits())
		o.admission = &decision
		return o
	}

	decision := resources.Decide(now,
		freshness.Fresh(fixtureAdmissionEnv, now, load),
		freshness.Fresh(fixtureAdmissionEnv, now, head),
		resources.DefaultLimits())
	o.admission = &decision
	return o
}

func unknownFixtureReading[T any]() freshness.Reading[T] {
	return freshness.Degrade(freshness.Reading[T]{}, fixtureAdmissionEnv,
		errUnsetFixtureAdmission, "set "+fixtureAdmissionEnv+" to healthy, cpu-saturated or memory-pressure")
}

var errUnsetFixtureAdmission = errors.New("no fixture host readings were requested")
