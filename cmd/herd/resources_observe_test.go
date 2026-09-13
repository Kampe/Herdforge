package main

// FAC-829: the CLI half. These exercise the actual dispatch functions
// `herd resources --watch` and `--observer-status` route to, including path
// resolution and exit codes. Nothing here starts an observer against the host:
// the watch cases are refused during validation, before any probe runs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// A published report carries BOTH decision stamps. Omitting DecidedAt made
	// this "valid" fixture invalid, and the production reader was right to
	// refuse it: a report that does not say when it was decided cannot show
	// that anything it carries is current. The fixture is corrected rather than
	// the rule relaxed. Decided at the observation instant and rendered no
	// earlier, which is the order a real decision produces.
	decided := at.UTC().Add(-10 * time.Millisecond).Format(time.RFC3339Nano)
	return resources.AdmissionReport{
		Decision:   "ADMIT",
		Admits:     true,
		Verdict:    "OK",
		DecidedAt:  decided,
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

	// The decision stamps get their own negative cases so the positive fixture
	// can never again be the only thing covering them. A missing stamp used to
	// be discovered as a broken "valid" fixture; it is asserted here instead.
	for _, tc := range []struct {
		name   string
		mutate func(*resources.AdmissionReport)
	}{
		{"missing decided_at refuses", func(r *resources.AdmissionReport) { r.DecidedAt = "" }},
		{"missing rendered_at refuses", func(r *resources.AdmissionReport) { r.RenderedAt = "" }},
		{"rendered before decided refuses", func(r *resources.AdmissionReport) {
			r.RenderedAt = now.Add(-time.Hour).Format(time.RFC3339Nano)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HERD_STATE_DIR", t.TempDir())
			sample := cliSample(now)
			tc.mutate(&sample.Report)
			writeObserverFixture(t, resources.ObserverStatus{
				SchemaVersion: resources.ObserverSchemaVersion,
				PublishedAt:   now.Format(time.RFC3339Nano),
				ExpiresAt:     now.Add(time.Hour).Format(time.RFC3339Nano),
				Latest:        sample,
			})
			if code := runResourcesObserverStatus(false); code != observerExitRefused {
				t.Fatalf("%s exited %d, expected %d", tc.name, code, observerExitRefused)
			}
		})
	}
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

// TestObserverExitCodeCannotReachSuccessAfterAPublishFailure is the CLI half of
// the propagation regression: whatever stopped the observer, a run whose
// terminal status failed to publish must not take the success branch.
func TestObserverExitCodeCannotReachSuccessAfterAPublishFailure(t *testing.T) {
	publishFailure := fmt.Errorf("%w: write refused", resources.ErrObserverPublishFailed)

	cases := map[string]struct {
		err  error
		want int
	}{
		"clean shutdown":                  {err: nil, want: 0},
		"publish failure alone":           {err: publishFailure, want: 1},
		"cancel joined with publish":      {err: errors.Join(context.Canceled, publishFailure), want: 1},
		"deadline joined with publish":    {err: errors.Join(context.DeadlineExceeded, publishFailure), want: 1},
		"another observer holds the lock": {err: fmt.Errorf("%w: held", resources.ErrObserverBusy), want: observerExitRefused},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := observerExitCodeFor(tc.err)
			if got != tc.want {
				t.Fatalf("%s produced exit %d, expected %d", name, got, tc.want)
			}
			if tc.want != 0 && got == 0 {
				t.Fatalf("%s reached the CLI success branch", name)
			}
		})
	}
}

