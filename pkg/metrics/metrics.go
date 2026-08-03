package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	ObservedAt   time.Time          `json:"observed_at,omitempty"`
}

// QueuePressure keeps probe failures separate from numeric measurements.
// Known=false means Depth and Capacity are not an assertion of free capacity.
type QueuePressure struct {
	Depth      int64     `json:"depth"`
	Capacity   int64     `json:"capacity"`
	Known      bool      `json:"known"`
	Error      string    `json:"error,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

type TransitionSLO struct {
	Attempts     uint64        `json:"attempts"`
	Completed    uint64        `json:"completed"`
	Failed       uint64        `json:"failed"`
	TotalLatency time.Duration `json:"total_latency"`
	ObservedAt   time.Time     `json:"observed_at"`
}

// FleetSignals is a bounded, aggregate-only observation. It deliberately has
// no task, ref, model, or raw-error dimensions.
type FleetSignals struct {
	StalledWork        uint64        `json:"stalled_work"`
	DroppedCallbacks   uint64        `json:"dropped_callbacks"`
	ReviewSaturation   uint8         `json:"review_saturation_percent"`
	DeadProvider       bool          `json:"dead_provider"`
	IntegrationBacklog uint64        `json:"integration_backlog"`
	Retries            uint64        `json:"retries"`
	DeadLetters        uint64        `json:"dead_letters"`
	MaxLeaseAge        time.Duration `json:"max_lease_age"`
	MaxCallbackAge     time.Duration `json:"max_callback_age"`
	EligibleIdle       time.Duration `json:"eligible_idle"`
	LastReconciliation time.Time     `json:"last_reconciliation"`
	ObservedAt         time.Time     `json:"observed_at"`
}

const (
	maxSignalCount = uint64(1_000_000_000)
	maxSignalAge   = 24 * time.Hour
)

type StateStore interface {
	Load(context.Context) ([]byte, error)
	Save(context.Context, []byte) error
}

type persistedState struct {
	Version             int            `json:"version"`
	TotalTasksProcessed uint64         `json:"total_tasks_processed"`
	TotalTokensBurned   uint64         `json:"total_tokens_burned"`
	TotalReviewPasses   uint64         `json:"total_review_passes"`
	TotalReviewFails    uint64         `json:"total_review_fails"`
	Health              HealthSnapshot `json:"health"`
	Queue               QueuePressure  `json:"queue"`
	SLO                 TransitionSLO  `json:"transition_slo"`
	Signals             FleetSignals   `json:"signals"`
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

func (s FleetSignals) Validate(now time.Time, maxAge time.Duration) error {
	if maxAge <= 0 {
		return errors.New("signal max age must be positive")
	}
	if s.ObservedAt.IsZero() || s.LastReconciliation.IsZero() {
		return errors.New("signals require observation and reconciliation timestamps")
	}
	if s.ObservedAt.After(now) || s.LastReconciliation.After(now) {
		return errors.New("signals cannot be from the future")
	}
	if now.Sub(s.ObservedAt) > maxAge || now.Sub(s.LastReconciliation) > maxAge {
		return errors.New("signals are stale")
	}
	if s.ReviewSaturation > 100 {
		return errors.New("review saturation must be between 0 and 100")
	}
	for _, count := range []uint64{s.StalledWork, s.DroppedCallbacks, s.IntegrationBacklog, s.Retries, s.DeadLetters} {
		if count > maxSignalCount {
			return errors.New("signal count exceeds bounded maximum")
		}
	}
	for _, age := range []time.Duration{s.MaxLeaseAge, s.MaxCallbackAge, s.EligibleIdle} {
		if age < 0 || age > maxSignalAge {
			return errors.New("signal age is outside bounded range")
		}
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
	signals             FleetSignals
	store               StateStore
	now                 func() time.Time
}

func NewMetricsExporter() *MetricsExporter {
	return NewMetricsExporterWithPersistence(nil, time.Now)
}

func NewMetricsExporterWithPersistence(store StateStore, now func() time.Time) *MetricsExporter {
	if now == nil {
		now = time.Now
	}
	return &MetricsExporter{
		health: UnknownHealthSnapshot(),
		queue:  QueuePressure{Known: false, Error: "not observed"},
		store:  store,
		now:    now,
	}
}

func (m *MetricsExporter) SetHealth(dependencies []DependencyHealth) error {
	return m.SetHealthAt(dependencies, m.now())
}

func (m *MetricsExporter) SetHealthAt(dependencies []DependencyHealth, observedAt time.Time) error {
	health, err := BuildHealthSnapshot(dependencies)
	if err != nil || !observationFresh(observedAt, m.now()) {
		m.mu.Lock()
		m.health = UnknownHealthSnapshot()
		m.mu.Unlock()
		if err == nil {
			err = errors.New("health observation is stale or invalid")
		}
		return err
	}
	health.ObservedAt = observedAt
	m.mu.Lock()
	m.health = health
	m.mu.Unlock()
	return nil
}

func (m *MetricsExporter) SetQueuePressure(queue QueuePressure) error {
	return m.SetQueuePressureAt(queue, m.now())
}

func (m *MetricsExporter) SetQueuePressureAt(queue QueuePressure, observedAt time.Time) error {
	if err := queue.Validate(); err != nil || (!queue.ObservedAt.IsZero() && !queue.ObservedAt.Equal(observedAt)) || !observationFresh(observedAt, m.now()) {
		m.mu.Lock()
		m.queue = QueuePressure{Known: false, Error: "not observed"}
		m.mu.Unlock()
		if err == nil {
			err = errors.New("queue observation is stale or invalid")
		}
		return err
	}
	queue.ObservedAt = observedAt
	m.mu.Lock()
	m.queue = queue
	m.mu.Unlock()
	return nil
}

func observationFresh(observedAt, now time.Time) bool {
	return !observedAt.IsZero() && !observedAt.After(now) && now.Sub(observedAt) <= maxSignalAge
}

func (m *MetricsExporter) SetSignals(signals FleetSignals) error {
	if err := signals.Validate(m.now(), maxSignalAge); err != nil {
		m.mu.Lock()
		m.signals = FleetSignals{}
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	m.signals = signals
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

func (m *MetricsExporter) Signals() FleetSignals {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.signals
}

func (m *MetricsExporter) resetUnsafeState() {
	m.mu.Lock()
	m.health = UnknownHealthSnapshot()
	m.queue = QueuePressure{Known: false, Error: "not observed"}
	m.slo = TransitionSLO{}
	m.signals = FleetSignals{}
	m.mu.Unlock()
	atomic.StoreUint64(&m.TotalTasksProcessed, 0)
	atomic.StoreUint64(&m.TotalTokensBurned, 0)
	atomic.StoreUint64(&m.TotalReviewPasses, 0)
	atomic.StoreUint64(&m.TotalReviewFails, 0)
}

func (m *MetricsExporter) Persist(ctx context.Context) error {
	if m.store == nil {
		return errors.New("metrics state store is not configured")
	}
	health, queue, slo := m.Snapshot()
	state := persistedState{
		Version:             1,
		TotalTasksProcessed: atomic.LoadUint64(&m.TotalTasksProcessed),
		TotalTokensBurned:   atomic.LoadUint64(&m.TotalTokensBurned),
		TotalReviewPasses:   atomic.LoadUint64(&m.TotalReviewPasses),
		TotalReviewFails:    atomic.LoadUint64(&m.TotalReviewFails),
		Health:              health,
		Queue:               queue,
		SLO:                 slo,
		Signals:             m.Signals(),
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal metrics state: %w", err)
	}
	if err := m.store.Save(ctx, payload); err != nil {
		return fmt.Errorf("save metrics state: %w", err)
	}
	return nil
}

func (m *MetricsExporter) Restore(ctx context.Context) error {
	if m.store == nil {
		return errors.New("metrics state store is not configured")
	}
	invalidRestore := func(err error) error {
		m.resetUnsafeState()
		return err
	}
	payload, err := m.store.Load(ctx)
	if err != nil {
		return invalidRestore(fmt.Errorf("load metrics state: %w", err))
	}
	var state persistedState
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return invalidRestore(fmt.Errorf("decode metrics state: %w", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return invalidRestore(errors.New("metrics state has trailing data"))
	}
	if state.Version != 1 {
		return invalidRestore(fmt.Errorf("unsupported metrics state version %d", state.Version))
	}
	if _, err := BuildHealthSnapshot(state.Health.Dependencies); err != nil || !observationFresh(state.Health.ObservedAt, m.now()) {
		return invalidRestore(errors.New("invalid persisted health state"))
	}
	if err := state.Queue.Validate(); err != nil || !observationFresh(state.Queue.ObservedAt, m.now()) {
		return invalidRestore(errors.New("invalid persisted queue state"))
	}
	if err := state.Signals.Validate(m.now(), maxSignalAge); err != nil {
		return invalidRestore(fmt.Errorf("invalid persisted signals: %w", err))
	}
	if state.SLO.Completed > state.SLO.Attempts || state.SLO.Failed > state.SLO.Attempts || state.SLO.Completed+state.SLO.Failed != state.SLO.Attempts {
		return invalidRestore(errors.New("invalid persisted transition totals"))
	}
	m.mu.Lock()
	m.health, m.queue, m.slo, m.signals = state.Health, state.Queue, state.SLO, state.Signals
	m.mu.Unlock()
	atomic.StoreUint64(&m.TotalTasksProcessed, state.TotalTasksProcessed)
	atomic.StoreUint64(&m.TotalTokensBurned, state.TotalTokensBurned)
	atomic.StoreUint64(&m.TotalReviewPasses, state.TotalReviewPasses)
	atomic.StoreUint64(&m.TotalReviewFails, state.TotalReviewFails)
	return nil
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

		signals := m.Signals()
		signalsKnown := 0
		if !signals.ObservedAt.IsZero() {
			signalsKnown = 1
		}
		fmt.Fprintf(w, "# HELP herd_liveness_alive Whether the process is alive\n# TYPE herd_liveness_alive gauge\nherd_liveness_alive %d\n", boolGauge(health.Liveness))
		fmt.Fprintf(w, "# HELP herd_fleet_signals_known Whether aggregate fleet signals are authoritative\n# TYPE herd_fleet_signals_known gauge\nherd_fleet_signals_known %d\n", signalsKnown)
		if signalsKnown == 1 {
			fmt.Fprintf(w, "# HELP herd_stalled_work_total Distinct aggregate stalled work\n# TYPE herd_stalled_work_total gauge\nherd_stalled_work_total %d\n", signals.StalledWork)
			fmt.Fprintf(w, "# HELP herd_dropped_callbacks_total Dropped callback count\n# TYPE herd_dropped_callbacks_total counter\nherd_dropped_callbacks_total %d\n", signals.DroppedCallbacks)
			fmt.Fprintf(w, "# HELP herd_review_saturation_ratio Review saturation ratio\n# TYPE herd_review_saturation_ratio gauge\nherd_review_saturation_ratio %.2f\n", float64(signals.ReviewSaturation)/100)
			fmt.Fprintf(w, "# HELP herd_dead_provider Whether a provider is considered dead\n# TYPE herd_dead_provider gauge\nherd_dead_provider %d\n", boolGauge(signals.DeadProvider))
			fmt.Fprintf(w, "# HELP herd_integration_backlog Integration backlog\n# TYPE herd_integration_backlog gauge\nherd_integration_backlog %d\n", signals.IntegrationBacklog)
			fmt.Fprintf(w, "# HELP herd_retries_total Retry count\n# TYPE herd_retries_total counter\nherd_retries_total %d\n", signals.Retries)
			fmt.Fprintf(w, "# HELP herd_dead_letters_total Dead-letter count\n# TYPE herd_dead_letters_total counter\nherd_dead_letters_total %d\n", signals.DeadLetters)
			fmt.Fprintf(w, "# HELP herd_max_lease_age_seconds Maximum lease age\n# TYPE herd_max_lease_age_seconds gauge\nherd_max_lease_age_seconds %.6f\n", signals.MaxLeaseAge.Seconds())
			fmt.Fprintf(w, "# HELP herd_max_callback_age_seconds Maximum callback age\n# TYPE herd_max_callback_age_seconds gauge\nherd_max_callback_age_seconds %.6f\n", signals.MaxCallbackAge.Seconds())
			fmt.Fprintf(w, "# HELP herd_eligible_idle_seconds Eligible idle time excluding blocked or backpressured work\n# TYPE herd_eligible_idle_seconds gauge\nherd_eligible_idle_seconds %.6f\n", signals.EligibleIdle.Seconds())
			fmt.Fprintf(w, "# HELP herd_last_reconciliation_timestamp_seconds Last reconciliation timestamp\n# TYPE herd_last_reconciliation_timestamp_seconds gauge\nherd_last_reconciliation_timestamp_seconds %.6f\n", float64(signals.LastReconciliation.UnixNano())/1e9)
		}
	})
}

func boolGauge(value bool) int {
	if value {
		return 1
	}
	return 0
}
