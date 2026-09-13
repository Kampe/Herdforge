package resources

// FAC-829 behaviour tests. Every one runs on a fake clock and injected probes:
// nothing here starts a process, touches the host, or waits in real time.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

// fakeObserverClock advances only when the loop asks to wait, or when a sample
// deliberately overruns. That makes cadence deterministic without real sleeps.
type fakeObserverClock struct {
	now time.Time
}

func (f *fakeObserverClock) Now() time.Time { return f.now }

func (f *fakeObserverClock) wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d > 0 {
		f.now = f.now.Add(d)
	}
	return nil
}

func baseObserverConfig() ObserverConfig {
	return ObserverConfig{
		Interval:      30 * time.Second,
		SampleTimeout: 10 * time.Second,
		Lifetime:      5 * time.Minute,
		StatusPath:    "status.json",
		LockPath:      "observer.lock",
		LockWait:      time.Second,
	}
}

// admittingReport is a REAL-SHAPED admitting decision: fresh readings with
// their own observation times, the derived numbers the decision rested on, and
// the limits it applied. Tests age or mutate this rather than hand-building a
// half-populated report, because a consumer check that passes on a skeleton
// proves nothing about a published one.
func admittingReport(at time.Time) AdmissionReport {
	normalized := 0.25
	load1 := 2.0
	cpus := 8
	freePct := 55
	swap := 0
	limits := DefaultLimits()
	return AdmissionReport{
		Decision:   string(DecisionAdmit),
		Admits:     true,
		Verdict:    "OK",
		DecidedAt:  stampUTC(at),
		RenderedAt: stampUTC(at),
		Limits:     limits,
		CPU: CPUReport{
			ReadingReport: ReadingReport{
				State:      string(freshness.StateFresh),
				Known:      true,
				ObservedAt: stampUTC(at),
				Source:     "test cpu",
			},
			Load1:      &load1,
			CPUs:       &cpus,
			Normalized: &normalized,
		},
		Memory: MemoryReport{
			ReadingReport: ReadingReport{
				State:      string(freshness.StateFresh),
				Known:      true,
				ObservedAt: stampUTC(at),
				Source:     "test memory",
			},
			Pressure:      "normal",
			PressureKnown: true,
			FreePct:       &freePct,
			FreePctGates:  false,
			SwapMB:        &swap,
		},
	}
}

// linuxAdmittingReport is the OTHER healthy platform shape: no kernel pressure
// signal at all, admitting on gating MemAvailable headroom. A consumer check
// that demanded a pressure level would refuse this host, which is why it has a
// positive control of its own.
func linuxAdmittingReport(at time.Time) AdmissionReport {
	r := admittingReport(at)
	r.Memory.Pressure = "unknown"
	r.Memory.PressureKnown = false
	gating := 40
	r.Memory.FreePct = &gating
	r.Memory.FreePctGates = true
	return r
}

// TestObserverDropsOverrunTicksWithoutCatchUp is the cadence contract.
//
// A sample that runs longer than the interval must cause the intervening ticks
// to be DROPPED and counted, never queued and replayed as a burst. A catch-up
// burst on a host already under pressure is the opposite of what this exists
// for.
func TestObserverDropsOverrunTicksWithoutCatchUp(t *testing.T) {
	clock := &fakeObserverClock{now: time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC)}
	cfg := baseObserverConfig()

	var starts []time.Time
	published := 0
	deps := observerDeps{
		now:  clock.Now,
		wait: clock.wait,
		sample: func(_ context.Context, at time.Time) (AdmissionReport, error) {
			starts = append(starts, at)
			// The second sample overruns by more than two intervals.
			if len(starts) == 2 {
				clock.now = clock.now.Add(75 * time.Second)
			}
			return admittingReport(at), nil
		},
		publish: func(ObserverStatus) error { published++; return nil },
	}

	status, err := runObserverLoop(context.Background(), cfg, ObserverIdentity{}, deps)
	if err != nil {
		t.Fatalf("loop returned an error on a clean run: %v", err)
	}
	if status.SkippedTicks == 0 {
		t.Fatalf("an overrunning sample produced zero skipped ticks; drops are being hidden")
	}
	// A dropped-tick count is a real count, not a wrapped subtraction. Every
	// published value must stay inside the schedule ceiling, and the per-sample
	// figures must add up to the total.
	var summed uint64
	for i, h := range status.History {
		if h.SkippedBefore > uint64(maxObserverTickIndex) {
			t.Fatalf("sample %d published %d dropped ticks, past the %d ceiling: the count wrapped",
				i, h.SkippedBefore, maxObserverTickIndex)
		}
		summed += h.SkippedBefore
	}
	if summed != status.SkippedTicks {
		t.Fatalf("per-sample dropped ticks sum to %d but the total says %d", summed, status.SkippedTicks)
	}
	if status.TotalTicks != uint64(len(starts)) {
		t.Fatalf("total ticks %d does not match samples actually taken %d", status.TotalTicks, len(starts))
	}
	// No catch-up: every sample must start strictly after the previous one
	// finished, and consecutive starts must be at least one interval apart.
	for i := 1; i < len(starts); i++ {
		gap := starts[i].Sub(starts[i-1])
		if gap < cfg.Interval {
			t.Fatalf("samples %d and %d started %s apart, inside the %s interval: that is a catch-up burst",
				i-1, i, gap, cfg.Interval)
		}
	}
	if published != len(starts)+1 {
		t.Fatalf("expected one publish per sample plus one terminal publish, got %d for %d samples",
			published, len(starts))
	}
}

