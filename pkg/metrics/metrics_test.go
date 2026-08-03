package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memoryStateStore struct{ payload []byte }

func (s *memoryStateStore) Load(context.Context) ([]byte, error) {
	return append([]byte(nil), s.payload...), nil
}
func (s *memoryStateStore) Save(_ context.Context, payload []byte) error {
	s.payload = append([]byte(nil), payload...)
	return nil
}

func completeDependencies() []DependencyHealth {
	return []DependencyHealth{
		{Name: "event", Critical: true, State: DependencyHealthy},
		{Name: "git", Critical: true, State: DependencyHealthy},
		{Name: "herdr", Critical: true, State: DependencyHealthy},
		{Name: "provider", Critical: true, State: DependencyHealthy},
	}
}

func TestUnknownCriticalDependenciesAreNotReady(t *testing.T) {
	if health, err := BuildHealthSnapshot(nil); err == nil || health.Readiness {
		t.Fatalf("an unobserved dependency set must fail closed: health=%+v err=%v", health, err)
	}
	health := UnknownHealthSnapshot()
	if !health.Liveness {
		t.Fatal("unknown dependencies must not make the process look dead")
	}
	if health.Readiness {
		t.Fatal("unknown critical dependencies must keep readiness false")
	}
	if _, queue, _ := NewMetricsExporter().Snapshot(); queue.Known {
		t.Fatal("a fresh exporter must not claim known queue pressure")
	}
	if got := fmt.Sprint(health.Dependencies); strings.Contains(got, "task") || strings.Contains(got, "model") {
		t.Fatalf("health dependency dimensions must remain bounded: %s", got)
	}
}

func TestHealthObservationRequiresCompleteFiniteCriticalSet(t *testing.T) {
	valid := []DependencyHealth{
		{Name: "event", Critical: true, State: DependencyHealthy},
		{Name: "git", Critical: true, State: DependencyHealthy},
		{Name: "herdr", Critical: true, State: DependencyHealthy},
		{Name: "provider", Critical: true, State: DependencyHealthy},
	}
	if health, err := BuildHealthSnapshot(valid); err != nil || !health.Readiness {
		t.Fatalf("valid complete observation must be ready: health=%+v err=%v", health, err)
	}
	cases := []struct {
		name   string
		mutate func([]DependencyHealth) []DependencyHealth
	}{
		{"missing", func(deps []DependencyHealth) []DependencyHealth { return deps[:3] }},
		{"duplicate", func(deps []DependencyHealth) []DependencyHealth { deps[3].Name = "event"; return deps }},
		{"empty name", func(deps []DependencyHealth) []DependencyHealth { deps[0].Name = ""; return deps }},
		{"invalid state", func(deps []DependencyHealth) []DependencyHealth { deps[0].State = "maybe"; return deps }},
		{"healthy with error", func(deps []DependencyHealth) []DependencyHealth { deps[0].Error = "probe failed"; return deps }},
		{"non-critical", func(deps []DependencyHealth) []DependencyHealth { deps[0].Critical = false; return deps }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := make([]DependencyHealth, len(valid))
			copy(deps, valid)
			if _, err := BuildHealthSnapshot(tc.mutate(deps)); err == nil {
				t.Fatal("invalid dependency observation was accepted")
			}
		})
	}
}

func TestQueuePressureErrorCannotBecomeFreeCapacity(t *testing.T) {
	queue := QueuePressure{Depth: 0, Capacity: 100, Known: true, Error: "provider unavailable"}
	if _, ok := queue.Available(); ok {
		t.Fatal("a failed queue probe must not report free capacity")
	}
	if err := queue.Validate(); err == nil {
		t.Fatal("known queue pressure with an error must be rejected")
	}

	exp := NewMetricsExporter()
	if err := exp.SetQueuePressure(queue); err == nil {
		t.Fatal("exporter must reject a failed queue observation")
	}
	_, stored, _ := exp.Snapshot()
	if stored.Known || stored.Error == "" {
		t.Fatalf("failed update must not replace unknown safe state: %+v", stored)
	}
	for _, invalid := range []QueuePressure{{Depth: -1, Capacity: 1, Known: true}, {Depth: 1, Capacity: -1, Known: true}, {Depth: 2, Capacity: 1, Known: true}, {Depth: 0, Capacity: 1, Known: false}, {Depth: 1, Capacity: 1, Known: false, Error: "probe failed"}} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid queue observation accepted: %+v", invalid)
		}
	}
}

