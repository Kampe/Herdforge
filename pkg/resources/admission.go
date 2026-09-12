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

// Admission is the one decision about whether this host can accept new heavy
// work. cmd/herd's resources gate, wave, backfill and capacity all route
// through Decide; none keeps a second copy of the policy.
//
// Timing is explicit. Decide resolves both observations at the clock it is
// given and stores the results, so Verdict, Explain and NormalizedCPU are pure
// and cannot disagree with the decision by consulting a different wall clock.
// A decision is only valid for the freshness window of the readings behind it:
// Report revalidates at its own supplied instant, and a decision reused after
// its observations aged out reports a refusal rather than a stale ADMIT.
type Admission struct {
	Decision Decision `json:"decision"`

	// DecidedAt is the clock Decide was given. It is the only time this
	// decision is known to hold.
	DecidedAt time.Time `json:"decided_at"`

	// CPU and Memory are json:"-" because freshness.Reading keeps its value
	// private and defines no MarshalJSON: encoding them emits posture and drops
	// every number. Report is the wire shape.
	CPU    freshness.Reading[CPULoad]     `json:"-"`
	Memory freshness.Reading[MemHeadroom] `json:"-"`

	// Reasons are operator-facing and actionable. Empty on ADMIT.
	Reasons []string `json:"reasons,omitempty"`

	// Limits are echoed so a refusal can be argued with rather than guessed at.
	Limits Limits `json:"limits"`

	// Resolved observations, computed once at DecidedAt. Unexported so no
	// caller can construct an Admission that claims more than it measured.
	cpuUsable     bool
	cpuNormalized float64
	cpuLoad       CPULoad
	memUsable     bool
	mem           MemHeadroom
}

// Decision is two-valued. "Probably fine" is what the old TIGHT meant, and
// TIGHT admitted.
type Decision string

const (
	DecisionAdmit  Decision = "ADMIT"
	DecisionRefuse Decision = "REFUSE"
)

// CPULoad is normalized run-queue pressure. Raw loadavg is meaningless without
// the core count: this fleet runs on a Mac and a WSL box with different widths.
// Normalized is DERIVED by normalizedFrom, never trusted from a caller.
type CPULoad struct {
	Load1      float64 `json:"load1"`
	CPUs       int     `json:"cpus"`
	Normalized float64 `json:"normalized"`
}

// PressureLevel is the platform's own answer to "is memory hurting now", which
// is a different question from "how many pages are free". Darwin keeps free
// pages near zero by design (file cache and compressor count against them), so
// a percentage gate refuses healthy hosts; the kernel level does not.
type PressureLevel int

