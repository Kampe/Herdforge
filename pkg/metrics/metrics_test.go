package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestDiskMetricsBoundedCardinality(t *testing.T) {
	m := NewMetricsExporter()

	// Never probed: no disk series at all (absence, not a fake ok).
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(rec.Body.String(), "herd_disk_") {
		t.Fatalf("disk series emitted before any probe:\n%s", rec.Body.String())
	}

	m.SetDiskState("blocked")
	m.SetDiskVolume("repo", 13<<30, 1.4)
	m.SetDiskVolume("temp", 500<<30, 50)
	// Unbounded inputs are coerced, never new label values.
	m.SetDiskVolume("../../attacker-path", 1, 1)
	m.SetDiskState("BLOCKED(disk_pressure)") // raw label is not a state

	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`herd_disk_pressure_state{state="unknown"} 1`,
		`herd_disk_pressure_state{state="blocked"} 0`,
		`herd_disk_free_bytes{volume="repo"} 13958643712`,
		`herd_disk_free_pct{volume="temp"} 50.00`,
		`herd_disk_free_bytes{volume="other"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "attacker-path") {
		t.Fatalf("unbounded label leaked:\n%s", body)
	}

	// Legal state transition renders one-hot.
	m.SetDiskState("ok")
	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `herd_disk_pressure_state{state="ok"} 1`) {
		t.Fatalf("state gauge did not follow transition:\n%s", rec.Body.String())
	}
}