func TestDeadProviderCannotContradictHealthyProviderObservation(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	exp := NewMetricsExporterWithPersistence(nil, func() time.Time { return now })
	if err := exp.SetHealthAt(completeDependencies(), now); err != nil {
		t.Fatal(err)
	}
	if err := exp.SetSignals(FleetSignals{DeadProvider: true, LastReconciliation: now, ObservedAt: now}); err == nil {
		t.Fatal("dead provider must not coexist with a healthy provider observation")
	}
	if got := exp.Signals(); got != (FleetSignals{}) {
		t.Fatalf("contradictory signal was not rejected fail-closed: %+v", got)
	}
}

func TestObservationSequenceFencesOlderHealthQueueSignalsAndSLO(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	exp := NewMetricsExporterWithPersistence(nil, func() time.Time { return now })
	if err := exp.SetHealthObservation(completeDependencies(), now, 2); err != nil {
		t.Fatal(err)
	}
	if err := exp.SetHealthObservation(completeDependencies(), now.Add(time.Minute), 1); err == nil {
		t.Fatal("older health sequence was accepted")
	}
	if err := exp.SetQueuePressureObservation(QueuePressure{Depth: 2, Capacity: 3, Known: true}, now, 2); err != nil {
		t.Fatal(err)
	}
	if err := exp.SetQueuePressureObservation(QueuePressure{Depth: 1, Capacity: 3, Known: true}, now.Add(time.Minute), 1); err == nil {
		t.Fatal("older queue sequence was accepted")
	}
	signals := FleetSignals{StalledWork: 2, LastReconciliation: now, ObservedAt: now}
	if err := exp.SetSignalsObservation(signals, 2); err != nil {
		t.Fatal(err)
	}
	signals.StalledWork = 1
	signals.ObservedAt = now.Add(-10 * time.Minute)
	if err := exp.SetSignalsObservation(signals, 1); err == nil {
		t.Fatal("older signal sequence was accepted")
	}
	if err := exp.RecordTransitionObservation(now, now.Add(time.Second), nil, now, 2); err != nil {
		t.Fatal(err)
	}
	if err := exp.RecordTransitionObservation(now, now.Add(2*time.Second), nil, now.Add(time.Minute), 1); err == nil {
		t.Fatal("older SLO sequence was accepted")
	}
	if got := exp.Signals().StalledWork; got != 2 {
		t.Fatalf("older signal overwrote authority: %d", got)
	}
}

func TestReadAtReevaluatesMinuteScaleFreshness(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	config := FreshnessConfig{HealthMaxAge: time.Minute, QueueMaxAge: time.Minute, SignalsMaxAge: time.Minute, SLOMaxAge: time.Minute}
	exp := NewMetricsExporterWithConfig(nil, func() time.Time { return base }, config)
	if err := exp.SetHealthAt(completeDependencies(), base); err != nil {
		t.Fatal(err)
	}
	if err := exp.SetQueuePressureAt(QueuePressure{Depth: 1, Capacity: 2, Known: true}, base); err != nil {
		t.Fatal(err)
	}
	if err := exp.SetSignals(FleetSignals{LastReconciliation: base, ObservedAt: base}); err != nil {
		t.Fatal(err)
	}
	view := exp.ReadAt(base.Add(2 * time.Minute))
	if view.Freshness.Ready || view.Freshness.HealthFresh || view.Freshness.QueueFresh || view.Freshness.SignalsFresh {
		t.Fatalf("stale observations became healthy: %+v", view.Freshness)
	}
}

func TestConditionCodesRemainDistinctAndEligibleIdleIsDerived(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	eligibleSince := now.Add(-10 * time.Second)
	idle := FleetSignals{StalledWork: 1, DroppedCallbacks: 1, ReviewSaturation: 80, DeadProvider: true, IntegrationBacklog: 1, DeadLetters: 1, EligibleWaiting: 1, EligibleSince: eligibleSince, Backpressured: true, LastReconciliation: now, ObservedAt: now}
	if err := idle.Validate(now, time.Minute); err != nil {
		t.Fatal(err)
	}
	conditions := idle.ConditionCodes()
	if len(conditions) != 6 || conditions[0] != ConditionStalledWork || conditions[1] != ConditionDroppedCallback || conditions[2] != ConditionReviewSaturation || conditions[3] != ConditionDeadProvider || conditions[4] != ConditionIntegrationBacklog || conditions[5] != ConditionDeadLetters {
		t.Fatalf("condition codes collapsed or reordered: %v", conditions)
	}
	eligible := FleetSignals{EligibleWaiting: 1, EligibleSince: eligibleSince, EligibleIdle: 10 * time.Second, LastReconciliation: now, ObservedAt: now}
	if got := eligible.ConditionCodes(); len(got) != 1 || got[0] != ConditionEligibleIdle {
		t.Fatalf("eligible idle condition was not distinct: %v", got)
	}
	contradictory := eligible
	contradictory.ReviewSaturation = 100
	if err := contradictory.Validate(now, time.Minute); err == nil {
		t.Fatal("eligible idle was accepted while saturated without backpressure")
	}
}

