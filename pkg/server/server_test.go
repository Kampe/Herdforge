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
