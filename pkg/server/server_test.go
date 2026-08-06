package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/metrics"
)

func TestStatusSchemaPreservesLivenessReadinessAndUnknownPressure(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	now := base.Add(3 * time.Second)
	exp := metrics.NewMetricsExporter()
	clockCalls := 0
	srv := NewControlServerWithMetrics("127.0.0.1:0", exp, func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return base
		}
		return now
	})
	w := httptest.NewRecorder()
	srv.handleStatus(w, httptest.NewRequest("GET", "/v1/status", nil))

	var got ServerStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status schema is not valid JSON: %v", err)
	}
	if got.Status != "degraded" || !got.Health.Liveness || got.Health.Readiness {
		t.Fatalf("liveness/readiness collapsed or changed: %+v", got)
	}
	if got.Queue.Known || got.Queue.Error == "" {
		t.Fatalf("unknown queue state collapsed into capacity: %+v", got.Queue)
	}
	if got.UptimeSec != 3 || !got.Timestamp.Equal(now) {
		t.Fatalf("fake clock not reflected in schema: %+v", got)
	}
	if clockCalls != 2 || got.Freshness.AsOf != now {
		t.Fatalf("status did not use one captured clock sample: calls=%d freshness=%+v", clockCalls, got.Freshness)
	}
}

