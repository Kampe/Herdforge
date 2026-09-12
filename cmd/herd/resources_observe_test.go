package main

// FAC-829: the CLI half. These exercise the actual dispatch functions
// `herd resources --watch` and `--observer-status` route to, including path
// resolution and exit codes. Nothing here starts an observer against the host:
// the watch cases are refused during validation, before any probe runs.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// cliAdmittingReport mirrors a real published decision: fresh readings with
// their own observation times and the numbers behind them. A skeleton report
// would pass the consumer check for the wrong reason.
func cliAdmittingReport(at time.Time) resources.AdmissionReport {
	normalized := 0.2
	load1 := 1.6
	cpus := 8
	freePct := 60
	stamp := at.UTC().Format(time.RFC3339Nano)
	return resources.AdmissionReport{
		Decision:   "ADMIT",
		Admits:     true,
		Verdict:    "OK",
		RenderedAt: stamp,
		Limits:     resources.DefaultLimits(),
		CPU: resources.CPUReport{
			ReadingReport: resources.ReadingReport{State: "FRESH", Known: true, ObservedAt: stamp, Source: "test cpu"},
			Load1:         &load1,
			CPUs:          &cpus,
			Normalized:    &normalized,
		},
		Memory: resources.MemoryReport{
			ReadingReport: resources.ReadingReport{State: "FRESH", Known: true, ObservedAt: stamp, Source: "test memory"},
			Pressure:      "normal",
			PressureKnown: true,
			FreePct:       &freePct,
		},
	}
}

// cliSample wraps the report with the stamps a credible status must carry: a
// status that cannot say when it was taken establishes nothing.
func cliSample(at time.Time) resources.ObserverSample {
	return resources.ObserverSample{
		StartedAt:   at.Add(-time.Second).UTC().Format(time.RFC3339Nano),
		CompletedAt: at.UTC().Format(time.RFC3339Nano),
		Report:      cliAdmittingReport(at),
	}
}

func writeObserverFixture(t *testing.T, status resources.ObserverStatus) string {
	t.Helper()
	path := resources.ObserverStatusPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create observer directory: %v", err)
	}
	body, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("encode fixture status: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write fixture status: %v", err)
	}
	return path
}

// TestObserverStatusCommandFailsClosed proves the consumer path treats absence,
// expiry and termination as refusals rather than as permission.
func TestObserverStatusCommandFailsClosed(t *testing.T) {
	now := time.Now().UTC()

	t.Run("missing status refuses", func(t *testing.T) {
		t.Setenv("HERD_STATE_DIR", t.TempDir())
		if code := runResourcesObserverStatus(false); code != observerExitRefused {
			t.Fatalf("a missing observer status exited %d, expected %d", code, observerExitRefused)
		}
	})

	t.Run("expired status refuses", func(t *testing.T) {
		t.Setenv("HERD_STATE_DIR", t.TempDir())
		writeObserverFixture(t, resources.ObserverStatus{
			SchemaVersion: resources.ObserverSchemaVersion,
			PublishedAt:   now.Add(-time.Hour).Format(time.RFC3339Nano),
			ExpiresAt:     now.Add(-time.Minute).Format(time.RFC3339Nano),
			Latest:        cliSample(now),
		})
		if code := runResourcesObserverStatus(false); code != observerExitRefused {
			t.Fatalf("an expired observer status exited %d, expected %d: a dead observer must authorize nothing",
				code, observerExitRefused)
		}
	})

	t.Run("terminated observer refuses", func(t *testing.T) {
		t.Setenv("HERD_STATE_DIR", t.TempDir())
		writeObserverFixture(t, resources.ObserverStatus{
			SchemaVersion: resources.ObserverSchemaVersion,
			ExpiresAt:     now.Add(time.Hour).Format(time.RFC3339Nano),
			Terminated:    "lifetime reached",
			Latest:        cliSample(now),
		})
		if code := runResourcesObserverStatus(false); code != observerExitRefused {
			t.Fatalf("a terminated observer exited %d, expected %d", code, observerExitRefused)
		}
	})

	t.Run("current admitting status succeeds", func(t *testing.T) {
		t.Setenv("HERD_STATE_DIR", t.TempDir())
		writeObserverFixture(t, resources.ObserverStatus{
			SchemaVersion: resources.ObserverSchemaVersion,
			PublishedAt:   now.Format(time.RFC3339Nano),
			ExpiresAt:     now.Add(time.Hour).Format(time.RFC3339Nano),
			Latest:        cliSample(now),
		})
		if code := runResourcesObserverStatus(false); code != 0 {
			t.Fatalf("a current admitting status exited %d, expected 0", code)
		}
	})

	t.Run("refusing decision refuses", func(t *testing.T) {
		t.Setenv("HERD_STATE_DIR", t.TempDir())
		writeObserverFixture(t, resources.ObserverStatus{
			SchemaVersion: resources.ObserverSchemaVersion,
			PublishedAt:   now.Format(time.RFC3339Nano),
			ExpiresAt:     now.Add(time.Hour).Format(time.RFC3339Nano),
			Latest: resources.ObserverSample{
				StartedAt:   now.Add(-time.Second).Format(time.RFC3339Nano),
				CompletedAt: now.Format(time.RFC3339Nano),
				Report:      resources.AdmissionReport{Decision: "REFUSE", Admits: false},
			},
		})
		if code := runResourcesObserverStatus(false); code != observerExitRefused {
			t.Fatalf("a refusing decision exited %d, expected %d", code, observerExitRefused)
		}
	})
}

// TestObserverWatchRefusesBadBoundsBeforeSampling proves an out-of-range flag
// is rejected during validation, so no probe, lock or file write happens.
func TestObserverWatchRefusesBadBoundsBeforeSampling(t *testing.T) {
	// Every case here MUST fail validation. A case that validated would start a
	// real observer against the host for its full lifetime, which is exactly
	// what must never happen in a unit test.
	set := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	cases := map[string]observerFlags{
		"interval below floor":    {interval: time.Millisecond, provided: set("interval")},
		"interval above ceil":     {interval: 24 * time.Hour, provided: set("interval")},
		"lifetime below floor":    {lifetime: time.Second, provided: set("lifetime")},
		"lifetime above ceil":     {lifetime: 30 * 24 * time.Hour, provided: set("lifetime")},
		"timeout beyond half":     {interval: 30 * time.Second, sampleTimeout: 25 * time.Second, provided: set("interval", "sample-timeout")},
		"explicit zero interval":  {interval: 0, provided: set("interval")},
		"explicit zero timeout":   {interval: 30 * time.Second, sampleTimeout: 0, provided: set("interval", "sample-timeout")},
		"explicit negative tmout": {interval: 30 * time.Second, sampleTimeout: -time.Second, provided: set("interval", "sample-timeout")},
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HERD_STATE_DIR", t.TempDir())
			if code := runResourcesObserver(flags); code != observerExitRefused {
				t.Fatalf("%s exited %d, expected %d", name, code, observerExitRefused)
			}
			// Nothing may have been published by a refused start.
			if _, err := os.Stat(resources.ObserverStatusPath()); err == nil {
				t.Fatalf("%s wrote a status file despite being refused", name)
			}
		})
	}
}
