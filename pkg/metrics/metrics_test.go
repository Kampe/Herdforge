package metrics

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
	for _, invalid := range []QueuePressure{{Depth: -1, Capacity: 1, Known: true}, {Depth: 1, Capacity: -1, Known: true}, {Depth: 0, Capacity: 1, Known: false}, {Depth: 1, Capacity: 1, Known: false, Error: "probe failed"}} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid queue observation accepted: %+v", invalid)
		}
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
	exp.RecordTransition(time.Unix(10, 0), time.Unix(11, 0), errors.New("git unavailable"), time.Unix(12, 0))
	w := httptest.NewRecorder()
	exp.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, forbidden := range []string{"task=", "ref=", "model=", "git unavailable"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics contains unbounded or raw-error label %q:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, "herd_readiness_ready 0") || !strings.Contains(body, "herd_queue_pressure_known 0") || !strings.Contains(body, `herd_transitions_total{outcome="failed"} 1`) {
		t.Fatalf("metrics lost safe health/SLO signals:\n%s", body)
	}
}
