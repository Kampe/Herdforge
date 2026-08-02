package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type MetricsExporter struct {
	TotalTasksProcessed uint64
	TotalTokensBurned   uint64
	TotalReviewPasses   uint64
	TotalReviewFails    uint64
}

func NewMetricsExporter() *MetricsExporter {
	return &MetricsExporter{}
}

func (m *MetricsExporter) RecordTaskProcessed() {
	atomic.AddUint64(&m.TotalTasksProcessed, 1)
}

func (m *MetricsExporter) RecordTokens(tokens uint64) {
	atomic.AddUint64(&m.TotalTokensBurned, tokens)
}

func (m *MetricsExporter) RecordReview(passed bool) {
	if passed {
		atomic.AddUint64(&m.TotalReviewPasses, 1)
	} else {
		atomic.AddUint64(&m.TotalReviewFails, 1)
	}
}

// Handler returns HTTP handler for Prometheus scraping (/metrics)
func (m *MetricsExporter) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP herd_tasks_processed_total Total tasks processed by Herdforge\n")
		fmt.Fprintf(w, "# TYPE herd_tasks_processed_total counter\n")
		fmt.Fprintf(w, "herd_tasks_processed_total %d\n\n", atomic.LoadUint64(&m.TotalTasksProcessed))

		fmt.Fprintf(w, "# HELP herd_tokens_burned_total Total LLM tokens burned\n")
		fmt.Fprintf(w, "# TYPE herd_tokens_burned_total counter\n")
		fmt.Fprintf(w, "herd_tokens_burned_total %d\n\n", atomic.LoadUint64(&m.TotalTokensBurned))

		fmt.Fprintf(w, "# HELP herd_review_verdicts_total Total code review verdicts\n")
		fmt.Fprintf(w, "# TYPE herd_review_verdicts_total counter\n")
		fmt.Fprintf(w, "herd_review_verdicts_total{verdict=\"pass\"} %d\n", atomic.LoadUint64(&m.TotalReviewPasses))
		fmt.Fprintf(w, "herd_review_verdicts_total{verdict=\"fail\"} %d\n", atomic.LoadUint64(&m.TotalReviewFails))
	})
}
