package resources

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

// Admission is the ONE decision about whether this host can accept new heavy
// work right now. cmd/herd's resources gate, wave, backfill and capacity all
// route through Decide; none of them keeps a second copy of the policy.
//
// What was wrong before FAC-826, as plain implementation facts:
//
//   - gatherMetrics returned free_pct=100, swap_mb=0 on ANY probe failure, and
//     every parser returned 100 on malformed input. On Linux the Darwin probes
//     do not exist, so a Linux host reported 100% free from probes that never
//     ran.
//   - Verdict could only ever return OK or TIGHT and GatePasses accepted TIGHT,
//     so `herd resources --gate` could not exit nonzero and the ALERT arm of
//     pkg/backfill's gate was unreachable.
//   - cmd/herd/capacity.go admitted on unmeasurable memory by explicit doctrine
//     and carried no CPU signal.
//
// Those are the defects this package fixes. Whether any specific host failure
// coincided with a specific gate evaluation is NOT something we measured, and
// this file does not claim it.
type Admission struct {
	Decision Decision `json:"decision"`

	// CPU and Memory hold the readings the decision was made from. They are
	// json:"-" because freshness.Reading keeps its value PRIVATE and defines no
	// MarshalJSON, so encoding them emits posture metadata and drops every
	// number that mattered. Report() is the wire shape; see AdmissionReport.
	CPU    freshness.Reading[CPULoad]     `json:"-"`
	Memory freshness.Reading[MemHeadroom] `json:"-"`

	// Reasons are operator-facing and actionable: each says what was observed,
	// what the limit is, and what would clear it. Empty on ADMIT.
	Reasons []string `json:"reasons,omitempty"`

	// Limits are echoed so a refusal can be argued with rather than guessed at.
	Limits Limits `json:"limits"`
}

// Decision is deliberately two-valued. "Probably fine" is what the old TIGHT
// meant, and TIGHT admitted.
type Decision string

const (
	DecisionAdmit  Decision = "ADMIT"
	DecisionRefuse Decision = "REFUSE"
)

// CPULoad is normalized run-queue pressure. Raw loadavg is meaningless without
// the core count: 8.0 is unremarkable on a 32-core host and severe on a 2-core
// one, and this fleet runs on both a Mac and a WSL box.
//
// Normalized is DERIVED, never trusted from a caller. See normalizedFrom.
type CPULoad struct {
	Load1      float64 `json:"load1"`
	CPUs       int     `json:"cpus"`
	Normalized float64 `json:"normalized"`
}

// PressureLevel is the platform's OWN answer to "is memory hurting right now",
// which is a different question from "how many pages are free".
//
// The performance guard measured the difference on this host at
// 2026-09-12T18:26:31Z: 1993MiB unused with 10GiB held by the compressor and
// swap at 0, while kern.memorystatus_vm_pressure_level read 1 (normal).
// Roughly 4% free, and the kernel reports nothing wrong. A reserve applied to
// that percentage refuses a healthy host -- which is the FAC-693 shape, and
// what the first draft of this file did.
type PressureLevel int

const (
	// PressureUnknown means the platform was not asked or did not answer. Never
	// healthy.
	PressureUnknown PressureLevel = 0
	// PressureNormal is Darwin level 1 / Linux PSI below the stall threshold.
	PressureNormal PressureLevel = 1
	// PressureWarn is Darwin level 2: the kernel is asking processes to free
	// memory.
	PressureWarn PressureLevel = 2
	// PressureCritical is Darwin level 4.
	PressureCritical PressureLevel = 4
)

