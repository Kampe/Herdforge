package resources

import (
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

// AdmissionReport is the PUBLIC wire shape of a decision.
//
// It exists because freshness.Reading keeps its value in a private field and
// defines no MarshalJSON: encoding an Admission directly emits state, source
// and timestamp and silently drops the load average, the cpu count, the
// pressure level and the headroom percentage -- every number the safety
// decision was actually made from. A consumer reading that JSON would see prose
// and posture and no evidence.
//
// Every observed number is a POINTER. An unknown reading omits the field
// instead of emitting a zero, because a zero load and a zero percentage are
// both plausible healthy-looking values and this package exists to stop
// absences from reading as measurements.
type AdmissionReport struct {
	Decision string       `json:"decision"`
	Admits   bool         `json:"admits"`
	Verdict  string       `json:"verdict"`
	CPU      CPUReport    `json:"cpu"`
	Memory   MemoryReport `json:"memory"`
	Reasons  []string     `json:"reasons,omitempty"`
	// Explanation is the operator sentence, carried so a JSON consumer and a
	// terminal reader see the same thing.
	Explanation string `json:"explanation"`
	Limits      Limits `json:"limits"`
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

// CPUReport carries the observed load numbers. Normalized is the DERIVED value
// the decision used, not the caller-supplied field: an inconsistent reading
// reports no number at all rather than publishing the one that was not checked.
type CPUReport struct {
	ReadingReport
	Load1      *float64 `json:"load1,omitempty"`
	CPUs       *int     `json:"cpus,omitempty"`
	Normalized *float64 `json:"normalized,omitempty"`
}

// MemoryReport carries the pressure level that decided plus the headroom
// figure, with free_pct_gates saying whether that figure was allowed to refuse.
type MemoryReport struct {
	ReadingReport
	Pressure      string `json:"pressure"`
	PressureKnown bool   `json:"pressure_known"`
	FreePct       *int   `json:"free_pct,omitempty"`
	FreePctGates  bool   `json:"free_pct_gates"`
	SwapMB        *int   `json:"swap_mb,omitempty"`
}

// Report renders the decision for JSON consumers.
func (a Admission) Report(now time.Time) AdmissionReport {
	r := AdmissionReport{
		Decision:    string(a.Decision),
		Admits:      a.Admits(),
		Verdict:     AdmissionVerdict(a),
		Reasons:     a.Reasons,
		Explanation: a.Explain(),
		Limits:      a.Limits,
		CPU:         CPUReport{ReadingReport: readingReport(now, a.CPU.State, a.CPU.ObservedAt, a.CPU.Source, a.CPU.Err, a.CPU.Recovery)},
		Memory:      MemoryReport{ReadingReport: readingReport(now, a.Memory.State, a.Memory.ObservedAt, a.Memory.Source, a.Memory.Err, a.Memory.Recovery), Pressure: PressureUnknown.String()},
	}

	// The values are published only when the same checks the DECISION used say
	// the reading is usable. Reporting numbers the decision refused to trust
	// would hand a consumer a healthy-looking row from a rejected observation.
	if problem := checkReading(now, a.CPU.State, a.CPU.ObservedAt, "", a.Limits, "cpu"); problem == "" {
		if load, ok := a.CPU.Value(); ok {
			if normalized, err := normalizedFrom(load); err == nil {
				load1, cpus, norm := load.Load1, load.CPUs, normalized
				r.CPU.Known = true
				r.CPU.Load1, r.CPU.CPUs, r.CPU.Normalized = &load1, &cpus, &norm
			}
		}
	}

	if problem := checkReading(now, a.Memory.State, a.Memory.ObservedAt, "", a.Limits, "memory"); problem == "" {
		if head, ok := a.Memory.Value(); ok && head.Usable() {
			r.Memory.Known = true
			r.Memory.Pressure = head.Pressure.String()
			r.Memory.PressureKnown = head.Pressure.Known()
			r.Memory.FreePctGates = head.FreePctGates
			// A negative FreePct is this package's "not measured" sentinel and
			// must not be published as a number.
			if head.FreePct >= 0 {
				pct := head.FreePct
				r.Memory.FreePct = &pct
			}
			if head.SwapKnown {
				swap := head.SwapMB
				r.Memory.SwapMB = &swap
			}
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