// TestObserverRefusesPublishBeforeObservation is the direct regression test for
// the copied-stale-field defect.
//
// A publish stamp that predates the observation it carries is exactly what a
// copied-forward timestamp looks like, and it must be refused rather than
// written.
func TestObserverRefusesPublishBeforeObservation(t *testing.T) {
	deps := observerDeps{
		now:     func() time.Time { return time.Date(2026, 9, 12, 22, 20, 52, 0, time.UTC) },
		publish: func(ObserverStatus) error { return nil },
	}
	status := ObserverStatus{
		Latest: ObserverSample{
			CompletedAt: stampUTC(time.Date(2026, 9, 12, 22, 27, 57, 0, time.UTC)),
		},
	}
	err := publishAt(deps, &status, deps.now(), 30*time.Second)
	if err == nil {
		t.Fatalf("publishing at 22:20:52 a sample observed at 22:27:57 was accepted; the copied-stale-field defect is not guarded")
	}
	if !strings.Contains(err.Error(), "before the observation") {
		t.Fatalf("refusal did not name the cause: %v", err)
	}
	if status.PublishedAt != "" {
		t.Fatalf("a refused publish still stamped PublishedAt=%q", status.PublishedAt)
	}
}

// TestObserverStampsPublishTimeAtWriteTime proves the positive half: the stamp
// comes from the clock at write time, not from the sample and not from a
// previous status.
func TestObserverStampsPublishTimeAtWriteTime(t *testing.T) {
	observed := time.Date(2026, 9, 12, 22, 30, 50, 0, time.UTC)
	written := time.Date(2026, 9, 12, 22, 31, 5, 0, time.UTC)
	deps := observerDeps{
		now:     func() time.Time { return written },
		publish: func(ObserverStatus) error { return nil },
	}
	status := ObserverStatus{
		PublishedAt: stampUTC(time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC)), // a stale carry-over
		Latest: ObserverSample{
			CompletedAt: stampUTC(observed),
			Report:      admittingReport(observed),
		},
	}
	if err := publishAt(deps, &status, written, 30*time.Second); err != nil {
		t.Fatalf("writing after observing is correct chronology and must be accepted: %v", err)
	}
	if status.PublishedAt != stampUTC(written) {
		t.Fatalf("publish stamp %q was not taken at write time %q", status.PublishedAt, stampUTC(written))
	}
	// Expiry is the EARLIER of the cadence deadline and the metric window. The
	// metrics here were observed at 22:30:50 with a 30s staleness limit, so the
	// artifact must expire at 22:31:20 rather than at write+90s: a slow publish
	// must never renew an observation that has already aged out.
	wantExpiry := stampUTC(observed.Add(DefaultLimits().StaleAfter))
	if status.ExpiresAt != wantExpiry {
		t.Fatalf("expiry %q outlives the metric window %q", status.ExpiresAt, wantExpiry)
	}
	if status.ExpiresAt >= stampUTC(written.Add(ObserverExpiryTicks*30*time.Second)) {
		t.Fatalf("expiry %q reached the cadence deadline; the metric window did not bound it", status.ExpiresAt)
	}
}