func TestStatusSchemaReportsHealthyOnlyWithAuthoritativeSignals(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	exp := metrics.NewMetricsExporterWithPersistence(nil, func() time.Time { return now })
	deps := []metrics.DependencyHealth{
		{Name: "event", Critical: true, State: metrics.DependencyHealthy},
		{Name: "git", Critical: true, State: metrics.DependencyHealthy},
		{Name: "herdr", Critical: true, State: metrics.DependencyHealthy},
		{Name: "provider", Critical: true, State: metrics.DependencyHealthy},
	}
	if err := exp.SetHealthAt(deps, now); err != nil {
		t.Fatal(err)
	}
	if err := exp.SetQueuePressureAt(metrics.QueuePressure{Depth: 1, Capacity: 2, Known: true}, now); err != nil {
		t.Fatal(err)
	}
	srv := NewControlServerWithMetrics("127.0.0.1:0", exp, func() time.Time { return now })
	w := httptest.NewRecorder()
	srv.handleStatus(w, httptest.NewRequest("GET", "/v1/status", nil))
	var beforeSignals ServerStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &beforeSignals); err != nil {
		t.Fatal(err)
	}
	if beforeSignals.Status != "degraded" {
		t.Fatalf("missing signal authority must remain degraded: %+v", beforeSignals)
	}
	if err := exp.SetSignals(metrics.FleetSignals{LastReconciliation: now, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	srv.handleStatus(w, httptest.NewRequest("GET", "/v1/status", nil))
	var got ServerStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "healthy" || !got.Health.Readiness || got.Signals.ObservedAt.IsZero() {
		t.Fatalf("authoritative state was not reported healthy: %+v", got)
	}
}

func TestStatusKeepsFreshUnhealthyHealthDistinctFromStale(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	exp := metrics.NewMetricsExporterWithPersistence(nil, func() time.Time { return now })
	deps := []metrics.DependencyHealth{
		{Name: "event", Critical: true, State: metrics.DependencyHealthy},
		{Name: "git", Critical: true, State: metrics.DependencyHealthy},
		{Name: "herdr", Critical: true, State: metrics.DependencyHealthy},
		{Name: "provider", Critical: true, State: metrics.DependencyUnhealthy, Error: "probe failed"},
	}
	if err := exp.SetHealthAt(deps, now); err != nil {
		t.Fatal(err)
	}
	srv := NewControlServerWithMetrics("127.0.0.1:0", exp, func() time.Time { return now })
	w := httptest.NewRecorder()
	srv.handleStatus(w, httptest.NewRequest("GET", "/v1/status", nil))
	var got ServerStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status == "healthy" || !got.Freshness.HealthFresh || got.Freshness.HealthReady || got.Health.Readiness {
		t.Fatalf("fresh unhealthy dependency was mislabeled stale or healthy: %+v", got)
	}
	if !containsCondition(got.Freshness.Reasons, metrics.ReasonHealthUnready) {
		t.Fatalf("bounded unhealthy reason missing: %+v", got.Freshness.Reasons)
	}
}

func containsCondition(conditions []metrics.ConditionCode, wanted metrics.ConditionCode) bool {
	for _, condition := range conditions {
		if condition == wanted {
			return true
		}
	}
	return false
}

func TestStatusSchemaPreservesEveryFleetConditionCode(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		code metrics.ConditionCode
		set  func(*metrics.FleetSignals)
	}{
		{"stalled", metrics.ConditionStalledWork, func(s *metrics.FleetSignals) { s.StalledWork = 1 }},
		{"dropped", metrics.ConditionDroppedCallback, func(s *metrics.FleetSignals) { s.DroppedCallbacks = 1 }},
		{"review", metrics.ConditionReviewSaturation, func(s *metrics.FleetSignals) { s.ReviewSaturation = 80 }},
		{"dead provider", metrics.ConditionDeadProvider, func(s *metrics.FleetSignals) { s.DeadProvider = true }},
		{"integration", metrics.ConditionIntegrationBacklog, func(s *metrics.FleetSignals) { s.IntegrationBacklog = 1 }},
		{"dead letters", metrics.ConditionDeadLetters, func(s *metrics.FleetSignals) { s.DeadLetters = 1 }},
		{"eligible idle", metrics.ConditionEligibleIdle, func(s *metrics.FleetSignals) { s.EligibleWaiting = 1; s.EligibleSince = now.Add(-time.Second) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exp := metrics.NewMetricsExporterWithPersistence(nil, func() time.Time { return now })
			deps := []metrics.DependencyHealth{
				{Name: "event", Critical: true, State: metrics.DependencyHealthy},
				{Name: "git", Critical: true, State: metrics.DependencyHealthy},
				{Name: "herdr", Critical: true, State: metrics.DependencyHealthy},
				{Name: "provider", Critical: true, State: metrics.DependencyHealthy},
			}
			if tc.code == metrics.ConditionDeadProvider {
				deps[3] = metrics.DependencyHealth{Name: "provider", Critical: true, State: metrics.DependencyUnhealthy, Error: "probe failed"}
			}
			if err := exp.SetHealthAt(deps, now); err != nil {
				t.Fatal(err)
			}
			if err := exp.SetQueuePressureAt(metrics.QueuePressure{Depth: 1, Capacity: 2, Known: true}, now); err != nil {
				t.Fatal(err)
			}
			signals := metrics.FleetSignals{LastReconciliation: now, ObservedAt: now}
			tc.set(&signals)
			if err := exp.SetSignals(signals); err != nil {
				t.Fatal(err)
			}
			srv := NewControlServerWithMetrics("127.0.0.1:0", exp, func() time.Time { return now })
			w := httptest.NewRecorder()
			srv.handleStatus(w, httptest.NewRequest("GET", "/v1/status", nil))
			var got ServerStatusResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status == "healthy" || len(got.Conditions) != 1 || got.Conditions[0] != tc.code {
				t.Fatalf("condition was collapsed or status was healthy: status=%s conditions=%v", got.Status, got.Conditions)
			}
		})
	}
}

func TestControlServer_StartAndEndpoints(t *testing.T) {
	srv := NewControlServer("127.0.0.1:18899")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("expected server start success, got err: %v", err)
	}
	defer srv.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	// Query /v1/status
	resp, err := http.Get("http://127.0.0.1:18899/v1/status")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /v1/status, got code %d (err: %v)", resp.StatusCode, err)
	}

	var statusResp ServerStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		t.Fatalf("failed to decode status response: %v", err)
	}
	if statusResp.Status != "degraded" {
		t.Errorf("expected unknown dependencies to be degraded, got %s", statusResp.Status)
	}

	// Query /openapi.json
	openAPIResp, err := http.Get("http://127.0.0.1:18899/openapi.json")
	if err != nil || openAPIResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /openapi.json, got code %d", openAPIResp.StatusCode)
	}
	metricsResp, err := http.Get("http://127.0.0.1:18899/metrics")
	if err != nil || metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /metrics, got code %d (err: %v)", metricsResp.StatusCode, err)
	}
}