const (
	// PressureUnknown means the platform was not asked or did not answer.
	PressureUnknown PressureLevel = 0
	// PressureNormal is Darwin level 1 / Linux PSI below the stall threshold.
	PressureNormal PressureLevel = 1
	// PressureWarn is Darwin level 2: the kernel is asking for memory back.
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

// Known reports whether the level came from the platform rather than a zero
// value nobody set.
func (p PressureLevel) Known() bool {
	return p == PressureNormal || p == PressureWarn || p == PressureCritical
}

// Unsafe reports whether the kernel is signalling memory trouble.
func (p PressureLevel) Unsafe() bool {
	return p == PressureWarn || p == PressureCritical
}

// MemHeadroom is what the platform knows about memory, in the two forms that
// mean different things.
type MemHeadroom struct {
	// Pressure is the authoritative signal where the platform has one.
	Pressure PressureLevel `json:"pressure"`

	// FreePct is headroom as a percentage, or -1 where the platform has no
	// figure that means what a percentage implies.
	FreePct int `json:"free_pct"`

	// FreePctGates says whether FreePct may REFUSE. Linux MemAvailable is the
	// kernel's answer to what a new workload can get without swapping, so a
	// reserve against it is meaningful. Darwin's free percentage is a page
	// count, so it is carried for the report only.
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
	// CPURefuseLoad is normalized load (load1/cpus) at or above which heavy
	// work is refused.
	//
	// The default is 0.75, not 1.0: at 1.0 the run queue is already as long as
	// the machine is wide, so admitting there reserves nothing for the work
	// about to start. It is a chosen reserve, like MemReservePct, not a
	// measured failure threshold.
	CPURefuseLoad float64 `json:"cpu_refuse_load"`

	// MemReservePct is the OS reserve gating headroom must clear.
	MemReservePct int `json:"mem_reserve_pct"`

	// StaleAfter is how old an observation may be and still be acted on,
	// enforced for EVERY posture by checkReading.
	StaleAfter time.Duration `json:"stale_after"`

	// ProbeTimeout bounds each platform probe.
	ProbeTimeout time.Duration `json:"probe_timeout"`

	// ClockSkew is how far ahead an ObservedAt may sit before it is rejected.
	ClockSkew time.Duration `json:"clock_skew"`
}

const (
	defaultCPURefuseLoad = 0.75
	defaultMemReservePct = 15
	defaultStaleAfter    = 30 * time.Second
	defaultProbeTimeout  = 2 * time.Second
	defaultClockSkew     = 2 * time.Second
)

// DefaultLimits reads the operator overrides once. A malformed, non-finite,
// negative or out-of-range override falls back to the compiled default rather
// than disabling a limit: ParseFloat accepts "NaN" and "+Inf", either of which
// makes `normalized >= limit` false for every possible load.
func DefaultLimits() Limits {
	return Limits{
		CPURefuseLoad: envFloat("HERD_CPU_REFUSE_LOAD", defaultCPURefuseLoad),
		MemReservePct: envPct("HERD_MEM_RESERVE_PCT", defaultMemReservePct),
		StaleAfter:    defaultStaleAfter,
		ProbeTimeout:  defaultProbeTimeout,
		ClockSkew:     defaultClockSkew,
	}
}

// finite reports whether f may be compared against a threshold. NaN fails every
// comparison; infinities make a threshold meaningless.
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

// sane returns limits safe to decide against and says what it repaired. Decide
// is public, so a caller may pass a zero Limits or a NaN threshold; a
// non-positive StaleAfter in particular would switch the age check off.
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
// a Normalized value reaches a comparison. CPULoad is a public struct, so the
// field is caller data: a reading whose Normalized disagrees with its own Load1
// and CPUs is rejected rather than reconciled, because there is no way to know
// which field was wrong.
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
	if !finite(load.Normalized) {
		return 0, fmt.Errorf("normalized load %v is not a finite number", load.Normalized)
	}
	if math.Abs(load.Normalized-derived) > 1e-6 {
		return 0, fmt.Errorf(
			"reading is inconsistent: normalized %v does not match load1 %.2f over %d cpus (%.4f); refusing rather than picking one",
			load.Normalized, load.Load1, load.CPUs, derived)
	}
	return derived, nil
}

// checkReading enforces what pkg/freshness deliberately does not: Value()
// returns ok for the ZERO State, and StaleBeyond() returns false for a FRESH
// reading at any age and whenever the limit is non-positive. Tightened here
// rather than in the shared package other callers depend on.
//
// Enforced for every posture: a recognized state, a set ObservedAt, not ahead
// of now beyond the skew allowance, and not older than the window.
func checkReading(now time.Time, state freshness.State, observedAt time.Time, explain string, limits Limits, what string) string {
	switch state {
	case freshness.StateFresh, freshness.StateStale:
	case freshness.StateUnknown:
		return what + " is not known, which is NOT the same as it being healthy: " + explain
	default:
		return fmt.Sprintf("%s carries an unrecognized freshness state %q; an unset posture is not an observation", what, string(state))
	}
	if observedAt.IsZero() {
		return what + " carries no observation time, so its age cannot be established"
	}
	if observedAt.After(now.Add(limits.ClockSkew)) {
		return fmt.Sprintf("%s is timestamped %s in the future, beyond the %s skew allowance",
			what, observedAt.Sub(now).Round(time.Millisecond), limits.ClockSkew)
	}
	if age := now.Sub(observedAt); age > limits.StaleAfter {
		return fmt.Sprintf("%s was observed %s ago, beyond the %s window; a host can change completely in that time: %s",
			what, age.Round(time.Millisecond), limits.StaleAfter, explain)
	}
	return ""
}