func TestTransitionSLOUsesDeterministicClockAndSeparatesFailures(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	end := start.Add(250 * time.Millisecond)
	observed := end.Add(time.Second)
	exp := NewMetricsExporter()
	exp.RecordTransition(start, end, nil, observed)
	exp.RecordTransition(start, end, errors.New("event append failed"), observed)
	_, _, slo := exp.Snapshot()
	if slo.Attempts != 2 || slo.Completed != 1 || slo.Failed != 1 {
		t.Fatalf("unexpected transition SLO: %+v", slo)
	}
	if slo.TotalLatency != 250*time.Millisecond || !slo.ObservedAt.Equal(observed) {
		t.Fatalf("unexpected deterministic latency/timestamp: %+v", slo)
	}
}

func TestFleetSignalsMatrixRejectsContradictoryAndStaleObservations(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	base := FleetSignals{ReviewSaturation: 100, LastReconciliation: now, ObservedAt: now}
	valid := []FleetSignals{base, {DeadProvider: true, LastReconciliation: now, ObservedAt: now}}
	for i, signals := range valid {
		signals.StalledWork = uint64(i)
		exp := NewMetricsExporterWithPersistence(nil, func() time.Time { return now })
		if err := exp.SetSignals(signals); err != nil {
			t.Fatalf("valid signal matrix row %d rejected: %v", i, err)
		}
	}
	invalid := []FleetSignals{
		{LastReconciliation: now, ObservedAt: now.Add(25 * time.Hour)},
		{LastReconciliation: now.Add(time.Hour), ObservedAt: now},
		{ReviewSaturation: 101, LastReconciliation: now, ObservedAt: now},
		{MaxLeaseAge: -time.Second, LastReconciliation: now, ObservedAt: now},
		{StalledWork: maxSignalCount + 1, LastReconciliation: now, ObservedAt: now},
	}
	for _, signals := range invalid {
		exp := NewMetricsExporterWithPersistence(nil, func() time.Time { return now })
		if err := exp.SetSignals(signals); err == nil {
			t.Fatalf("contradictory/stale signals accepted: %+v", signals)
		}
		if got := exp.Signals(); got != (FleetSignals{}) {
			t.Fatalf("invalid signal did not fail closed: %+v", got)
		}
	}
}