// TestObserverHistoryStaysBounded proves the ring never grows.
func TestObserverHistoryStaysBounded(t *testing.T) {
	history := make([]ObserverSample, 0, ObserverHistoryMax)
	for i := 0; i < ObserverHistoryMax*3; i++ {
		history = appendBounded(history, ObserverSample{Sequence: uint64(i)})
		if len(history) > ObserverHistoryMax {
			t.Fatalf("history grew to %d entries, beyond the %d bound", len(history), ObserverHistoryMax)
		}
	}
	if len(history) != ObserverHistoryMax {
		t.Fatalf("history settled at %d entries, expected the full ring of %d", len(history), ObserverHistoryMax)
	}
	if history[len(history)-1].Sequence != uint64(ObserverHistoryMax*3-1) {
		t.Fatalf("newest sample is not last: got sequence %d", history[len(history)-1].Sequence)
	}
	if history[0].Sequence != uint64(ObserverHistoryMax*2) {
		t.Fatalf("oldest retained sample is %d, expected the window to have advanced to %d",
			history[0].Sequence, ObserverHistoryMax*2)
	}
}

// TestObserverHistoryNeverRestampsEarlierSamples protects the rule that a
// historical observation keeps its own time forever.
func TestObserverHistoryNeverRestampsEarlierSamples(t *testing.T) {
	first := ObserverSample{Sequence: 1, CompletedAt: "2026-09-12T22:00:00Z"}
	history := appendBounded(nil, first)
	for i := 2; i <= ObserverHistoryMax; i++ {
		history = appendBounded(history, ObserverSample{
			Sequence:    uint64(i),
			CompletedAt: "2026-09-12T23:00:00Z",
		})
	}
	if history[0].CompletedAt != first.CompletedAt {
		t.Fatalf("the first sample's observation time became %q; historical observations must never be restamped",
			history[0].CompletedAt)
	}
}

// TestObserverConfigRefusesBusyLoopBounds proves a flag cannot create a busy
// loop, and that an out-of-range value is REFUSED rather than silently
// repaired into something reasonable.
func TestObserverConfigRefusesBusyLoopBounds(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ObserverConfig)
	}{
		{"zero interval", func(c *ObserverConfig) { c.Interval = 0 }},
		{"negative interval", func(c *ObserverConfig) { c.Interval = -time.Second }},
		{"interval below floor", func(c *ObserverConfig) { c.Interval = MinObserverInterval - time.Millisecond }},
		{"interval above ceiling", func(c *ObserverConfig) { c.Interval = MaxObserverInterval + time.Second }},
		// Only the interval ceiling is violated here: the timeout and lifetime
		// are scaled to stay legal, so this case isolates that one bound
		// instead of being refused by a neighbouring rule.
		{"interval above ceiling only", func(c *ObserverConfig) {
			c.Interval = MaxObserverInterval + time.Second
			c.SampleTimeout = c.Interval / 2
			c.Lifetime = 4 * c.Interval
		}},
		{"zero lifetime", func(c *ObserverConfig) { c.Lifetime = 0 }},
		{"negative lifetime", func(c *ObserverConfig) { c.Lifetime = -time.Hour }},
		{"lifetime shorter than interval", func(c *ObserverConfig) { c.Lifetime = time.Minute; c.Interval = 5 * time.Minute }},
		{"lifetime leaves no room for a sample", func(c *ObserverConfig) { c.Interval = time.Minute; c.Lifetime = time.Minute }},
		{"sample timeout beyond half interval", func(c *ObserverConfig) { c.SampleTimeout = c.Interval }},
		{"explicit zero sample timeout", func(c *ObserverConfig) { c.SampleTimeout = 0 }},
		{"explicit negative sample timeout", func(c *ObserverConfig) { c.SampleTimeout = -time.Second }},
		{"empty status path", func(c *ObserverConfig) { c.StatusPath = "" }},
		{"empty lock path", func(c *ObserverConfig) { c.LockPath = "" }},
		{"status and lock collide", func(c *ObserverConfig) { c.LockPath = c.StatusPath }},
		{"zero lock wait", func(c *ObserverConfig) { c.LockWait = 0 }},
		{"unbounded lock wait", func(c *ObserverConfig) { c.LockWait = time.Hour }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseObserverConfig()
			tc.mutate(&cfg)
			if _, err := cfg.Validate(); err == nil {
				t.Fatalf("%s was accepted; an invalid observer bound must be refused, not repaired", tc.name)
			}
		})
	}
}

