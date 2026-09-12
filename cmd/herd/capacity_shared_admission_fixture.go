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
//
// It also pins the census fields the arms AFTER the admission read; see
// pinFixtureCensus. Without that the observation was only half controlled.
func withSharedAdmission(o CapacityObservation) CapacityObservation {
	// Pin the census arms FIRST, on every path: a tagged binary is a fixture
	// binary, and a half-controlled observation is what let runner load decide
	// a control's outcome.
	o = pinFixtureCensus(o)
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

// Pinned census values for the arms decideCapacity evaluates AFTER the shared
// admission: memory pressure, swap exhaustion, and per-reviewer headroom.
//
// Every one of those reads the RUNNER's own census. A fixture that only pinned
// the admission left them live, so a mutant that neutralises the admission arm
// could still be refused by a loaded runner -- reporting SURVIVED for a reason
// that has nothing to do with the guard under test -- and a runner that drifted
// between the baseline and a later mutant could fail the baseline instead. The
// healthy baseline proves those arms were quiet at ONE moment, not for the rest
// of the run.
//
// The values are deliberately far from every threshold, and the arms still
// execute: this pins the OBSERVATION, never the refusal logic.
const (
	fixturePressurePct  = 0.0  // memoryPressurePct is 20
	fixtureSwapTotalMiB = 8192 // swapExhaustedPct is 75; used stays 0
	fixtureSwapUsedMiB  = 0
	fixtureMemTotalMiB  = 65536
	// Distinctive on purpose: the pool gate prints mem_available, so a test can
	// assert this exact number and prove the seam reached the consumer.
	fixtureMemAvailMiB = 49152
)

// pinFixtureCensus replaces only the census fields the post-admission arms read.
// Herdr liveness, the agent census and the reviewer counts are left alone: the
// pool fixtures already control those through their own stubs, and overriding
// them would hide a real failure in that plumbing.
func pinFixtureCensus(o CapacityObservation) CapacityObservation {
	o.PressurePct = fixturePressurePct
	o.SwapTotalMiB = fixtureSwapTotalMiB
	o.SwapUsedMiB = fixtureSwapUsedMiB
	o.MemTotalMiB = fixtureMemTotalMiB
	o.MemAvailMiB = fixtureMemAvailMiB
	return o
}
