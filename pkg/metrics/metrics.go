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
	Sequence     uint64             `json:"sequence,omitempty"`
}

// QueuePressure keeps probe failures separate from numeric measurements.
// Known=false means Depth and Capacity are not an assertion of free capacity.
type QueuePressure struct {
	Depth      int64     `json:"depth"`
	Capacity   int64     `json:"capacity"`
	Known      bool      `json:"known"`
	Error      string    `json:"error,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
	Sequence   uint64    `json:"sequence,omitempty"`
}

type TransitionSLO struct {
	Attempts     uint64        `json:"attempts"`
	Completed    uint64        `json:"completed"`
	Failed       uint64        `json:"failed"`
	TotalLatency time.Duration `json:"total_latency"`
	ObservedAt   time.Time     `json:"observed_at"`
	Sequence     uint64        `json:"sequence,omitempty"`
}

type ConditionCode string

const (
	ConditionStalledWork        ConditionCode = "stalled_work"
	ConditionDroppedCallback    ConditionCode = "dropped_callback"
	ConditionReviewSaturation   ConditionCode = "review_saturation"
	ConditionDeadProvider       ConditionCode = "dead_provider"
	ConditionIntegrationBacklog ConditionCode = "integration_backlog"
	ConditionDeadLetters        ConditionCode = "dead_letters"
	ConditionEligibleIdle       ConditionCode = "eligible_idle"
)

type FreshnessConfig struct {
	HealthMaxAge  time.Duration `json:"health_max_age"`
	QueueMaxAge   time.Duration `json:"queue_max_age"`
	SignalsMaxAge time.Duration `json:"signals_max_age"`
	SLOMaxAge     time.Duration `json:"slo_max_age"`
}

var DefaultFreshnessConfig = FreshnessConfig{
	HealthMaxAge:  5 * time.Minute,
	QueueMaxAge:   5 * time.Minute,
	SignalsMaxAge: 5 * time.Minute,
	SLOMaxAge:     5 * time.Minute,
}

type FreshnessState struct {
	HealthFresh  bool      `json:"health_fresh"`
	QueueFresh   bool      `json:"queue_fresh"`
	SignalsFresh bool      `json:"signals_fresh"`
	SLOFresh     bool      `json:"slo_fresh"`
	Ready        bool      `json:"ready"`
	AsOf         time.Time `json:"as_of"`
}

type SnapshotView struct {
	Health     HealthSnapshot  `json:"health"`
	Queue      QueuePressure   `json:"queue"`
	SLO        TransitionSLO   `json:"transition_slo"`
	Signals    FleetSignals    `json:"signals"`
	Freshness  FreshnessState  `json:"freshness"`
	Conditions []ConditionCode `json:"condition_codes"`
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
	EligibleWaiting    uint64        `json:"eligible_waiting"`
	Blocked            bool          `json:"blocked"`
	Backpressured      bool          `json:"backpressured"`
	EligibleSince      time.Time     `json:"eligible_since,omitempty"`
	LastReconciliation time.Time     `json:"last_reconciliation"`
	ObservedAt         time.Time     `json:"observed_at"`
	Sequence           uint64        `json:"sequence,omitempty"`
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
	if q.Known && q.Depth > q.Capacity {
		return errors.New("queue depth cannot exceed capacity")
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
	if s.EligibleWaiting == 0 {
		if !s.EligibleSince.IsZero() || s.EligibleIdle != 0 {
			return errors.New("eligible idle requires eligible waiting work")
		}
	} else {
		if s.EligibleSince.IsZero() || s.EligibleSince.After(now) {
			return errors.New("eligible waiting work requires a valid start time")
		}
		if s.Blocked || s.Backpressured {
			if s.EligibleIdle != 0 {
				return errors.New("blocked or backpressured work cannot be eligible idle")
			}
		} else if expected := now.Sub(s.EligibleSince); s.EligibleIdle != expected {
			return errors.New("eligible idle does not match eligible waiting age")
		}
		if (s.ReviewSaturation == 100 || s.IntegrationBacklog > 0) && !s.Backpressured {
			return errors.New("eligible work contradicts saturation or integration backlog")
		}
	}
	return nil
}

func validateFreshnessConfig(config FreshnessConfig) error {
	for _, age := range []time.Duration{config.HealthMaxAge, config.QueueMaxAge, config.SignalsMaxAge, config.SLOMaxAge} {
		if age <= 0 || age > maxSignalAge {
			return errors.New("freshness thresholds must be positive and bounded")
		}
	}
	return nil
}

func observationNewer(sequence uint64, observedAt time.Time, currentSequence uint64, currentAt time.Time) bool {
	if sequence > 0 || currentSequence > 0 {
		return sequence > currentSequence
	}
	return currentAt.IsZero() || !observedAt.Before(currentAt)
}

func freshAt(observedAt, now time.Time, maxAge time.Duration) bool {
	return !observedAt.IsZero() && !observedAt.After(now) && now.Sub(observedAt) <= maxAge
}

func (s FleetSignals) ConditionCodes() []ConditionCode {
	conditions := make([]ConditionCode, 0, 7)
	if s.StalledWork > 0 {
		conditions = append(conditions, ConditionStalledWork)
	}
	if s.DroppedCallbacks > 0 {
		conditions = append(conditions, ConditionDroppedCallback)
	}
	if s.ReviewSaturation >= 80 {
		conditions = append(conditions, ConditionReviewSaturation)
	}
	if s.DeadProvider {
		conditions = append(conditions, ConditionDeadProvider)
	}
	if s.IntegrationBacklog > 0 {
		conditions = append(conditions, ConditionIntegrationBacklog)
	}
	if s.DeadLetters > 0 {
		conditions = append(conditions, ConditionDeadLetters)
	}
	if s.EligibleWaiting > 0 && !s.Blocked && !s.Backpressured && s.EligibleIdle > 0 {
		conditions = append(conditions, ConditionEligibleIdle)
	}
	return conditions
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
	thresholds          FreshnessConfig
}

func NewMetricsExporter() *MetricsExporter {
	return NewMetricsExporterWithPersistence(nil, time.Now)
}

func NewMetricsExporterWithPersistence(store StateStore, now func() time.Time) *MetricsExporter {
	return NewMetricsExporterWithConfig(store, now, DefaultFreshnessConfig)
}

func NewMetricsExporterWithConfig(store StateStore, now func() time.Time, thresholds FreshnessConfig) *MetricsExporter {
	if now == nil {
		now = time.Now
	}
	if validateFreshnessConfig(thresholds) != nil {
		thresholds = DefaultFreshnessConfig
	}
	return &MetricsExporter{
		health:     UnknownHealthSnapshot(),
		queue:      QueuePressure{Known: false, Error: "not observed"},
		store:      store,
		now:        now,
		thresholds: thresholds,
	}
}

func (m *MetricsExporter) SetHealth(dependencies []DependencyHealth) error {
	return m.SetHealthAt(dependencies, m.now())
}

func (m *MetricsExporter) SetHealthAt(dependencies []DependencyHealth, observedAt time.Time) error {
	return m.SetHealthObservation(dependencies, observedAt, 0)
}

func (m *MetricsExporter) SetHealthObservation(dependencies []DependencyHealth, observedAt time.Time, sequence uint64) error {
	health, err := BuildHealthSnapshot(dependencies)
	now := m.now()
	m.mu.Lock()
	if !observationNewer(sequence, observedAt, m.health.Sequence, m.health.ObservedAt) {
		m.mu.Unlock()
		return errors.New("stale health observation")
	}
	if err != nil || !freshAt(observedAt, now, m.thresholds.HealthMaxAge) {
		m.health = UnknownHealthSnapshot()
		m.mu.Unlock()
		if err == nil {
			err = errors.New("health observation is stale or invalid")
		}
		return err
	}
	health.ObservedAt = observedAt
	if m.signals.DeadProvider && dependencyState(health, "provider") == DependencyHealthy {
		m.health = UnknownHealthSnapshot()
		m.mu.Unlock()
		return errors.New("healthy provider contradicts dead-provider signal")
	}
	health.Sequence = sequence
	m.health = health
	m.mu.Unlock()
	return nil
}

func (m *MetricsExporter) SetQueuePressure(queue QueuePressure) error {
	return m.SetQueuePressureAt(queue, m.now())
}

func (m *MetricsExporter) SetQueuePressureAt(queue QueuePressure, observedAt time.Time) error {
	return m.SetQueuePressureObservation(queue, observedAt, queue.Sequence)
}

func (m *MetricsExporter) SetQueuePressureObservation(queue QueuePressure, observedAt time.Time, sequence uint64) error {
	now := m.now()
	validationErr := queue.Validate()
	if validationErr == nil && (!queue.ObservedAt.IsZero() && !queue.ObservedAt.Equal(observedAt) || !freshAt(observedAt, now, m.thresholds.QueueMaxAge)) {
		validationErr = errors.New("queue observation is stale or invalid")
	}
	m.mu.Lock()
	if !observationNewer(sequence, observedAt, m.queue.Sequence, m.queue.ObservedAt) {
		m.mu.Unlock()
		return errors.New("stale queue observation")
	}
	if validationErr != nil {
		m.queue = QueuePressure{Known: false, Error: "not observed"}
		m.mu.Unlock()
		return validationErr
	}
	queue.ObservedAt = observedAt
	queue.Sequence = sequence
	m.queue = queue
	m.mu.Unlock()
	return nil
}

func observationFresh(observedAt, now time.Time) bool {
	return freshAt(observedAt, now, maxSignalAge)
}

func (m *MetricsExporter) SetSignals(signals FleetSignals) error {
	return m.SetSignalsObservation(signals, signals.Sequence)
}

func (m *MetricsExporter) SetSignalsObservation(signals FleetSignals, sequence uint64) error {
	now := m.now()
	m.mu.Lock()
	if !observationNewer(sequence, signals.ObservedAt, m.signals.Sequence, m.signals.ObservedAt) {
		m.mu.Unlock()
		return errors.New("stale signal observation")
	}
	if err := signals.Validate(now, m.thresholds.SignalsMaxAge); err != nil {
		m.signals = FleetSignals{}
		m.mu.Unlock()
		return err
	}
	if signals.DeadProvider && dependencyState(m.health, "provider") == DependencyHealthy {
		m.signals = FleetSignals{}
		m.mu.Unlock()
		return errors.New("dead-provider signal contradicts healthy provider")
	}
	signals.Sequence = sequence
	m.signals = signals
	m.mu.Unlock()
	return nil
}

func dependencyState(health HealthSnapshot, name string) DependencyState {
	for _, dependency := range health.Dependencies {
		if dependency.Name == name {
			return dependency.State
		}
	}
	return DependencyUnknown
}

func (m *MetricsExporter) RecordTransition(start, end time.Time, transitionErr error, now time.Time) error {
	return m.RecordTransitionObservation(start, end, transitionErr, now, 0)
}

func (m *MetricsExporter) RecordTransitionObservation(start, end time.Time, transitionErr error, now time.Time, sequence uint64) error {
	if end.Before(start) || end.Sub(start) > maxSignalAge {
		return errors.New("transition latency is invalid or unbounded")
	}
	m.mu.Lock()
	if !observationNewer(sequence, now, m.slo.Sequence, m.slo.ObservedAt) {
		m.mu.Unlock()
		return errors.New("stale transition observation")
	}
	m.slo.Attempts++
	if transitionErr != nil {
		m.slo.Failed++
	} else if !end.Before(start) {
		latency := end.Sub(start)
		if m.slo.TotalLatency > maxSignalAge-latency {
			m.mu.Unlock()
			return errors.New("transition latency total exceeds bounded maximum")
		}
		m.slo.Completed++
		m.slo.TotalLatency += latency
	} else {
		m.slo.Failed++
	}
	m.slo.ObservedAt = now
	m.slo.Sequence = sequence
	m.mu.Unlock()
	return nil
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

func (m *MetricsExporter) ReadAt(now time.Time) SnapshotView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	health, queue, slo, signals := m.health, m.queue, m.slo, m.signals
	freshness := FreshnessState{
		HealthFresh:  health.Readiness && freshAt(health.ObservedAt, now, m.thresholds.HealthMaxAge),
		QueueFresh:   queue.Known && queue.Error == "" && freshAt(queue.ObservedAt, now, m.thresholds.QueueMaxAge),
		SignalsFresh: signals.Validate(now, m.thresholds.SignalsMaxAge) == nil,
		SLOFresh:     slo.Attempts == 0 || freshAt(slo.ObservedAt, now, m.thresholds.SLOMaxAge),
		AsOf:         now,
	}
	freshness.Ready = freshness.HealthFresh && freshness.QueueFresh && freshness.SignalsFresh && freshness.SLOFresh
	view := SnapshotView{Health: health, Queue: queue, SLO: slo, Signals: signals, Freshness: freshness}
	if freshness.SignalsFresh {
		view.Conditions = signals.ConditionCodes()
	}
	return view
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
	now := m.now()
	m.mu.RLock()
	state := persistedState{
		Version:             1,
		TotalTasksProcessed: atomic.LoadUint64(&m.TotalTasksProcessed),
		TotalTokensBurned:   atomic.LoadUint64(&m.TotalTokensBurned),
		TotalReviewPasses:   atomic.LoadUint64(&m.TotalReviewPasses),
		TotalReviewFails:    atomic.LoadUint64(&m.TotalReviewFails),
		Health:              m.health,
		Queue:               m.queue,
		SLO:                 m.slo,
		Signals:             m.signals,
	}
	validationErr := m.validateStateLocked(state, now)
	m.mu.RUnlock()
	if validationErr != nil {
		return validationErr
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

func (m *MetricsExporter) validateStateLocked(state persistedState, now time.Time) error {
	if state.Version != 1 {
		return fmt.Errorf("unsupported metrics state version %d", state.Version)
	}
	rebuiltHealth, err := BuildHealthSnapshot(state.Health.Dependencies)
	if err != nil || !state.Health.Liveness || state.Health.Readiness != rebuiltHealth.Readiness || !freshAt(state.Health.ObservedAt, now, m.thresholds.HealthMaxAge) {
		return errors.New("invalid metrics health state")
	}
	if err := state.Queue.Validate(); err != nil || !freshAt(state.Queue.ObservedAt, now, m.thresholds.QueueMaxAge) {
		return errors.New("invalid metrics queue state")
	}
	if err := state.Signals.Validate(now, m.thresholds.SignalsMaxAge); err != nil {
		return fmt.Errorf("invalid metrics signals: %w", err)
	}
	if state.Signals.DeadProvider && dependencyState(state.Health, "provider") == DependencyHealthy {
		return errors.New("metrics state contradicts provider health")
	}
	if state.SLO.Completed > state.SLO.Attempts || state.SLO.Failed > state.SLO.Attempts || state.SLO.Completed+state.SLO.Failed != state.SLO.Attempts {
		return errors.New("invalid metrics transition totals")
	}
	if state.SLO.TotalLatency < 0 || state.SLO.Completed == 0 && state.SLO.TotalLatency != 0 || state.SLO.TotalLatency > maxSignalAge {
		return errors.New("invalid metrics transition latency")
	}
	if state.SLO.Attempts > 0 && !freshAt(state.SLO.ObservedAt, now, m.thresholds.SLOMaxAge) {
		return errors.New("invalid metrics transition timestamp")
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
	now := m.now()
	m.mu.RLock()
	validationErr := m.validateStateLocked(state, now)
	m.mu.RUnlock()
	if validationErr != nil {
		return invalidRestore(validationErr)
	}
	m.mu.Lock()
	m.health, m.queue, m.slo, m.signals = state.Health, state.Queue, state.SLO, state.Signals
	atomic.StoreUint64(&m.TotalTasksProcessed, state.TotalTasksProcessed)
	atomic.StoreUint64(&m.TotalTokensBurned, state.TotalTokensBurned)
	atomic.StoreUint64(&m.TotalReviewPasses, state.TotalReviewPasses)
	atomic.StoreUint64(&m.TotalReviewFails, state.TotalReviewFails)
	m.mu.Unlock()
	return nil
}

func (m *MetricsExporter) RecordTaskProcessed() {
	m.mu.Lock()
	atomic.AddUint64(&m.TotalTasksProcessed, 1)
	m.mu.Unlock()
}

func (m *MetricsExporter) RecordTokens(tokens uint64) {
	m.mu.Lock()
	atomic.AddUint64(&m.TotalTokensBurned, tokens)
	m.mu.Unlock()
}

func (m *MetricsExporter) RecordReview(passed bool) {
	m.mu.Lock()
	if passed {
		atomic.AddUint64(&m.TotalReviewPasses, 1)
	} else {
		atomic.AddUint64(&m.TotalReviewFails, 1)
	}
	m.mu.Unlock()
}

// Handler returns HTTP handler for Prometheus scraping (/metrics)
func (m *MetricsExporter) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		now := m.now()
		view := m.ReadAt(now)
		health, queue, slo, signals := view.Health, view.Queue, view.SLO, view.Signals
		m.mu.RLock()
		tasks, tokens := atomic.LoadUint64(&m.TotalTasksProcessed), atomic.LoadUint64(&m.TotalTokensBurned)
		passes, fails := atomic.LoadUint64(&m.TotalReviewPasses), atomic.LoadUint64(&m.TotalReviewFails)
		m.mu.RUnlock()
		var body strings.Builder
		fmt.Fprintf(&body, "# HELP herd_tasks_processed_total Total tasks processed by Herdforge\n# TYPE herd_tasks_processed_total counter\nherd_tasks_processed_total %d\n\n", tasks)

		fmt.Fprintf(&body, "# HELP herd_tokens_burned_total Total LLM tokens burned\n# TYPE herd_tokens_burned_total counter\nherd_tokens_burned_total %d\n\n", tokens)

		fmt.Fprintf(&body, "# HELP herd_review_verdicts_total Total code review verdicts\n# TYPE herd_review_verdicts_total counter\nherd_review_verdicts_total{verdict=\"pass\"} %d\nherd_review_verdicts_total{verdict=\"fail\"} %d\n", passes, fails)
		ready := 0
		if view.Freshness.Ready {
			ready = 1
		}
		fmt.Fprintf(&body, "\n# HELP herd_readiness_ready Whether critical dependencies and observations are ready\n# TYPE herd_readiness_ready gauge\nherd_readiness_ready %d\n", ready)
		known := 0
		if view.Freshness.QueueFresh {
			known = 1
		}
		fmt.Fprintf(&body, "# HELP herd_queue_pressure_known Whether queue pressure is a fresh valid observation\n# TYPE herd_queue_pressure_known gauge\nherd_queue_pressure_known %d\n", known)
		if known == 1 {
			fmt.Fprintf(&body, "# HELP herd_queue_depth Current queue depth\n# TYPE herd_queue_depth gauge\nherd_queue_depth %d\n# HELP herd_queue_capacity Current queue capacity\n# TYPE herd_queue_capacity gauge\nherd_queue_capacity %d\n", queue.Depth, queue.Capacity)
		}
		fmt.Fprintf(&body, "# HELP herd_transitions_total Transition attempts by outcome\n# TYPE herd_transitions_total counter\nherd_transitions_total{outcome=\"completed\"} %d\nherd_transitions_total{outcome=\"failed\"} %d\n", slo.Completed, slo.Failed)
		if view.Freshness.SLOFresh && slo.Completed > 0 {
			fmt.Fprintf(&body, "# HELP herd_transition_latency_seconds Average completed transition latency\n# TYPE herd_transition_latency_seconds gauge\nherd_transition_latency_seconds %.6f\n", slo.TotalLatency.Seconds()/float64(slo.Completed))
		}
		signalsKnown := 0
		if view.Freshness.SignalsFresh {
			signalsKnown = 1
		}
		fmt.Fprintf(&body, "# HELP herd_liveness_alive Whether the process is alive\n# TYPE herd_liveness_alive gauge\nherd_liveness_alive %d\n", boolGauge(health.Liveness))
		fmt.Fprintf(&body, "# HELP herd_fleet_signals_known Whether aggregate fleet signals are fresh and authoritative\n# TYPE herd_fleet_signals_known gauge\nherd_fleet_signals_known %d\n", signalsKnown)
		if signalsKnown == 1 {
			fmt.Fprintf(&body, "# HELP herd_stalled_work Distinct aggregate stalled work\n# TYPE herd_stalled_work gauge\nherd_stalled_work %d\n", signals.StalledWork)
			fmt.Fprintf(&body, "# HELP herd_dropped_callbacks Dropped callback count\n# TYPE herd_dropped_callbacks gauge\nherd_dropped_callbacks %d\n", signals.DroppedCallbacks)
			fmt.Fprintf(&body, "# HELP herd_review_saturation_ratio Review saturation ratio\n# TYPE herd_review_saturation_ratio gauge\nherd_review_saturation_ratio %.2f\n", float64(signals.ReviewSaturation)/100)
			fmt.Fprintf(&body, "# HELP herd_dead_provider Whether a provider is considered dead\n# TYPE herd_dead_provider gauge\nherd_dead_provider %d\n", boolGauge(signals.DeadProvider))
			fmt.Fprintf(&body, "# HELP herd_integration_backlog Integration backlog\n# TYPE herd_integration_backlog gauge\nherd_integration_backlog %d\n", signals.IntegrationBacklog)
			fmt.Fprintf(&body, "# HELP herd_retries Retry count\n# TYPE herd_retries gauge\nherd_retries %d\n", signals.Retries)
			fmt.Fprintf(&body, "# HELP herd_dead_letters Dead-letter count\n# TYPE herd_dead_letters gauge\nherd_dead_letters %d\n", signals.DeadLetters)
			fmt.Fprintf(&body, "# HELP herd_max_lease_age_seconds Maximum lease age\n# TYPE herd_max_lease_age_seconds gauge\nherd_max_lease_age_seconds %.6f\n", signals.MaxLeaseAge.Seconds())
			fmt.Fprintf(&body, "# HELP herd_max_callback_age_seconds Maximum callback age\n# TYPE herd_max_callback_age_seconds gauge\nherd_max_callback_age_seconds %.6f\n", signals.MaxCallbackAge.Seconds())
			fmt.Fprintf(&body, "# HELP herd_eligible_idle_seconds Eligible idle time excluding blocked or backpressured work\n# TYPE herd_eligible_idle_seconds gauge\nherd_eligible_idle_seconds %.6f\n", signals.EligibleIdle.Seconds())
			fmt.Fprintf(&body, "# HELP herd_last_reconciliation_timestamp_seconds Last reconciliation timestamp\n# TYPE herd_last_reconciliation_timestamp_seconds gauge\nherd_last_reconciliation_timestamp_seconds %.6f\n", float64(signals.LastReconciliation.UnixNano())/1e9)
		}
		_, _ = io.WriteString(w, body.String())
	})
}

func boolGauge(value bool) int {
	if value {
		return 1
	}
	return 0
}