// TestObserverConfigAcceptsDefaults keeps the refusal test honest: the shipped
// configuration must actually pass.
func TestObserverConfigAcceptsDefaults(t *testing.T) {
	if _, err := baseObserverConfig().Validate(); err != nil {
		t.Fatalf("the baseline configuration was refused: %v", err)
	}
	cfg := DefaultObserverConfig()
	if _, err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultObserverConfig does not validate: %v", err)
	}
	if cfg.Interval != DefaultObserverInterval || cfg.Lifetime != DefaultObserverLifetime {
		t.Fatalf("defaults drifted: interval=%s lifetime=%s", cfg.Interval, cfg.Lifetime)
	}
}

// TestObserverUsableFailsClosed proves no absence reads as permission.
func TestObserverUsableFailsClosed(t *testing.T) {
	now := time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC)
	fresh := func() ObserverStatus { return usableStatus(now, admittingReport(now)) }
	if ok, why := ObserverUsable(fresh(), now); !ok {
		t.Fatalf("a fresh admitting status was rejected: %s", why)
	}

	cases := []struct {
		name   string
		mutate func(*ObserverStatus)
	}{
		{"expired", func(s *ObserverStatus) { s.ExpiresAt = stampUTC(now.Add(-time.Second)) }},
		{"no expiry", func(s *ObserverStatus) { s.ExpiresAt = "" }},
		{"unparseable expiry", func(s *ObserverStatus) { s.ExpiresAt = "whenever" }},
		{"terminated", func(s *ObserverStatus) { s.Terminated = "lifetime reached" }},
		{"sample error", func(s *ObserverStatus) { s.Latest.SampleError = "probe failed" }},
		{"decision refuses", func(s *ObserverStatus) {
			s.Latest.Report.Admits = false
			s.Latest.Report.Decision = string(DecisionRefuse)
		}},
		{"wrong schema", func(s *ObserverStatus) { s.SchemaVersion = ObserverSchemaVersion + 1 }},
		{"cpu observation unknown", func(s *ObserverStatus) { s.Latest.Report.CPU.Known = false }},
		{"memory observation unknown", func(s *ObserverStatus) { s.Latest.Report.Memory.Known = false }},
		{"cpu observation stale at consumer time", func(s *ObserverStatus) {
			s.Latest.Report.CPU.ObservedAt = stampUTC(now.Add(-time.Hour))
		}},
		{"memory observation in the future", func(s *ObserverStatus) {
			s.Latest.Report.Memory.ObservedAt = stampUTC(now.Add(time.Hour))
		}},
		{"cpu carries no normalized load", func(s *ObserverStatus) { s.Latest.Report.CPU.Normalized = nil }},
		{"memory pressure unknown", func(s *ObserverStatus) { s.Latest.Report.Memory.PressureKnown = false }},
		{"decision contradicts admits", func(s *ObserverStatus) { s.Latest.Report.Decision = string(DecisionRefuse) }},
		{"reading state not fresh", func(s *ObserverStatus) { s.Latest.Report.CPU.State = string(freshness.StateStale) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := fresh()
			tc.mutate(&status)
			ok, why := ObserverUsable(status, now)
			if ok {
				t.Fatalf("%s was reported usable; an absent or refusing observation must never authorize anything", tc.name)
			}
			if strings.TrimSpace(why) == "" {
				t.Fatalf("%s was refused without a stated reason", tc.name)
			}
		})
	}
}

// TestObserverUsableAcceptsBothHealthyPlatformShapes is the positive half of
// the consumer check. Darwin admits on a known kernel pressure level; Linux
// admits on gating headroom with no pressure signal. Both are healthy, and a
// revalidation that refused either would be wrong about a real host.
func TestObserverUsableAcceptsBothHealthyPlatformShapes(t *testing.T) {
	now := time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC)
	for name, report := range map[string]AdmissionReport{
		"darwin kernel pressure": admittingReport(now),
		"linux gating headroom":  linuxAdmittingReport(now),
	} {
		t.Run(name, func(t *testing.T) {
			status := usableStatus(now, report)
			if ok, why := ObserverUsable(status, now); !ok {
				t.Fatalf("%s was refused: %s", name, why)
			}
		})
	}
}