func TestMetricsStateRestartRoundTripAndStaleRestore(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	store := &memoryStateStore{}
	exp := NewMetricsExporterWithPersistence(store, func() time.Time { return now })
	if err := exp.SetHealthAt(completeDependencies(), now); err != nil {
		t.Fatal(err)
	}
	if err := exp.SetQueuePressureAt(QueuePressure{Depth: 2, Capacity: 10, Known: true}, now); err != nil {
		t.Fatal(err)
	}
	signals := FleetSignals{StalledWork: 3, DroppedCallbacks: 2, ReviewSaturation: 75, IntegrationBacklog: 4, Retries: 5, DeadLetters: 1, MaxLeaseAge: time.Minute, MaxCallbackAge: 2 * time.Minute, EligibleWaiting: 1, EligibleSince: now, Backpressured: true, LastReconciliation: now, ObservedAt: now}
	if err := exp.SetSignals(signals); err != nil {
		t.Fatal(err)
	}
	exp.RecordTaskProcessed()
	exp.RecordTransition(now.Add(-time.Second), now, nil, now)
	if err := exp.Persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	var persisted persistedState
	if err := json.Unmarshal(store.payload, &persisted); err != nil || persisted.Version != 1 {
		t.Fatalf("invalid persisted schema: err=%v state=%+v", err, persisted)
	}
	for name, mutate := range map[string]func(*persistedState){
		"negative latency":    func(state *persistedState) { state.SLO.TotalLatency = -time.Second },
		"oversized latency":   func(state *persistedState) { state.SLO.TotalLatency = maxSignalAge + time.Nanosecond },
		"inconsistent totals": func(state *persistedState) { state.SLO.Failed++ },
		"torn readiness":      func(state *persistedState) { state.Health.Readiness = !state.Health.Readiness },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := persisted
			mutate(&candidate)
			payload, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			store.payload = payload
			invalid := NewMetricsExporterWithPersistence(store, func() time.Time { return now })
			if err := invalid.Restore(context.Background()); err == nil {
				t.Fatal("invalid persisted state was accepted")
			}
		})
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	store.payload = payload
	restored := NewMetricsExporterWithPersistence(store, func() time.Time { return now })
	if err := restored.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	health, queue, slo := restored.Snapshot()
	if !health.Readiness || queue.Depth != 2 || restored.Signals() != signals || slo.Completed != 1 || restored.TotalTasksProcessed != 1 {
		t.Fatalf("restart round-trip lost state: health=%+v queue=%+v signals=%+v slo=%+v tasks=%d", health, queue, restored.Signals(), slo, restored.TotalTasksProcessed)
	}
	current := now
	stale := NewMetricsExporterWithPersistence(store, func() time.Time { return current })
	if err := stale.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	current = now.Add(25 * time.Hour)
	if err := stale.Restore(context.Background()); err == nil {
		t.Fatal("stale persisted observations must be rejected")
	}
	if health, queue, signals := stale.Snapshot(); health.Readiness || queue.Known || stale.Signals() != (FleetSignals{}) || signals.Attempts != 0 {
		t.Fatalf("stale restore did not remain fail-closed: health=%+v queue=%+v signals=%+v slo=%+v", health, queue, stale.Signals(), signals)
	}
}

func TestMetricsExporter_Handler(t *testing.T) {
	exp := NewMetricsExporter()
	exp.RecordTaskProcessed()
	exp.RecordTokens(1500)
	exp.RecordReview(true)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()

	exp.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "herd_tasks_processed_total 1") || !strings.Contains(body, "herd_tokens_burned_total 1500") {
		t.Errorf("unexpected metrics output:\n%s", body)
	}
}

func TestRecordReview_Failure(t *testing.T) {
	exp := NewMetricsExporter()
	exp.RecordReview(false)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	exp.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `herd_review_verdicts_total{verdict="fail"} 1`) {
		t.Errorf("expected review fail counter, got:\n%s", body)
	}
}

func TestMetricsHandlerUsesBoundedLabelsAndSafeGauges(t *testing.T) {
	exp := NewMetricsExporter()
	now := time.Unix(12, 0)
	exp = NewMetricsExporterWithPersistence(nil, func() time.Time { return now })
	exp.RecordTransition(time.Unix(10, 0), time.Unix(11, 0), errors.New("git unavailable"), now)
	if err := exp.SetQueuePressureAt(QueuePressure{Depth: 2, Capacity: 3, Known: true}, now); err != nil {
		t.Fatal(err)
	}
	if err := exp.SetSignals(FleetSignals{StalledWork: 2, DroppedCallbacks: 1, ReviewSaturation: 50, DeadProvider: true, IntegrationBacklog: 3, Retries: 4, DeadLetters: 1, MaxLeaseAge: time.Second, MaxCallbackAge: 2 * time.Second, EligibleWaiting: 1, EligibleSince: now, Backpressured: true, LastReconciliation: now, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	exp.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, forbidden := range []string{"task=", "ref=", "model=", "git unavailable"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics contains unbounded or raw-error label %q:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, "herd_readiness_ready 0") || !strings.Contains(body, "herd_queue_pressure_known 1") || !strings.Contains(body, `herd_transitions_total{outcome="failed"} 1`) {
		t.Fatalf("metrics lost safe health/SLO signals:\n%s", body)
	}
	for _, expected := range []string{"# TYPE herd_stalled_work gauge", "herd_stalled_work 2", "# TYPE herd_dropped_callbacks gauge", "herd_dropped_callbacks 1", "herd_review_saturation_ratio 0.50", "herd_dead_provider 1", "herd_integration_backlog 3", "herd_retries 4", "herd_dead_letters 1", "herd_eligible_idle_seconds 0.000000", "herd_queue_depth 2", "herd_queue_capacity 3"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing bounded signal %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "herd_transition_latency_seconds") {
		t.Fatalf("failed transitions must not export completed latency: %s", body)
	}
}
