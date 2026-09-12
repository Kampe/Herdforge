package resources

// FAC-829 behaviour tests. Every one runs on a fake clock and injected probes:
// nothing here starts a process, touches the host, or waits in real time.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

// admittingReport is a decision shaped like a real one, without any probe.
func admittingReport(at time.Time) AdmissionReport {
	return AdmissionReport{
		Decision:   string(DecisionAdmit),
		Admits:     true,
		RenderedAt: stampUTC(at),
	}
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
		Latest:      ObserverSample{CompletedAt: stampUTC(observed)},
	}
	if err := publishAt(deps, &status, written, 30*time.Second); err != nil {
		t.Fatalf("writing after observing is correct chronology and must be accepted: %v", err)
	}
	if status.PublishedAt != stampUTC(written) {
		t.Fatalf("publish stamp %q was not taken at write time %q", status.PublishedAt, stampUTC(written))
	}
	if status.ExpiresAt != stampUTC(written.Add(ObserverExpiryTicks*30*time.Second)) {
		t.Fatalf("expiry %q was not derived from the write time", status.ExpiresAt)
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
		{"zero lifetime", func(c *ObserverConfig) { c.Lifetime = 0 }},
		{"negative lifetime", func(c *ObserverConfig) { c.Lifetime = -time.Hour }},
		{"lifetime shorter than interval", func(c *ObserverConfig) { c.Lifetime = time.Minute; c.Interval = 5 * time.Minute }},
		{"sample timeout beyond half interval", func(c *ObserverConfig) { c.SampleTimeout = c.Interval }},
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
	fresh := func() ObserverStatus {
		return ObserverStatus{
			SchemaVersion: ObserverSchemaVersion,
			ExpiresAt:     stampUTC(now.Add(time.Minute)),
			Latest:        ObserverSample{Report: admittingReport(now)},
		}
	}
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
// carried as a failure, never smoothed into a healthy tick.
func TestObserverRecordsSampleFailureWithoutAdmitting(t *testing.T) {
	clock := &fakeObserverClock{now: time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC)}
	cfg := baseObserverConfig()
	cfg.Lifetime = 90 * time.Second

	deps := observerDeps{
		now:  clock.Now,
		wait: clock.wait,
		sample: func(_ context.Context, _ time.Time) (AdmissionReport, error) {
			return AdmissionReport{Decision: string(DecisionRefuse)}, errors.New("probe unavailable")
		},
		publish: func(ObserverStatus) error { return nil },
	}

	status, err := runObserverLoop(context.Background(), cfg, ObserverIdentity{}, deps)
	if err != nil {
		t.Fatalf("a failing probe is a recorded refusal, not a loop error: %v", err)
	}
	if status.Latest.SampleError == "" {
		t.Fatalf("a failed sample published no error")
	}
	if ok, _ := ObserverUsable(status, clock.Now()); ok {
		t.Fatalf("a status whose latest sample failed was reported usable")
	}
}