// usableStatus wraps a report in an otherwise-valid status, so a test that
// mutates one field is testing that field.
func usableStatus(now time.Time, report AdmissionReport) ObserverStatus {
	return ObserverStatus{
		SchemaVersion: ObserverSchemaVersion,
		PublishedAt:   stampUTC(now),
		ExpiresAt:     stampUTC(now.Add(time.Minute)),
		Latest: ObserverSample{
			StartedAt:   stampUTC(now.Add(-time.Second)),
			CompletedAt: stampUTC(now),
			Report:      report,
		},
	}
}

// TestObserverUsableRefusesContradictoryReports is the negative half: a report
// whose booleans say ADMIT while its numbers say otherwise must refuse, because
// the numbers are what the host actually did.
func TestObserverUsableRefusesContradictoryReports(t *testing.T) {
	now := time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC)
	cases := map[string]func(*AdmissionReport){
		"saturated cpu with admit booleans": func(r *AdmissionReport) {
			load := 16.0
			norm := 2.0
			r.CPU.Load1 = &load
			r.CPU.Normalized = &norm
		},
		"critical pressure with admit booleans": func(r *AdmissionReport) {
			r.Memory.Pressure = "critical"
		},
		"warning pressure with admit booleans": func(r *AdmissionReport) {
			r.Memory.Pressure = "warning"
		},
		"low gating headroom": func(r *AdmissionReport) {
			r.Memory.Pressure = "unknown"
			r.Memory.PressureKnown = false
			low := 3
			r.Memory.FreePct = &low
			r.Memory.FreePctGates = true
		},
		"raw and derived cpu disagree": func(r *AdmissionReport) {
			forged := 0.9
			r.CPU.Normalized = &forged
		},
		"cpu raw numbers missing": func(r *AdmissionReport) {
			r.CPU.Load1 = nil
		},
		"cpu count not positive": func(r *AdmissionReport) {
			zero := 0
			r.CPU.CPUs = &zero
		},
		// Both directions of the enum/boolean disagreement. This one is caught
		// twice over: with FreePctGates false, the later neither-signal guard
		// would refuse it anyway.
		"pressure enum contradicts pressure_known": func(r *AdmissionReport) {
			r.Memory.PressureKnown = false
		},
		// The direction only the enum/boolean check catches. The report claims
		// pressure IS known while naming an unknown level, and it gates on
		// headroom, so the neither-signal guard does not fire and the numbers
		// otherwise admit. Without that check the unknown level would be handed
		// to Decide as if it had been measured. Its healthy counterpart is the
		// linux shape in TestObserverUsableAcceptsBothHealthyPlatformShapes,
		// identical except that it honestly reports pressure as NOT known.
		"pressure_known contradicts an unknown level": func(r *AdmissionReport) {
			r.Memory.Pressure = "unknown"
			r.Memory.PressureKnown = true
			gating := 40
			r.Memory.FreePct = &gating
			r.Memory.FreePctGates = true
		},
		"unrecognised pressure level": func(r *AdmissionReport) {
			r.Memory.Pressure = "spicy"
		},
		"neither pressure nor gating headroom": func(r *AdmissionReport) {
			r.Memory.Pressure = "unknown"
			r.Memory.PressureKnown = false
			r.Memory.FreePctGates = false
		},
		"gating without a percentage": func(r *AdmissionReport) {
			r.Memory.Pressure = "unknown"
			r.Memory.PressureKnown = false
			r.Memory.FreePct = nil
			r.Memory.FreePctGates = true
		},
		"rendered before decided": func(r *AdmissionReport) {
			r.DecidedAt = stampUTC(now)
			r.RenderedAt = stampUTC(now.Add(-time.Minute))
		},
		"decided in the future": func(r *AdmissionReport) {
			r.DecidedAt = stampUTC(now.Add(time.Hour))
		},
		"decided_at missing": func(r *AdmissionReport) {
			r.DecidedAt = ""
		},
		"rendered_at missing": func(r *AdmissionReport) {
			r.RenderedAt = ""
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			report := admittingReport(now)
			mutate(&report)
			// The booleans still claim an admit; only the numbers changed.
			report.Admits = true
			report.Decision = string(DecisionAdmit)
			status := usableStatus(now, report)
			ok, why := ObserverUsable(status, now)
			if ok {
				t.Fatalf("%s was reported usable; published booleans must not outrank published numbers", name)
			}
			if strings.TrimSpace(why) == "" {
				t.Fatalf("%s refused without a stated reason", name)
			}
		})
	}
}