// Decide is the pure core: observations and a clock in, decision out. No I/O
// and no clock of its own, so a fixture drives it deterministically.
//
// CPU and memory are evaluated INDEPENDENTLY and both reasons are reported: an
// operator who fixed only the one checked first would retry into the other.
func Decide(now time.Time, cpu freshness.Reading[CPULoad], mem freshness.Reading[MemHeadroom], limits Limits) Admission {
	limits, repaired := limits.sane()
	a := Admission{Decision: DecisionAdmit, DecidedAt: now, CPU: cpu, Memory: mem, Limits: limits}
	for _, r := range repaired {
		// A repaired configuration is reported, never silent. It does not
		// itself refuse: the repaired limit is the compiled default and the
		// observation is still judged against it.
		a.Reasons = append(a.Reasons, "configuration repaired: "+r)
	}
	configNotes := len(a.Reasons)

	if problem := checkReading(now, cpu.State, cpu.ObservedAt, cpu.MustExplain(now), limits, "CPU load"); problem != "" {
		a.refuse(problem)
	} else if load, ok := cpu.Value(); !ok {
		a.refuse("CPU load is not known: " + cpu.MustExplain(now))
	} else if normalized, err := normalizedFrom(load); err != nil {
		a.refuse("CPU load is not usable: " + err.Error())
	} else {
		a.cpuUsable, a.cpuNormalized, a.cpuLoad = true, normalized, load
		if normalized >= limits.CPURefuseLoad {
			a.refuse(fmt.Sprintf(
				"CPU is saturated: normalized load %.2f (load1 %.2f over %d cpus) is at or above the %.2f limit. "+
					"Let running work drain, or reduce concurrency, before starting more.",
				normalized, load.Load1, load.CPUs, limits.CPURefuseLoad))
		}
	}

	if problem := checkReading(now, mem.State, mem.ObservedAt, mem.MustExplain(now), limits, "memory headroom"); problem != "" {
		a.refuse(problem)
	} else if head, ok := mem.Value(); !ok {
		a.refuse("memory headroom is not known: " + mem.MustExplain(now))
	} else if !head.Usable() {
		a.refuse("memory was read but carries no signal allowed to decide: the platform reported neither a kernel pressure level nor a gating headroom figure")
	} else {
		a.memUsable, a.mem = true, head
		switch {
		case head.Pressure.Unsafe():
			a.refuse(fmt.Sprintf(
				"the kernel reports memory pressure %s (level %d). This is the platform's own signal, not a page count. "+
					"Let running work finish or reap idle lanes before starting more.",
				head.Pressure, int(head.Pressure)))
		case head.FreePctGates && (head.FreePct < 0 || head.FreePct > 100):
			a.refuse(fmt.Sprintf("memory headroom %d%% is outside 0-100 and cannot be believed; treating an impossible reading as unknown, not as room", head.FreePct))
		case head.FreePctGates && head.FreePct < limits.MemReservePct:
			a.refuse(fmt.Sprintf(
				"memory headroom %d%% is below the %d%% OS reserve. Heavy work started here competes with the system itself. "+
					"Free memory or reap idle lanes first.",
				head.FreePct, limits.MemReservePct))
		}
	}

	if len(a.Reasons) == configNotes {
		a.Decision = DecisionAdmit
	}
	return a
}

func (a *Admission) refuse(reason string) {
	a.Decision = DecisionRefuse
	a.Reasons = append(a.Reasons, reason)
}

// Admits reports whether heavy work may start, as judged at DecidedAt. A
// consumer acting later must call Report with its own clock first.
func (a Admission) Admits() bool { return a.Decision == DecisionAdmit }

// NormalizedCPU returns the derived normalized load resolved at DecidedAt, and
// whether it is usable. Pure: no clock of its own.
func (a Admission) NormalizedCPU() (float64, bool) {
	return a.cpuNormalized, a.cpuUsable
}

// Headroom returns the memory reading resolved at DecidedAt and whether it was
// usable. Pure.
func (a Admission) Headroom() (MemHeadroom, bool) {
	return a.mem, a.memUsable
}

// expiredAt reports whether the observations behind this decision have aged out
// by at, and says why. Empty means the decision still holds.
func (a Admission) expiredAt(at time.Time) []string {
	var stale []string
	if problem := checkReading(at, a.CPU.State, a.CPU.ObservedAt, a.CPU.MustExplain(at), a.Limits, "CPU load"); problem != "" && a.cpuUsable {
		stale = append(stale, problem)
	}
	if problem := checkReading(at, a.Memory.State, a.Memory.ObservedAt, a.Memory.MustExplain(at), a.Limits, "memory headroom"); problem != "" && a.memUsable {
		stale = append(stale, problem)
	}
	return stale
}

// RevalidateAt returns this decision as it stands at at. A decision whose
// observations aged out becomes a REFUSAL rather than a stale ADMIT: a
// consumer that held one for a minute is not entitled to the answer it got.
func (a Admission) RevalidateAt(at time.Time) Admission {
	stale := a.expiredAt(at)
	if len(stale) == 0 {
		return a
	}
	a.cpuUsable, a.memUsable = false, false
	for _, s := range stale {
		a.refuse("the decision is no longer current: " + s)
	}
	return a
}

