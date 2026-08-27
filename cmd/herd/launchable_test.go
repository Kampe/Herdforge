package main

import (
	"os/exec"
	"strings"
	"testing"
)

// FAC-609: capacity reported six free review slots while every provider surface
// was at concurrency cap, and two launches failed against that wall.

func TestProviderConcurrencyBindsTheMemoryCeiling(t *testing.T) {
	// The exact observed state: memory says six, providers say none.
	c := Capacity{AvailableSlots: 6, Admit: true, Reason: "host can host another reviewer"}
	applyLaunchable(&c, launchable{Slots: 0, Known: true, Detail: "provider concurrency reports 0 launchable slot(s)"})

	if c.AvailableSlots != 0 {
		t.Fatalf("available_slots = %d; capacity is still offering slots no provider can accept", c.AvailableSlots)
	}
	if c.Admit {
		t.Fatal("capacity still admits a launch that cannot succeed")
	}
	if !c.LaunchableBinding {
		t.Fatal("the binding constraint is not marked, so an operator cannot tell which ceiling bit")
	}
	if !strings.Contains(c.Reason, "provider concurrency") {
		t.Fatalf("reason does not name provider concurrency as the constraint: %s", c.Reason)
	}
	// The action differs by constraint: freeing memory does nothing here.
	if !strings.Contains(c.Reason, "do not free memory") {
		t.Fatalf("reason does not tell the operator which action would actually help: %s", c.Reason)
	}
}

// THE property this must not get wrong. An unreadable secondary authority is
// UNKNOWN. Refusing on it would make a missing helper binary an outage -- the
// exact "a gate that refuses whenever it cannot measure is an outage generator"
// failure capacity.go warns about elsewhere.
func TestAnUnreadableRouterNeverReducesTheCeiling(t *testing.T) {
	c := Capacity{AvailableSlots: 6, Admit: true, Reason: "host can host another reviewer"}
	applyLaunchable(&c, launchable{Known: false, Detail: "herdr-route not on PATH; provider concurrency not consulted"})

	if c.AvailableSlots != 6 {
		t.Fatalf("available_slots = %d; an unreadable router reduced the ceiling, turning a missing binary into an outage", c.AvailableSlots)
	}
	if !c.Admit {
		t.Fatal("capacity refused because a SECONDARY authority could not be read")
	}
	if c.LaunchableBinding {
		t.Fatal("an unknown reading was marked as the binding constraint")
	}
	if !strings.Contains(c.LaunchableDetail, "unknown") {
		t.Fatalf("detail does not mark the reading unknown: %s", c.LaunchableDetail)
	}
}

// Memory must still bind when it is the smaller ceiling. This fix adds a second
// constraint; it must not replace the first.
func TestMemoryStillBindsWhenItIsTheSmallerCeiling(t *testing.T) {
	c := Capacity{AvailableSlots: 1, Admit: true}
	applyLaunchable(&c, launchable{Slots: 5, Known: true, Detail: "provider concurrency reports 5 launchable slot(s)"})

	if c.AvailableSlots != 1 {
		t.Fatalf("available_slots = %d; a larger provider ceiling raised the memory-derived one", c.AvailableSlots)
	}
	if c.LaunchableBinding {
		t.Fatal("provider concurrency marked binding when memory was the smaller ceiling")
	}
	if !c.Admit {
		t.Fatal("capacity refused with a slot available under both ceilings")
	}
}

// Surfaces are pooled per provider, not per model row: three claude model lines
// share one claude concurrency pool. Summing rows would re-invent the
// overstatement this card exists to remove.
func TestModelRowsOfOneSurfaceDoNotMultiplySlots(t *testing.T) {
	doctor := "" +
		"claude           READY  69   claude-sonnet-5    available live=1 cap=2\n" +
		"claude           READY  69   claude-fable-5     available live=1 cap=2\n" +
		"claude           READY  69   claude-haiku-4-5   available live=1 cap=2\n" +
		"grok             SKIP   19   grok-4.5           at concurrency cap live=4 cap=4\n"

	got := parseDoctorSurfaces(doctor)
	if !got.Known {
		t.Fatal("parsable doctor output reported unknown")
	}
	if got.Slots != 1 {
		t.Fatalf("slots = %d, want 1: three claude model rows share ONE pool with cap=2 live=1", got.Slots)
	}
}

func TestAllSurfacesAtCapYieldsZeroLaunchable(t *testing.T) {
	doctor := "" +
		"claude           SKIP   73   claude-sonnet-5  at concurrency cap live=2 cap=2\n" +
		"grok             SKIP   19   grok-4.5         at concurrency cap live=4 cap=4\n" +
		"codex            SKIP   108  gpt-5.6-luna     at concurrency cap live=4 cap=1\n" +
		"agy              SKIP   125  gemini-3.1-pro   quota unavailable: exhausted\n"

	got := parseDoctorSurfaces(doctor)
	if !got.Known {
		t.Fatal("parsable doctor output reported unknown")
	}
	if got.Slots != 0 {
		t.Fatalf("slots = %d, want 0 with every surface at cap", got.Slots)
	}
}

// Output that parses to no surface rows at all is UNKNOWN, not zero. A router
// whose format changed must not read as "nothing can launch".
func TestUnrecognisedDoctorOutputIsUnknownNotZero(t *testing.T) {
	got := parseDoctorSurfaces("some future format nobody here can parse\n")
	if got.Known {
		t.Fatal("unrecognised output claimed to be a known reading of zero")
	}
}

// FAC-609: the "no cached reading" detail tells an operator to run
// `herd capacity --refresh-launchable`. A diagnostic naming a recovery that
// does not exist is the defect FAC-592 fixed for the broker, and this message
// shipped pointing at an UNWIRED flag until it was checked.
//
// This drives the REAL binary. A first version of this test registered a mirror
// flag set and asserted on that -- which would have proved the mirror had the
// flag while runCapacity did not. That is the vacuous shape that produced an
// independent FAIL on #631 and a merged defect in FAC-602 on the same day.
func TestTheNamedRecoveryFlagActuallyExists(t *testing.T) {
	l := launchable{Known: false, Detail: "no cached provider-concurrency reading (run `herd capacity --refresh-launchable`)"}
	c := Capacity{AvailableSlots: 4, Admit: true}
	applyLaunchable(&c, l)
	if !strings.Contains(c.LaunchableDetail, "--refresh-launchable") {
		t.Fatalf("detail no longer names the recovery: %s", c.LaunchableDetail)
	}

	// An INVALID value for the flag. If the flag is defined, the parser
	// complains about the value; if it is not, it complains that the flag is
	// undefined. Either way it fails fast, so this never triggers the 20-90s
	// router probe.
	//
	// The obvious version of this test used --help, which short-circuits before
	// flag-parse errors -- it passed even with the flag RENAMED, proving
	// nothing. That was caught by renaming the flag and watching the test stay
	// green, which is the only reason this version exists.
	binary := buildHerd(t)
	out, _ := exec.Command(binary, "capacity", "--refresh-launchable=not-a-bool").CombinedOutput()
	if strings.Contains(string(out), "not defined") {
		t.Fatalf("capacity does not define --refresh-launchable, but its own diagnostic tells operators to run it:\n%s", out)
	}
	if !strings.Contains(string(out), "invalid boolean value") {
		t.Fatalf("could not confirm the flag is registered; parser said:\n%s", out)
	}
}
