package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
)

// Disk-pressure metrics are bounded-cardinality by construction (FAC-153):
// states and volume roles are closed sets; anything else is coerced, never
// used as a label value.
var (
	diskStates = []string{"ok", "recovering", "blocked", "unknown"}
	diskRoles  = map[string]bool{"repo": true, "pool": true, "temp": true}
)

type diskVolumeMetric struct {
	FreeBytes uint64
	FreePct   float64
}

type MetricsExporter struct {
	TotalTasksProcessed uint64
	TotalTokensBurned   uint64
	TotalReviewPasses   uint64
	TotalReviewFails    uint64

	mu             sync.Mutex
	diskState      string
	diskVolumes    map[string]diskVolumeMetric
	diskUnreadable map[string]bool
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

// SetDiskState records the disk guard projection (ok | recovering |
// blocked). Any other value is coerced to "unknown" so state can never
// open the label set.
func (m *MetricsExporter) SetDiskState(state string) {
	switch state {
	case "ok", "recovering", "blocked":
	default:
		state = "unknown"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diskState = state
}

// SetDiskVolume records free-space headroom for one volume role. Roles are
// the closed set repo | pool | temp; anything else is coerced to "other"
// so per-path probes can never explode cardinality.
func (m *MetricsExporter) SetDiskVolume(role string, freeBytes uint64, freePct float64) {
	if !diskRoles[role] {
		role = "other"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.diskVolumes == nil {
		m.diskVolumes = make(map[string]diskVolumeMetric, 4)
	}
	m.diskVolumes[role] = diskVolumeMetric{FreeBytes: freeBytes, FreePct: freePct}
	if m.diskUnreadable == nil {
		m.diskUnreadable = make(map[string]bool, 4)
	}
	m.diskUnreadable[role] = false
}

// SetDiskVolumeUnreadable marks a bounded role's probe as failed. An
// unreadable volume must be visible — never silently absent while the
// process looks healthy.
func (m *MetricsExporter) SetDiskVolumeUnreadable(role string) {
	if !diskRoles[role] {
		role = "other"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.diskUnreadable == nil {
		m.diskUnreadable = make(map[string]bool, 4)
	}
	m.diskUnreadable[role] = true
}

// writeDiskMetrics renders the bounded disk gauges (one-hot state plus
// per-role free bytes/percent) in deterministic order.
func (m *MetricsExporter) writeDiskMetrics(w http.ResponseWriter) {
	m.mu.Lock()
	state := m.diskState
	volumes := make(map[string]diskVolumeMetric, len(m.diskVolumes))
	for k, v := range m.diskVolumes {
		volumes[k] = v
	}
	unreadable := make(map[string]bool, len(m.diskUnreadable))
	for k, v := range m.diskUnreadable {
		unreadable[k] = v
	}
	m.mu.Unlock()

	if state == "" && len(volumes) == 0 && len(unreadable) == 0 {
		return // never probed; emit nothing rather than a fake ok
	}

	fmt.Fprintf(w, "# HELP herd_disk_pressure_state Disk guard state (one-hot; FAC-153)\n")
	fmt.Fprintf(w, "# TYPE herd_disk_pressure_state gauge\n")
	for _, s := range diskStates {
		v := 0
		if s == state {
			v = 1
		}
		fmt.Fprintf(w, "herd_disk_pressure_state{state=%q} %d\n", s, v)
	}
	fmt.Fprintf(w, "\n")

	roles := make([]string, 0, len(volumes))
	for r := range volumes {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	fmt.Fprintf(w, "# HELP herd_disk_free_bytes Free bytes on the volume for a bounded role\n")
	fmt.Fprintf(w, "# TYPE herd_disk_free_bytes gauge\n")
	for _, r := range roles {
		fmt.Fprintf(w, "herd_disk_free_bytes{volume=%q} %d\n", r, volumes[r].FreeBytes)
	}
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "# HELP herd_disk_free_pct Free percent on the volume for a bounded role\n")
	fmt.Fprintf(w, "# TYPE herd_disk_free_pct gauge\n")
	for _, r := range roles {
		fmt.Fprintf(w, "herd_disk_free_pct{volume=%q} %.2f\n", r, volumes[r].FreePct)
	}

	if len(unreadable) > 0 {
		uroles := make([]string, 0, len(unreadable))
		for r := range unreadable {
			uroles = append(uroles, r)
		}
		sort.Strings(uroles)
		fmt.Fprintf(w, "\n# HELP herd_disk_volume_unreadable Volume probe failed on last scrape (fail-closed signal)\n")
		fmt.Fprintf(w, "# TYPE herd_disk_volume_unreadable gauge\n")
		for _, r := range uroles {
			v := 0
			if unreadable[r] {
				v = 1
			}
			fmt.Fprintf(w, "herd_disk_volume_unreadable{volume=%q} %d\n", r, v)
		}
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
		fmt.Fprintf(w, "herd_review_verdicts_total{verdict=\"fail\"} %d\n\n", atomic.LoadUint64(&m.TotalReviewFails))

		m.writeDiskMetrics(w)
	})
}