// Explain renders the refusal, or the admission with the numbers it rests on.
// Pure, and never empty.
func (a Admission) Explain() string {
	if a.Admits() {
		cpu := "cpu load unavailable"
		if normalized, ok := a.NormalizedCPU(); ok {
			cpu = fmt.Sprintf("normalized cpu load %.2f (limit %.2f)", normalized, a.Limits.CPURefuseLoad)
		}
		mem := "memory unavailable"
		if head, ok := a.Headroom(); ok {
			mem = "kernel memory pressure " + head.Pressure.String()
			if head.FreePctGates {
				// Only quote the percentage where it was allowed to decide;
				// printing a non-gating free-page figure as "headroom" is how a
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

// AdmissionVerdict maps a decision onto the reporting vocabulary. Pure.
//
// TIGHT names a refusal backed by a measurement; ALERT names a refusal because
// nothing usable could be measured. Only OK admits; see GatePasses.
func AdmissionVerdict(a Admission) string {
	if a.Admits() {
		return VerdictOK
	}
	if !a.cpuUsable || !a.memUsable {
		return VerdictAlert
	}
	return VerdictTight
}

// Admit observes this host and decides AFTER the probes finish. The context
// bounds every probe and cancellation reaches the child process.
func Admit(ctx context.Context) Admission {
	limits := DefaultLimits()
	return admitFrom(limits, time.Now,
		func(at time.Time) freshness.Reading[CPULoad] { return observeCPU(ctx, at, limits) },
		func(at time.Time) freshness.Reading[MemHeadroom] { return observeMemory(ctx, at, limits) })
}

// admitFrom sequences observation and decision against an injectable clock.
//
// The clock is read three times on purpose. Each observation keeps its OWN
// timestamp, and the DECISION is taken after both probes return -- so the time
// the probes themselves spend is visible to the staleness check. Stamping
// everything with one pre-probe instant made a slow probe sequence free: a CPU
// sample taken before a long memory probe still looked current at the end of
// it, and the snapshot rendered at that same old instant admitted on it.
//
// A probe sequence longer than StaleAfter therefore refuses on its own first
// reading, which is the intended behaviour: if measuring took that long, the
// measurement is no longer about the host we are deciding for.
func admitFrom(limits Limits, clock func() time.Time,
	cpuProbe func(time.Time) freshness.Reading[CPULoad],
	memProbe func(time.Time) freshness.Reading[MemHeadroom]) Admission {
	cpu := cpuProbe(clock())
	mem := memProbe(clock())
	return Decide(clock(), cpu, mem, limits)
}

// ObserveCPU exposes this package's CPU observation to callers outside it, so
// no caller carries its own parsing, timeout or idea of "busy".
func ObserveCPU(ctx context.Context) freshness.Reading[CPULoad] {
	return observeCPU(ctx, time.Now(), DefaultLimits())
}

// unknownReading is the one shape for "this platform or probe told us nothing".
// It carries no value, so freshness.Value() returns ok=false.
func unknownReading[T any](source string, err error, recovery string) freshness.Reading[T] {
	return freshness.Degrade(freshness.Reading[T]{}, source, err, recovery)
}

// parseLoad1 reads the 1-minute load average from either platform's format:
// Darwin's "{ 1.23 1.45 1.67 }" and Linux's "1.23 1.45 1.67 2/345 6789".
//
// Missing, non-numeric, negative or NON-FINITE is an error, never zero:
// ParseFloat accepts "NaN" and "Inf", and a NaN load clears every threshold.
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

// cpuReadingFrom builds the CPU reading from a raw load string, so a fixture
// can drive it without a host.
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

// parseDarwinPressureLevel reads kern.memorystatus_vm_pressure_level's value.
//
// It lives here, unconstrained, rather than beside the Darwin probe, because it
// is a pure text parser with nothing platform-specific in it: running the
// sysctl is Darwin's job, interpreting its output is not. That split is what
// lets the vocabulary be tested on every platform CI runs on, which is the
// point — a parser only exercised on the maintainer's laptop is a parser whose
// regressions reach production.
//
// The vocabulary is fixed and closed: 1 normal, 2 warning, 4 critical, as the
// memorystatus subsystem defines them. Everything else — an unlisted level, a
// word, or silence — is an error and PressureUnknown. Silence in particular
// must never read as normal: that would turn a failed probe into an admission.
func parseDarwinPressureLevel(output string) (PressureLevel, error) {
	token := strings.TrimSpace(output)
	if token == "" {
		return PressureUnknown, fmt.Errorf("kernel pressure level was empty; silence is not a reading")
	}
	switch token {
	case "1":
		return PressureNormal, nil
	case "2":
		return PressureWarn, nil
	case "4":
		return PressureCritical, nil
	default:
		return PressureUnknown, fmt.Errorf("kernel pressure level %q is not one of 1, 2 or 4", token)
	}
}

// parseFreePctStrict is the truthful counterpart to parseMemoryPressureFreePct,
// which answers 100 to every malformed shape.
//
// It reads the WHOLE token after the label rather than scanning backwards for
// trailing digits, which silently rewrote "-50%" into 50 and "1.50%" into 50.
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
