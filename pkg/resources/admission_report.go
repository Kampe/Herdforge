package resources

import (
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

// AdmissionReport is the public wire shape of a decision.
//
// It exists because freshness.Reading keeps its value private and defines no
// MarshalJSON: encoding an Admission emits state, source and timestamp and
// drops the load average, cpu count, pressure level and headroom percentage --
// every number the decision rested on.
//
// Every observed number is a POINTER: an unknown reading OMITS the field rather
// than publishing a zero, because a zero load and a zero percentage both look
// healthy.
type AdmissionReport struct {
	Decision string `json:"decision"`
	Admits   bool   `json:"admits"`
	Verdict  string `json:"verdict"`
	// DecidedAt is when the decision was made; RenderedAt is the clock it was
	// revalidated against. They differ whenever a held decision is reused.
	DecidedAt   string       `json:"decided_at,omitempty"`
	RenderedAt  string       `json:"rendered_at,omitempty"`
	CPU         CPUReport    `json:"cpu"`
	Memory      MemoryReport `json:"memory"`
	Reasons     []string     `json:"reasons,omitempty"`
	Explanation string       `json:"explanation"`
	Limits      Limits       `json:"limits"`
}

// ReadingReport is the posture half, shared by both observations.
type ReadingReport struct {
	State string `json:"state"`
	// Known is false whenever the values below are absent, so a consumer can
	// branch on one boolean instead of testing every pointer.
	Known      bool   `json:"known"`
	ObservedAt string `json:"observed_at,omitempty"`
	AgeMS      *int64 `json:"age_ms,omitempty"`
	Source     string `json:"source,omitempty"`
	Error      string `json:"error,omitempty"`
	Recovery   string `json:"recovery,omitempty"`
}

// CPUReport carries the observed load. Normalized is the DERIVED value the
// decision used, not the caller-supplied field.
type CPUReport struct {
	ReadingReport
	Load1      *float64 `json:"load1,omitempty"`
	CPUs       *int     `json:"cpus,omitempty"`
	Normalized *float64 `json:"normalized,omitempty"`
}

// MemoryReport carries the pressure level that decided plus the headroom
// figure, with free_pct_gates saying whether that figure could refuse.
type MemoryReport struct {
	ReadingReport
	Pressure      string `json:"pressure"`
	PressureKnown bool   `json:"pressure_known"`
	FreePct       *int   `json:"free_pct,omitempty"`
	FreePctGates  bool   `json:"free_pct_gates"`
	SwapMB        *int   `json:"swap_mb,omitempty"`
}

// Report renders the decision AS IT STANDS AT at.
//
// It revalidates first: a decision whose observations aged out renders as a
// refusal with its reasons, never as a stale ADMIT with no observations behind
// it. Values are published only when the revalidated decision still holds them.
func (a Admission) Report(at time.Time) AdmissionReport {
	current := a.RevalidateAt(at)
	r := AdmissionReport{
		Decision:    string(current.Decision),
		Admits:      current.Admits(),
		Verdict:     AdmissionVerdict(current),
		RenderedAt:  at.UTC().Format(time.RFC3339Nano),
		Reasons:     current.Reasons,
		Explanation: current.Explain(),
		Limits:      current.Limits,
		CPU:         CPUReport{ReadingReport: readingReport(at, current.CPU.State, current.CPU.ObservedAt, current.CPU.Source, current.CPU.Err, current.CPU.Recovery)},
		Memory: MemoryReport{
			ReadingReport: readingReport(at, current.Memory.State, current.Memory.ObservedAt, current.Memory.Source, current.Memory.Err, current.Memory.Recovery),
			Pressure:      PressureUnknown.String(),
		},
	}
	if !a.DecidedAt.IsZero() {
		r.DecidedAt = a.DecidedAt.UTC().Format(time.RFC3339Nano)
	}

	if normalized, ok := current.NormalizedCPU(); ok {
		load1, cpus, norm := current.cpuLoad.Load1, current.cpuLoad.CPUs, normalized
		r.CPU.Known = true
		r.CPU.Load1, r.CPU.CPUs, r.CPU.Normalized = &load1, &cpus, &norm
	}
	if head, ok := current.Headroom(); ok {
		r.Memory.Known = true
		r.Memory.Pressure = head.Pressure.String()
		r.Memory.PressureKnown = head.Pressure.Known()
		r.Memory.FreePctGates = head.FreePctGates
		// A negative FreePct is this package's "not measured" sentinel and must
		// not be published as a number.
		if head.FreePct >= 0 {
			pct := head.FreePct
			r.Memory.FreePct = &pct
		}
		if head.SwapKnown {
			swap := head.SwapMB
			r.Memory.SwapMB = &swap
		}
	}
	return r
}

func readingReport(now time.Time, state freshness.State, observedAt time.Time, source, errMsg, recovery string) ReadingReport {
	out := ReadingReport{State: string(state), Source: source, Error: errMsg, Recovery: recovery}
	if out.State == "" {
		// A zero State is not a posture. Say so rather than emitting "".
		out.State = string(freshness.StateUnknown)
		if out.Error == "" {
			out.Error = "reading carried no freshness state"
		}
	}
	if !observedAt.IsZero() {
		out.ObservedAt = observedAt.UTC().Format(time.RFC3339Nano)
		age := now.Sub(observedAt).Milliseconds()
		out.AgeMS = &age
	}
	return out
}