// TestRunResourcesRejectsFlagsTheModeCannotHonour is the command-level
// contract: it drives the real argv path, not a helper.
//
// Every case here was previously ACCEPTED and silently ignored. --interval
// outside --watch was parsed and dropped, including an explicit --interval=0
// that the observer would have refused as invalid; --gate and --selftest under
// --watch were skipped by an early return, so a resource guard the operator
// asked for simply did not run and the command still exited 0. A discarded
// guard that reports success is worse than one that fails.
func TestRunResourcesRejectsFlagsTheModeCannotHonour(t *testing.T) {
	cases := map[string][]string{
		"observer bounds without watch":        {"--interval", "45s"},
		"explicit zero interval without watch": {"--interval=0"},
		"lifetime without watch":               {"--lifetime", "1h"},
		"sample timeout without watch":         {"--sample-timeout", "5s"},
		"gate under watch":                     {"--watch", "--gate"},
		"selftest under watch":                 {"--watch", "--selftest"},
		"json under watch":                     {"--watch", "--json"},
		"gate under observer status":           {"--observer-status", "--gate"},
		"selftest under observer status":       {"--observer-status", "--selftest"},
		"bounds under observer status":         {"--observer-status", "--interval", "30s"},
		"two modes at once":                    {"--watch", "--observer-status"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HERD_STATE_DIR", t.TempDir())
			var stdout, stderr bytes.Buffer
			code := runResourcesWithArgs(args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("%s exited %d, want 2: an unsupported mix must be refused, never ignored", name, code)
			}
			// No sampling and no observer side effect may have happened: the
			// refusal is decided before any observation.
			if stdout.Len() != 0 {
				t.Fatalf("%s produced stdout %q; a rejected invocation must observe nothing", name, stdout.String())
			}
			if _, err := os.Stat(resources.ObserverStatusPath()); err == nil {
				t.Fatalf("%s wrote an observer status despite being rejected", name)
			}
			if stderr.Len() == 0 {
				t.Fatalf("%s was refused without telling the operator why", name)
			}
		})
	}
}

// TestRunResourcesAcceptsTheStatusModeAndItsJSON keeps the rejection table
// honest: the combinations that ARE supported must still reach their job. Both
// cases read a published snapshot and never sample the host, so this stays
// hermetic.
func TestRunResourcesAcceptsTheStatusModeAndItsJSON(t *testing.T) {
	for name, args := range map[string][]string{
		"status alone":    {"--observer-status"},
		"status and json": {"--observer-status", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HERD_STATE_DIR", t.TempDir())
			var stdout, stderr bytes.Buffer
			// No snapshot exists, so the reader refuses with 3 -- which proves
			// the mode was DISPATCHED rather than refused at parse time.
			if code := runResourcesWithArgs(args, &stdout, &stderr); code != observerExitRefused {
				t.Fatalf("%s exited %d, want %d from the status reader", name, code, observerExitRefused)
			}
		})
	}
}

// TestValidateResourcesModeAcceptsEverySupportedCombination pins the accepted
// sets directly, including the one-shot and watch modes whose jobs this test
// deliberately does not run: a rejection table alone could pass by refusing
// everything.
func TestValidateResourcesModeAcceptsEverySupportedCombination(t *testing.T) {
	supported := []struct {
		name     string
		provided []string
		want     resourcesMode
	}{
		{"plain resources", nil, resourcesModeOneShot},
		{"one-shot json", []string{"json"}, resourcesModeOneShot},
		{"one-shot gate", []string{"gate"}, resourcesModeOneShot},
		{"one-shot selftest", []string{"selftest"}, resourcesModeOneShot},
		{"one-shot gate and json", []string{"gate", "json"}, resourcesModeOneShot},
		{"watch alone", []string{"watch"}, resourcesModeWatch},
		{"watch with every bound", []string{"watch", "interval", "lifetime", "sample-timeout"}, resourcesModeWatch},
		{"status alone", []string{"observer-status"}, resourcesModeStatus},
		{"status with json", []string{"observer-status", "json"}, resourcesModeStatus},
	}
	for _, tc := range supported {
		t.Run(tc.name, func(t *testing.T) {
			provided := map[string]bool{}
			for _, n := range tc.provided {
				provided[n] = true
			}
			mode, err := validateResourcesMode(provided)
			if err != nil {
				t.Fatalf("%s must be supported, got %v", tc.name, err)
			}
			if mode != tc.want {
				t.Fatalf("%s resolved to mode %q, want %q", tc.name, mode, tc.want)
			}
		})
	}
}