func (p PressureLevel) String() string {
	switch p {
	case PressureNormal:
		return "normal"
	case PressureWarn:
		return "warning"
	case PressureCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Known reports whether the level came from the platform rather than from a
// zero value nobody set.
func (p PressureLevel) Known() bool {
	return p == PressureNormal || p == PressureWarn || p == PressureCritical
}

// Unsafe reports whether the kernel itself is signalling memory trouble.
func (p PressureLevel) Unsafe() bool {
	return p == PressureWarn || p == PressureCritical
}

// MemHeadroom is what the platform knows about memory, in the two forms that
// mean different things.
type MemHeadroom struct {
	// Pressure is the authoritative signal where the platform has one.
	Pressure PressureLevel `json:"pressure"`

	// FreePct is headroom as a percentage, or -1 when the platform has no
	// figure that means what a percentage implies.
	FreePct int `json:"free_pct"`

	// FreePctGates says whether FreePct may REFUSE, and it is why two fields
	// exist. Linux MemAvailable is the kernel's answer to "how much can a new
	// workload get without swapping", so a reserve against it is meaningful and
	// this is true. Darwin's free percentage is a page count that sits near
	// zero by design because the file cache and the compressor count against
	// it, so this is false there and the figure is carried for the report only.
	FreePctGates bool `json:"free_pct_gates"`

	// SwapMB is informational on every platform and never decides (FAC-693).
	SwapMB    int  `json:"swap_mb"`
	SwapKnown bool `json:"swap_known"`
}

// Usable reports whether this reading carries at least one signal allowed to
// decide. A reading with neither is not an observation.
func (m MemHeadroom) Usable() bool {
	return m.Pressure.Known() || m.FreePctGates
}

// Limits are the thresholds a decision was made against.
type Limits struct {
	// CPURefuseLoad is normalized load (load1/cpus) at or above which heavy work
	// is refused.
	//
	// The default is 0.75, NOT 1.0. At 1.0 the run queue is already as long as
	// the machine is wide, so admitting there reserves nothing: the work we are
	// about to start has to contend with a machine that is already fully
	// subscribed, and so does everything interactive on it. 0.75 keeps a
	// quarter of the machine's width as headroom for the admitted work and for
	// whatever else the host has to keep doing. It is a reserve chosen for the
	// same reason MemReservePct exists, not a number derived from any measured
	// failure threshold, and this file makes no claim about what value would or
	// would not have prevented a particular host incident.
	CPURefuseLoad float64 `json:"cpu_refuse_load"`

	// MemReservePct is the OS reserve: gating headroom must be at least this
	// before heavy work is admitted.
	MemReservePct int `json:"mem_reserve_pct"`

	// StaleAfter is how old an observation may be and still be acted on. It is
	// enforced against ObservedAt for EVERY posture, not only STALE ones; see
	// checkReading.
	StaleAfter time.Duration `json:"stale_after"`

	// ProbeTimeout bounds each platform probe.
	ProbeTimeout time.Duration `json:"probe_timeout"`

	// ClockSkew is how far in the future an ObservedAt may sit before the
	// reading is rejected as untrustworthy rather than merely early.
	ClockSkew time.Duration `json:"clock_skew"`
}

const (
	defaultCPURefuseLoad = 0.75
	defaultMemReservePct = 15
	defaultStaleAfter    = 30 * time.Second
	defaultProbeTimeout  = 2 * time.Second
	defaultClockSkew     = 2 * time.Second
)

// DefaultLimits reads the operator overrides once.
//
// A malformed, non-finite, negative or out-of-range override falls back to the
// compiled default rather than disabling a limit. An unparsable threshold must
// not silently become "no threshold": strconv.ParseFloat accepts "NaN" and
// "+Inf", and either one turns `normalized >= limit` into a comparison that is
// false for every possible load.
func DefaultLimits() Limits {
	return Limits{
		CPURefuseLoad: envFloat("HERD_CPU_REFUSE_LOAD", defaultCPURefuseLoad),
		MemReservePct: envPct("HERD_MEM_RESERVE_PCT", defaultMemReservePct),
		StaleAfter:    defaultStaleAfter,
		ProbeTimeout:  defaultProbeTimeout,
		ClockSkew:     defaultClockSkew,
	}
}

// finite reports whether f is a real number we may compare against. NaN fails
// every comparison, and infinities make a threshold meaningless.
func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

func envFloat(key string, def float64) float64 {
	raw, set := os.LookupEnv(key)
	if !set {
		return def
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || !finite(v) || v <= 0 {
		return def
	}
	return v
}

func envPct(key string, def int) int {
	raw, set := os.LookupEnv(key)
	if !set {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v < 0 || v > 100 {
		return def
	}
	return v
}

// sane returns limits safe to decide against, and says what it had to repair.
//
// Decide is public, so its inputs are not all produced by DefaultLimits. A
// caller that passes a zero Limits, a NaN threshold or a non-positive
// StaleAfter must not thereby switch a check off -- a zero StaleAfter would
// disable the age check entirely, which is how an arbitrarily old reading
// becomes a current one.
func (l Limits) sane() (Limits, []string) {
	var repaired []string
	if !finite(l.CPURefuseLoad) || l.CPURefuseLoad <= 0 {
		repaired = append(repaired, fmt.Sprintf(
			"cpu limit %v is not a usable threshold; falling back to the compiled %.2f rather than comparing against a value no load can exceed",
			l.CPURefuseLoad, defaultCPURefuseLoad))
		l.CPURefuseLoad = defaultCPURefuseLoad
	}
	if l.MemReservePct < 0 || l.MemReservePct > 100 {
		repaired = append(repaired, fmt.Sprintf(
			"memory reserve %d%% is outside 0-100; falling back to the compiled %d%%", l.MemReservePct, defaultMemReservePct))
		l.MemReservePct = defaultMemReservePct
	}
	if l.StaleAfter <= 0 {
		repaired = append(repaired, fmt.Sprintf(
			"staleness window %s would disable the age check; falling back to the compiled %s", l.StaleAfter, defaultStaleAfter))
		l.StaleAfter = defaultStaleAfter
	}
	if l.ProbeTimeout <= 0 {
		l.ProbeTimeout = defaultProbeTimeout
	}
	if l.ClockSkew < 0 {
		l.ClockSkew = defaultClockSkew
	}
	return l, repaired
}

// normalizedFrom derives normalized load from the raw pair, and is the only way
// a Normalized value reaches a comparison.
//
// Decide is public and CPULoad is a plain struct, so Normalized is
// caller-supplied data, not a fact. A reading whose Normalized disagrees with
// its own Load1 and CPUs is inconsistent, and an inconsistent reading is not
// evidence -- it is rejected rather than reconciled, because there is no way to
// know which of the two fields was the lie.
func normalizedFrom(load CPULoad) (float64, error) {
	if !finite(load.Load1) {
		return 0, fmt.Errorf("load average %v is not a finite number", load.Load1)
	}
	if load.Load1 < 0 {
		return 0, fmt.Errorf("load average %.2f is negative", load.Load1)
	}
	if load.CPUs <= 0 {
		return 0, fmt.Errorf("cpu count %d is unusable, so load cannot be normalized", load.CPUs)
	}
	derived := load.Load1 / float64(load.CPUs)
	if !finite(derived) {
		return 0, fmt.Errorf("normalizing %.2f over %d cpus did not produce a finite number", load.Load1, load.CPUs)
	}
	if finite(load.Normalized) && math.Abs(load.Normalized-derived) > 1e-6 {
		return 0, fmt.Errorf(
			"reading is inconsistent: normalized %v does not match load1 %.2f over %d cpus (%.4f); refusing rather than picking one",
			load.Normalized, load.Load1, load.CPUs, derived)
	}
	if !finite(load.Normalized) {
		return 0, fmt.Errorf("normalized load %v is not a finite number", load.Normalized)
	}
	return derived, nil
}

// checkReading enforces what pkg/freshness deliberately does not.
//
// Reading.Value() returns ok=true for the ZERO State, so a zero-valued Reading
// yields a zero T that reads as a real observation. Reading.StaleBeyond returns
// false for StateFresh no matter how old ObservedAt is, and false whenever the
// limit is non-positive. Those are reasonable defaults for the package's other
// consumers, so this policy layer tightens them here rather than changing a
// shared API that other callers depend on.
//
// Enforced for EVERY posture: the state is one we recognize, it is not UNKNOWN,
// ObservedAt is set, it is not in the future beyond the skew allowance, and it
// is not older than the window.
func checkReading(now time.Time, state freshness.State, observedAt time.Time, explain string, limits Limits, what string) string {
	switch state {
	case freshness.StateFresh, freshness.StateStale:
	case freshness.StateUnknown:
		return what + " is not known, which is NOT the same as it being healthy: " + explain
	default:
		return fmt.Sprintf("%s carries an unrecognized freshness state %q; an unset posture is not an observation", what, string(state))
	}
	if observedAt.IsZero() {
		return what + " carries no observation time, so its age cannot be established; a reading that cannot be dated cannot be trusted"
	}
	if observedAt.After(now.Add(limits.ClockSkew)) {
		return fmt.Sprintf("%s is timestamped %s in the future, beyond the %s skew allowance; refusing rather than trusting a clock we cannot explain",
			what, observedAt.Sub(now).Round(time.Millisecond), limits.ClockSkew)
	}
	if age := now.Sub(observedAt); age > limits.StaleAfter {
		return fmt.Sprintf("%s was observed %s ago, beyond the %s window; a host can change completely in that time: %s",
			what, age.Round(time.Millisecond), limits.StaleAfter, explain)
	}
	return ""
}

// Decide is the pure core: observations in, decision out, no I/O and no clock
// of its own. Every fixture drives this directly.
//
// CPU and memory are evaluated INDEPENDENTLY and both reasons are reported: an
// operator who fixed only the one checked first would otherwise retry straight
// into the other.
func Decide(now time.Time, cpu freshness.Reading[CPULoad], mem freshness.Reading[MemHeadroom], limits Limits) Admission {
	limits, repaired := limits.sane()
	a := Admission{Decision: DecisionAdmit, CPU: cpu, Memory: mem, Limits: limits}
	for _, r := range repaired {
		// A configuration we had to repair is reported, never silent. It does
		// not by itself refuse: the repaired limit is the compiled default, and
		// the observation is still judged against it.
		a.Reasons = append(a.Reasons, "configuration repaired: "+r)
	}
	configRepairs := len(a.Reasons)

	if problem := checkReading(now, cpu.State, cpu.ObservedAt, cpu.MustExplain(now), limits, "CPU load"); problem != "" {
		a.refuse(problem)
	} else if load, ok := cpu.Value(); !ok {
		a.refuse("CPU load is not known: " + cpu.MustExplain(now))
	} else if normalized, err := normalizedFrom(load); err != nil {
		a.refuse("CPU load is not usable: " + err.Error())
	} else if normalized >= limits.CPURefuseLoad {
		a.refuse(fmt.Sprintf(
			"CPU is saturated: normalized load %.2f (load1 %.2f over %d cpus) is at or above the %.2f limit. "+
				"Let running work drain, or reduce concurrency, before starting more.",
			normalized, load.Load1, load.CPUs, limits.CPURefuseLoad))
	}

	if problem := checkReading(now, mem.State, mem.ObservedAt, mem.MustExplain(now), limits, "memory headroom"); problem != "" {
		a.refuse(problem)
	} else if head, ok := mem.Value(); !ok {
		a.refuse("memory headroom is not known: " + mem.MustExplain(now))
	} else if !head.Usable() {
		a.refuse("memory was read but carries no signal allowed to decide: the platform reported neither a kernel pressure level nor a gating headroom figure")
	} else if head.Pressure.Unsafe() {
		a.refuse(fmt.Sprintf(
			"the kernel reports memory pressure %s (level %d). This is the platform's own signal, not a page count. "+
				"Let running work finish or reap idle lanes before starting more.",
			head.Pressure, int(head.Pressure)))
	} else if head.FreePctGates && (head.FreePct < 0 || head.FreePct > 100) {
		a.refuse(fmt.Sprintf("memory headroom %d%% is outside 0-100 and cannot be believed; treating an impossible reading as unknown, not as room", head.FreePct))
	} else if head.FreePctGates && head.FreePct < limits.MemReservePct {
		a.refuse(fmt.Sprintf(
			"memory headroom %d%% is below the %d%% OS reserve. Heavy work started here competes with the system itself. "+
				"Free memory or reap idle lanes first.",
			head.FreePct, limits.MemReservePct))
	}

	if len(a.Reasons) == configRepairs {
		// Only configuration notes were recorded; nothing refused.
		a.Decision = DecisionAdmit
	}
	return a
}

func (a *Admission) refuse(reason string) {
	a.Decision = DecisionRefuse
	a.Reasons = append(a.Reasons, reason)
}

// Admits reports whether heavy work may start.
func (a Admission) Admits() bool { return a.Decision == DecisionAdmit }

// NormalizedCPU returns the derived normalized load and whether it is usable.
// Consumers that report the number must go through here rather than reading
// CPULoad.Normalized, which is caller-supplied until it is validated.
func (a Admission) NormalizedCPU() (float64, bool) {
	if problem := checkReading(time.Now(), a.CPU.State, a.CPU.ObservedAt, "", a.Limits, "cpu"); problem != "" {
		return 0, false
	}
	load, ok := a.CPU.Value()
	if !ok {
		return 0, false
	}
	normalized, err := normalizedFrom(load)
	if err != nil {
		return 0, false
	}
	return normalized, true
}

// Explain renders the refusal for an operator, or states the admission with the
// numbers it rests on. Never empty, so no caller can print a blank refusal.
func (a Admission) Explain() string {
	if a.Admits() {
		cpu := "cpu load unavailable"
		if normalized, ok := a.NormalizedCPU(); ok {
			cpu = fmt.Sprintf("normalized cpu load %.2f (limit %.2f)", normalized, a.Limits.CPURefuseLoad)
		}
		mem := "memory unavailable"
		if head, ok := a.Memory.Value(); ok {
			mem = "kernel memory pressure " + head.Pressure.String()
			if head.FreePctGates {
				// Only quote the percentage where it was allowed to decide.
				// Printing a non-gating free-page figure as "headroom" is how a
				// healthy host at 4% free reads as an emergency.
				mem += fmt.Sprintf(", headroom %d%% (reserve %d%%)", head.FreePct, a.Limits.MemReservePct)
			} else if head.FreePct >= 0 {
				mem += fmt.Sprintf(" (free %d%%, informational on this platform)", head.FreePct)
			}
		}
		return "admitted: " + cpu + ", " + mem
	}
	out := "refusing heavy work:"
	for _, r := range a.Reasons {
		out += "\n  - " + r
	}
	return out
}

// AdmissionVerdict maps a decision onto this package's reporting vocabulary so
// `herd resources` keeps its shape.
//
// The mapping CHANGES what TIGHT means. It used to be a warning that still
// admitted; it now names a refusal backed by a measurement, while ALERT names a
// refusal because nothing usable could be measured. Not being able to tell is
// the more serious of the two, because that is the state that used to render as
// 100% free and OK.
func AdmissionVerdict(a Admission) string {
	if a.Admits() {
		return VerdictOK
	}
	if _, ok := a.NormalizedCPU(); !ok {
		return VerdictAlert
	}
	if head, ok := a.Memory.Value(); !ok || !head.Usable() {
		return VerdictAlert
	}
	return VerdictTight
}

// Admit observes this host and decides. The context bounds every probe: a hung
// probe can neither run unbounded nor be abandoned, and cancellation reaches the
// child process. It adds no host-wide scan of its own.
func Admit(ctx context.Context) Admission {
	limits := DefaultLimits()
	now := time.Now()
	return Decide(now, observeCPU(ctx, now, limits), observeMemory(ctx, now, limits), limits)
}

// ObserveCPU exposes this package's CPU observation to callers outside it.
//
// FAC-826 asked for one policy, not one policy plus a second opinion: a caller
// that probed load itself would carry its own parsing, its own timeout and its
// own idea of what "busy" means, and the copies would drift.
func ObserveCPU(ctx context.Context) freshness.Reading[CPULoad] {
	return observeCPU(ctx, time.Now(), DefaultLimits())
}

// unknownReading is the single shape for "this platform or probe told us
// nothing". It carries no value, so freshness.Value() returns ok=false.
func unknownReading[T any](source string, err error, recovery string) freshness.Reading[T] {
	return freshness.Degrade(freshness.Reading[T]{}, source, err, recovery)
}

// parseLoad1 reads the 1-minute load average from either platform's format:
// Darwin's `sysctl -n vm.loadavg` prints "{ 1.23 1.45 1.67 }" and Linux's
// /proc/loadavg prints "1.23 1.45 1.67 2/345 6789". One parser, because two
// copies of "how do we read load" is how the copies come to disagree.
//
// A missing, non-numeric, negative or NON-FINITE field is an ERROR, never zero.
// strconv.ParseFloat accepts "NaN", "Inf" and "+Inf"; NaN in particular is
// false in every comparison, so a NaN load would clear any threshold.
func parseLoad1(out string) (float64, error) {
	fields := strings.Fields(strings.NewReplacer("{", " ", "}", " ").Replace(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("load average output had no fields")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("load average %q is not a number: %w", fields[0], err)
	}
	if !finite(v) {
		return 0, fmt.Errorf("load average %q is not finite; a value no threshold can exceed is not a measurement", fields[0])
	}
	if v < 0 {
		return 0, fmt.Errorf("load average %.2f is negative", v)
	}
	return v, nil
}

// cpuReadingFrom builds the CPU reading from a raw load string. Split out so a
// fixture can drive it without a host.
func cpuReadingFrom(source string, at time.Time, out string, cpus int) freshness.Reading[CPULoad] {
	load1, err := parseLoad1(out)
	if err != nil {
		return unknownReading[CPULoad](source, err, "check that the load-average probe for this platform is available")
	}
	if cpus <= 0 {
		return unknownReading[CPULoad](source, fmt.Errorf("runtime reported %d cpus", cpus), "cpu count is required to normalize load")
	}
	return freshness.Fresh(source, at, CPULoad{Load1: load1, CPUs: cpus, Normalized: load1 / float64(cpus)})
}

// parseFreePctStrict is the truthful counterpart to parseMemoryPressureFreePct,
// which answers 100 to every malformed shape.
//
// It reads the WHOLE token before the percent sign rather than scanning
// backwards for trailing digits. Scanning backwards silently rewrites input
// into a healthy-looking number: "-50%" yields 50, and "1.50%" yields 50. A
// signed, fractional or junk-bearing token is an error here.
func parseFreePctStrict(output, label string) (int, error) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(strings.ToLower(line), strings.ToLower(label)) {
			continue
		}
		percent := strings.IndexByte(line, '%')
		if percent < 0 {
			return 0, fmt.Errorf("%q line carried no percent sign: %q", label, line)
		}
		prefix := strings.TrimSpace(line[:percent])
		idx := strings.LastIndex(strings.ToLower(prefix), strings.ToLower(label))
		if idx < 0 {
			return 0, fmt.Errorf("%q line had its percentage before the label: %q", label, line)
		}
		token := strings.TrimSpace(prefix[idx+len(label):])
		if token == "" {
			return 0, fmt.Errorf("%q line carried no value before the percent sign: %q", label, line)
		}
		if strings.ContainsAny(token, "+-.,eExX") || strings.Fields(token)[0] != token {
			return 0, fmt.Errorf("%q value %q is not a plain whole percentage; refusing rather than reinterpreting it", label, token)
		}
		n, err := strconv.Atoi(token)
		if err != nil {
			return 0, fmt.Errorf("%q percentage %q is not a number: %w", label, token, err)
		}
		if n < 0 || n > 100 {
			return 0, fmt.Errorf("%q percentage %d is outside 0-100", label, n)
		}
		return n, nil
	}
	return 0, fmt.Errorf("probe output carried no %q line", label)
}