// TestObserverUsableRefusesBrokenChronology keeps a status that cannot say when
// it happened from establishing that anything is fresh.
func TestObserverUsableRefusesBrokenChronology(t *testing.T) {
	now := time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC)
	cases := map[string]func(*ObserverStatus){
		"missing started_at":          func(s *ObserverStatus) { s.Latest.StartedAt = "" },
		"missing completed_at":        func(s *ObserverStatus) { s.Latest.CompletedAt = "" },
		"missing published_at":        func(s *ObserverStatus) { s.PublishedAt = "" },
		"completed before started":    func(s *ObserverStatus) { s.Latest.CompletedAt = stampUTC(now.Add(-time.Hour)) },
		"published before completed":  func(s *ObserverStatus) { s.PublishedAt = stampUTC(now.Add(-time.Hour)) },
		"published in the future":     func(s *ObserverStatus) { s.PublishedAt = stampUTC(now.Add(time.Hour)) },
		"unparseable completed stamp": func(s *ObserverStatus) { s.Latest.CompletedAt = "recently" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			status := usableStatus(now, admittingReport(now))
			mutate(&status)
			if ok, _ := ObserverUsable(status, now); ok {
				t.Fatalf("%s was reported usable", name)
			}
		})
	}
}

// TestObserverStopsOnCancellationAndPublishesTermination proves shutdown is
// bounded and leaves an honest terminal artifact rather than a file that simply
// stops being updated.
func TestObserverStopsOnCancellationAndPublishesTermination(t *testing.T) {
	clock := &fakeObserverClock{now: time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC)}
	cfg := baseObserverConfig()
	ctx, cancel := context.WithCancel(context.Background())

	samples := 0
	var last ObserverStatus
	deps := observerDeps{
		now: clock.Now,
		wait: func(waitCtx context.Context, d time.Duration) error {
			if samples >= 2 {
				cancel()
			}
			return clock.wait(waitCtx, d)
		},
		sample: func(_ context.Context, at time.Time) (AdmissionReport, error) {
			samples++
			return admittingReport(at), nil
		},
		publish: func(s ObserverStatus) error { last = s; return nil },
	}

	status, err := runObserverLoop(ctx, cfg, ObserverIdentity{}, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation should surface as context.Canceled, got %v", err)
	}
	if status.Terminated == "" {
		t.Fatalf("a cancelled observer published no termination reason")
	}
	if last.Terminated == "" {
		t.Fatalf("the final published status did not record termination; readers cannot tell a stop from a stall")
	}
}

// TestObserverStopsAtLifetime proves the run is finite without a signal.
func TestObserverStopsAtLifetime(t *testing.T) {
	clock := &fakeObserverClock{now: time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC)}
	cfg := baseObserverConfig()
	cfg.Interval = 30 * time.Second
	cfg.SampleTimeout = 10 * time.Second
	cfg.Lifetime = 2 * time.Minute

	samples := 0
	deps := observerDeps{
		now:  clock.Now,
		wait: clock.wait,
		sample: func(_ context.Context, at time.Time) (AdmissionReport, error) {
			samples++
			if samples > 100 {
				t.Fatalf("observer did not stop at its lifetime; it took %d samples", samples)
			}
			return admittingReport(at), nil
		},
		publish: func(ObserverStatus) error { return nil },
	}

	status, err := runObserverLoop(context.Background(), cfg, ObserverIdentity{}, deps)
	if err != nil {
		t.Fatalf("lifetime expiry is a clean exit, got %v", err)
	}
	if status.Terminated != "lifetime reached" {
		t.Fatalf("termination reason was %q, expected the lifetime", status.Terminated)
	}
	if samples == 0 {
		t.Fatalf("observer exited without taking a single sample")
	}
}

