package metrics

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DependencyState is deliberately small and closed. Unknown is distinct from
// healthy: an unavailable probe must never manufacture readiness or capacity.
type DependencyState string

const (
	DependencyHealthy   DependencyState = "healthy"
	DependencyUnhealthy DependencyState = "unhealthy"
	DependencyUnknown   DependencyState = "unknown"
)

type DependencyHealth struct {
	Name     string          `json:"name"`
	Critical bool            `json:"critical"`
	State    DependencyState `json:"state"`
	Error    string          `json:"error,omitempty"`
}

type HealthSnapshot struct {
	Liveness     bool               `json:"liveness"`
	Readiness    bool               `json:"readiness"`
	Dependencies []DependencyHealth `json:"dependencies"`
}

// QueuePressure keeps probe failures separate from numeric measurements.
// Known=false means Depth and Capacity are not an assertion of free capacity.
type QueuePressure struct {
	Depth    int64  `json:"depth"`
	Capacity int64  `json:"capacity"`
	Known    bool   `json:"known"`
	Error    string `json:"error,omitempty"`
}

type TransitionSLO struct {
	Attempts     uint64        `json:"attempts"`
	Completed    uint64        `json:"completed"`
	Failed       uint64        `json:"failed"`
	TotalLatency time.Duration `json:"total_latency"`
	ObservedAt   time.Time     `json:"observed_at"`
}

var defaultCriticalDependencies = []string{"event", "git", "herdr", "provider"}

func UnknownHealthSnapshot() HealthSnapshot {
	deps := make([]DependencyHealth, 0, len(defaultCriticalDependencies))
	for _, name := range defaultCriticalDependencies {
		deps = append(deps, DependencyHealth{Name: name, Critical: true, State: DependencyUnknown, Error: "not observed"})
	}
	return HealthSnapshot{Liveness: true, Readiness: false, Dependencies: deps}
}

func BuildHealthSnapshot(dependencies []DependencyHealth) (HealthSnapshot, error) {
	if err := validateDependencies(dependencies); err != nil {
		return UnknownHealthSnapshot(), err
	}
	deps := append([]DependencyHealth(nil), dependencies...)
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	ready := true
	for _, dep := range deps {
		if dep.Critical && dep.State != DependencyHealthy {
			ready = false
		}
	}
	return HealthSnapshot{Liveness: true, Readiness: ready, Dependencies: deps}, nil
}

func validateDependencies(dependencies []DependencyHealth) error {
	if len(dependencies) != len(defaultCriticalDependencies) {
		return fmt.Errorf("health requires exactly %d critical dependencies", len(defaultCriticalDependencies))
	}
	seen := make(map[string]struct{}, len(dependencies))
	for _, dep := range dependencies {
		if dep.Name == "" || dep.Name != strings.ToLower(dep.Name) {
			return errors.New("dependency names must be non-empty lowercase identifiers")
		}
		if _, ok := seen[dep.Name]; ok {
			return fmt.Errorf("duplicate dependency %q", dep.Name)
		}
		seen[dep.Name] = struct{}{}
		if dep.State != DependencyHealthy && dep.State != DependencyUnhealthy && dep.State != DependencyUnknown {
			return fmt.Errorf("invalid dependency state %q", dep.State)
		}
		if !dep.Critical {
			return fmt.Errorf("dependency %q must be critical", dep.Name)
		}
		if dep.State == DependencyHealthy && dep.Error != "" {
			return fmt.Errorf("healthy dependency %q cannot have an error", dep.Name)
		}
		if dep.State != DependencyHealthy && dep.Error == "" {
			return fmt.Errorf("dependency %q requires an error when not healthy", dep.Name)
		}
	}
	for _, required := range defaultCriticalDependencies {
		if _, ok := seen[required]; !ok {
			return fmt.Errorf("missing required dependency %q", required)
		}
	}
	return nil
}

func (q QueuePressure) Available() (int64, bool) {
	if !q.Known || q.Error != "" || q.Depth < 0 || q.Capacity < 0 {
		return 0, false
	}
	return q.Capacity - q.Depth, true
}

func (q QueuePressure) Validate() error {
	if q.Depth < 0 || q.Capacity < 0 {
		return errors.New("queue pressure cannot be negative")
	}
	if !q.Known && (q.Depth != 0 || q.Capacity != 0) {
		return errors.New("unknown queue pressure cannot carry measurements")
	}
	if q.Known && q.Error != "" {
		return errors.New("queue pressure cannot be known and failed")
	}
	if !q.Known && q.Error == "" {
		return errors.New("unknown queue pressure requires an error")
	}
	return nil
}

type MetricsExporter struct {
	mu                  sync.RWMutex
	TotalTasksProcessed uint64
	TotalTokensBurned   uint64
	TotalReviewPasses   uint64
	TotalReviewFails    uint64
	health              HealthSnapshot
	queue               QueuePressure
	slo                 TransitionSLO
}

func NewMetricsExporter() *MetricsExporter {
	return &MetricsExporter{health: UnknownHealthSnapshot(), queue: QueuePressure{Known: false, Error: "not observed"}}
}

func (m *MetricsExporter) SetHealth(dependencies []DependencyHealth) error {
	health, err := BuildHealthSnapshot(dependencies)
	if err != nil {
		m.mu.Lock()
		m.health = UnknownHealthSnapshot()
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	m.health = health
	m.mu.Unlock()
	return nil
}

func (m *MetricsExporter) SetQueuePressure(queue QueuePressure) error {
	if err := queue.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	m.queue = queue
	m.mu.Unlock()
	return nil
}

func (m *MetricsExporter) RecordTransition(start, end time.Time, transitionErr error, now time.Time) {
	m.mu.Lock()
	m.slo.Attempts++
	if transitionErr != nil {
		m.slo.Failed++
	} else if !end.Before(start) {
		m.slo.Completed++
		m.slo.TotalLatency += end.Sub(start)
	} else {
		m.slo.Failed++
	}
	m.slo.ObservedAt = now
	m.mu.Unlock()
}

func (m *MetricsExporter) Snapshot() (HealthSnapshot, QueuePressure, TransitionSLO) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.health, m.queue, m.slo
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

		health, queue, slo := m.Snapshot()
		ready := 0
		if health.Readiness {
			ready = 1
		}
		fmt.Fprintf(w, "\n# HELP herd_readiness_ready Whether critical dependencies are ready\n# TYPE herd_readiness_ready gauge\nherd_readiness_ready %d\n", ready)
		known := 0
		if queue.Known && queue.Error == "" {
			known = 1
		}
		fmt.Fprintf(w, "# HELP herd_queue_pressure_known Whether queue pressure is a valid observation\n# TYPE herd_queue_pressure_known gauge\nherd_queue_pressure_known %d\n", known)
		fmt.Fprintf(w, "# HELP herd_transitions_total Transition attempts by outcome\n# TYPE herd_transitions_total counter\nherd_transitions_total{outcome=\"completed\"} %d\nherd_transitions_total{outcome=\"failed\"} %d\n", slo.Completed, slo.Failed)
	})
}