// TestObserverRecordsSampleFailureWithoutAdmitting proves a failed sample is
// carried as a failure and, specifically, that the EMITTED report no longer
// admits.
//
// The published Admits flag is asserted directly. Asserting only that the final
// status is unusable would pass even with the forced refusal removed, because
// termination and the recorded SampleError reject it on their own.
func TestObserverRecordsSampleFailureWithoutAdmitting(t *testing.T) {
	clock := &fakeObserverClock{now: time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC)}
	cfg := baseObserverConfig()
	cfg.Lifetime = 90 * time.Second

	var published []ObserverStatus
	deps := observerDeps{
		now:  clock.Now,
		wait: clock.wait,
		sample: func(_ context.Context, at time.Time) (AdmissionReport, error) {
			// An ADMITTING report beside a failure: the loop must overrule it.
			// Returning an already-refusing report here would let the forced
			// refusal be removed without any test noticing.
			return admittingReport(at), errors.New("probe unavailable")
		},
		publish: func(st ObserverStatus) error { published = append(published, st); return nil },
	}

	status, err := runObserverLoop(context.Background(), cfg, ObserverIdentity{}, deps)
	if err != nil {
		t.Fatalf("a failing probe is a recorded refusal, not a loop error: %v", err)
	}
	if len(published) == 0 {
		t.Fatalf("the observer published nothing")
	}
	if status.Latest.SampleError == "" {
		t.Fatalf("a failed sample published no error")
	}
	// The emitted flag itself, on the retained sample and on what was written.
	if status.Latest.Report.Admits {
		t.Fatalf("a failed sample published report.Admits=true; a failure must not keep its admit")
	}
	if status.Latest.Report.Decision != string(DecisionRefuse) {
		t.Fatalf("a failed sample published decision %q, expected %q",
			status.Latest.Report.Decision, string(DecisionRefuse))
	}
	for i, st := range published {
		if st.Latest.SampleError != "" && st.Latest.Report.Admits {
			t.Fatalf("publication %d wrote report.Admits=true beside a sample error", i)
		}
	}
	// The broader consumer refusal is kept as a SEPARATE check, so neither
	// assertion can stand in for the other.
	if ok, _ := ObserverUsable(status, clock.Now()); ok {
		t.Fatalf("a status whose latest sample failed was reported usable")
	}
}

// TestDroppedTicksIsCheckedAtBothEnds pins the arithmetic behind the published
// dropped-tick count.
//
// The subtraction that produces it converts to an unsigned count, so a
// backwards or jumped index must be reported as a fault rather than converted:
// unchecked, either would wrap into an enormous plausible-looking number of
// dropped ticks.
func TestDroppedTicksIsCheckedAtBothEnds(t *testing.T) {
	cases := []struct {
		name      string
		tickIndex int64
		lastIndex int64
		want      uint64
		wantOK    bool
	}{
		{"no previous sample", 1, 0, 0, true},
		{"consecutive ticks drop nothing", 2, 1, 0, true},
		{"one dropped tick", 3, 1, 1, true},
		{"several dropped ticks", 9, 4, 4, true},
		{"at the ceiling", maxObserverTickIndex + 1, 1, uint64(maxObserverTickIndex - 1), true},
		{"index moved backwards", 1, 5, 0, false},
		{"index equal to the previous", 4, 4, 0, false},
		{"gap past the ceiling", maxObserverTickIndex + 3, 1, 0, false},
		{"negative index", -1, 1, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := droppedTicks(tc.tickIndex, tc.lastIndex)
			if ok != tc.wantOK {
				t.Fatalf("%s: ok=%v, want %v", tc.name, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("%s: count=%d, want %d", tc.name, got, tc.want)
			}
			if !ok && got != 0 {
				t.Fatalf("%s: a faulted gap still produced the count %d", tc.name, got)
			}
		})
	}
}

// TestMaxObserverTickIndexBoundsALegalRun keeps the ceiling meaningful: it must
// be reachable by the longest legal run and no smaller, or it would either
// reject a valid schedule or fail to bound anything.
func TestMaxObserverTickIndexBoundsALegalRun(t *testing.T) {
	if maxObserverTickIndex <= 0 {
		t.Fatalf("the tick ceiling is %d; it bounds nothing", maxObserverTickIndex)
	}
	longest := int64(MaxObserverLifetime / MinObserverInterval)
	if maxObserverTickIndex != longest {
		t.Fatalf("the tick ceiling is %d but the longest legal run needs %d slots",
			maxObserverTickIndex, longest)
	}
	// The bounded index can never overflow the Duration it is multiplied into.
	if maxObserverTickIndex > int64(MaxObserverLifetime)/int64(MinObserverInterval) {
		t.Fatalf("the ceiling exceeds what the schedule multiplication can represent")
	}
}
