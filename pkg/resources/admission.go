package resources

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

// Admission is the ONE decision about whether this host can accept new heavy
// work right now.
//
// FAC-826. The three authorities that existed before this all answered
// "healthy" when they could not measure:
//
//   - gatherMetrics returned free_pct=100, swap_mb=0 on ANY probe failure, and
//     every parser in this file returns 100 on malformed input. On Linux the
//     Darwin probes do not exist at all, so a Linux host reported 100% free.
//   - Verdict could only ever return OK or TIGHT, and GatePasses accepted
//     TIGHT, so `herd resources --gate` could not exit nonzero and the ALERT
//     arm of the backfill gate was unreachable. The gate was dead, not lenient.
//   - cmd/herd/capacity.go admitted on unmeasurable memory by explicit
//     doctrine and carried no CPU signal whatsoever.
//
// The operator's Mac took repeated WindowServer watchdog crashes while all
// three reported a healthy host. An absence of measurement is not headroom.
//
// So: the value and its posture travel together (pkg/freshness), UNKNOWN and
// STALE refuse, CPU and memory refuse INDEPENDENTLY with their own actionable
// reason, and there is exactly one decision function that every heavy caller
// routes through.
type Admission struct {
	Decision Decision                      `json:"decision"`
	CPU      freshness.Reading[CPULoad]    `json:"cpu"`
	Memory   freshness.Reading[MemHeadroom] `json:"memory"`
	// Reasons are operator-facing and actionable: each one says what was
	// observed, what the limit is, and what would clear it. Empty on ADMIT.
	Reasons []string `json:"reasons,omitempty"`
	// Limits are echoed so a refusal can be argued with rather than guessed at.
	Limits Limits `json:"limits"`
}

// Decision is deliberately two-valued. "Probably fine" is what the old TIGHT
// meant, and it is what admitted the work that crashed the host.
type Decision string

const (
	DecisionAdmit  Decision = "ADMIT"
	DecisionRefuse Decision = "REFUSE"
)

// CPULoad is normalized run-queue pressure. Raw loadavg is meaningless without
// the core count: 8.0 is idle on a 16-core host and catastrophic on a 2-core
// one, and this fleet runs on both a Mac and a WSL box.
type CPULoad struct {
	Load1      float64 `json:"load1"`
	CPUs       int     `json:"cpus"`
	Normalized float64 `json:"normalized"`
}

// MemHeadroom is free headroom as a percentage, from the platform's TRUTHFUL
// signal.
//
// On Darwin that is the kernel's own memory-pressure figure, NOT "Pages free":
// macOS keeps free pages near zero by design because the file cache counts
// against them, so a low pagesfree is normal steady state and not danger. That
// lesson is already recorded at cmd/herd/capacity.go (Darwin pagesfree sits
// ~87% while the host is completely healthy) and FAC-693 (a sticky swap scar
// read as a wound refused every launch on a fine host). Neither is re-broken
// here: swap is carried for the report and never decides.
type MemHeadroom struct {
	FreePct   int  `json:"free_pct"`
	SwapMB    int  `json:"swap_mb"`
	SwapKnown bool `json:"swap_known"`
}

// Limits are the thresholds a decision was made against.
type Limits struct {
	// CPURefuseLoad is normalized load (load1/cpus) at or above which heavy
	// work is refused. 1.0 means "the run queue is already as long as the
	// machine is wide"; anything at or past that is contention, and heavy work
	// added there is what starves a UI compositor into a watchdog reset.
	CPURefuseLoad float64 `json:"cpu_refuse_load"`
	// MemReservePct is the OS reserve: free headroom must exceed this before
	// heavy work is admitted. This is the "explicit OS reserve" the card asks
	// for -- healthy means room left over for the system, not merely nonzero.
	MemReservePct int `json:"mem_reserve_pct"`
	// StaleAfter is how old a held observation may be and still be acted on.
	StaleAfter time.Duration `json:"stale_after"`
	// ProbeTimeout bounds each platform probe.
	ProbeTimeout time.Duration `json:"probe_timeout"`
}

const (
	defaultCPURefuseLoad = 1.0
	defaultMemReservePct = 15
	defaultStaleAfter    = 30 * time.Second
	defaultProbeTimeout  = 2 * time.Second
)

// DefaultLimits reads the operator overrides once. A malformed or negative
// override falls back to the compiled default rather than disabling a limit:
// an unparsable threshold must not silently become "no threshold".
func DefaultLimits() Limits {
	return Limits{
		CPURefuseLoad: envFloat("HERD_CPU_REFUSE_LOAD", defaultCPURefuseLoad),
		MemReservePct: envPct("HERD_MEM_RESERVE_PCT", defaultMemReservePct),
		StaleAfter:    defaultStaleAfter,
		ProbeTimeout:  defaultProbeTimeout,
	}
}

func envFloat(key string, def float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(key), 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func envPct(key string, def int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v < 0 || v > 100 {
		return def
	}
	return v
}

// Decide is the pure core: observations in, decision out, no I/O and no clock
// of its own. Every fixture drives this directly, so the policy is testable
// without a host to measure.
//
// CPU and memory are evaluated INDEPENDENTLY and both reasons are reported: an
// operator who fixes only the one that happened to be checked first would
// otherwise retry straight into the other.
func Decide(now time.Time, cpu freshness.Reading[CPULoad], mem freshness.Reading[MemHeadroom], limits Limits) Admission {
	a := Admission{Decision: DecisionAdmit, CPU: cpu, Memory: mem, Limits: limits}

	if load, ok := cpu.Value(); !ok {
		a.refuse("CPU load is not known, which is NOT the same as the CPU being idle: " + cpu.MustExplain(now))
	} else if cpu.StaleBeyond(now, limits.StaleAfter) {
		a.refuse("CPU load is stale beyond " + limits.StaleAfter.String() + " and a stale reading cannot clear a live host: " + cpu.MustExplain(now))
	} else if load.CPUs <= 0 {
		a.refuse("CPU count is unusable (cpus=" + strconv.Itoa(load.CPUs) + "), so load cannot be normalized; refusing rather than dividing by an assumed width")
	} else if load.Normalized >= limits.CPURefuseLoad {
		a.refuse(fmt.Sprintf(
			"CPU is saturated: normalized load %.2f (load1 %.2f over %d cpus) is at or above the %.2f limit. "+
				"Let running work drain, or reduce concurrency, before starting more.",
			load.Normalized, load.Load1, load.CPUs, limits.CPURefuseLoad))
	}

	if head, ok := mem.Value(); !ok {
		a.refuse("memory headroom is not known, which is NOT the same as memory being free: " + mem.MustExplain(now))
	} else if mem.StaleBeyond(now, limits.StaleAfter) {
		a.refuse("memory headroom is stale beyond " + limits.StaleAfter.String() + "; refusing on history rather than admitting on it: " + mem.MustExplain(now))
	} else if head.FreePct < 0 || head.FreePct > 100 {
		a.refuse(fmt.Sprintf("memory headroom %d%% is outside 0-100 and cannot be believed; treating an impossible reading as unknown, not as room", head.FreePct))
	} else if head.FreePct < limits.MemReservePct {
		a.refuse(fmt.Sprintf(
			"memory headroom %d%% is below the %d%% OS reserve. Heavy work started here competes with the system itself. "+
				"Free memory or reap idle lanes first.",
			head.FreePct, limits.MemReservePct))
	}

	return a
}

func (a *Admission) refuse(reason string) {
	a.Decision = DecisionRefuse
	a.Reasons = append(a.Reasons, reason)
}

// Admits reports whether heavy work may start.
func (a Admission) Admits() bool { return a.Decision == DecisionAdmit }

// Explain renders the refusal for an operator, or states the admission with
// the numbers it rests on. Never empty, so no caller can print a blank refusal.
func (a Admission) Explain() string {
	if a.Admits() {
		load, _ := a.CPU.Value()
		head, _ := a.Memory.Value()
		return fmt.Sprintf("admitted: normalized cpu load %.2f (limit %.2f), memory headroom %d%% (reserve %d%%)",
			load.Normalized, a.Limits.CPURefuseLoad, head.FreePct, a.Limits.MemReservePct)
	}
	out := "refusing heavy work:"
	for _, r := range a.Reasons {
		out += "\n  - " + r
	}
	return out
}

// AdmissionVerdict maps a decision onto this package's existing reporting
// vocabulary so `herd resources` keeps its shape.
//
// The mapping is deliberate and it CHANGES what TIGHT means. It used to be a
// warning that still admitted; it now names a refusal backed by a measurement,
// while ALERT names a refusal because nothing could be measured. Not being able
// to tell is the more serious of the two, because that is precisely the state
// that used to render as 100% free and OK.
func AdmissionVerdict(a Admission) string {
	if a.Admits() {
		return VerdictOK
	}
	if _, cpuOK := a.CPU.Value(); !cpuOK {
		return VerdictAlert
	}
	if _, memOK := a.Memory.Value(); !memOK {
		return VerdictAlert
	}
	return VerdictTight
}

// Admit observes this host and decides. The context bounds every probe: a hung
// probe can neither run unbounded nor be abandoned, and cancellation reaches
// the child process rather than leaking it.
//
// It adds no host-wide scan of its own -- one loadavg read and one memory
// reading, both already cheap on every supported platform.
func Admit(ctx context.Context) Admission {
	limits := DefaultLimits()
	now := time.Now()
	return Decide(now, observeCPU(ctx, now, limits), observeMemory(ctx, now, limits), limits)
}

// unknownReading is the single shape for "this platform or probe told us
// nothing". It never carries a value, so freshness.Value() returns ok=false and
// a consumer cannot read a plausible zero out of it.
func unknownReading[T any](source string, err error, recovery string) freshness.Reading[T] {
	return freshness.Degrade(freshness.Reading[T]{}, source, err, recovery)
}

// parseLoad1 reads the 1-minute load average from either platform's format:
// Darwin's `sysctl -n vm.loadavg` prints "{ 1.23 1.45 1.67 }" and Linux's
// /proc/loadavg prints "1.23 1.45 1.67 2/345 6789". One parser, because two
// copies of "how do we read load" is how the copies come to disagree.
//
// A field that is missing or not a number is an ERROR, never zero: zero load is
// the healthiest possible reading and is exactly what a malformed probe must
// not be able to claim.
func parseLoad1(out string) (float64, error) {
	fields := strings.Fields(strings.NewReplacer("{", " ", "}", " ").Replace(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("load average output had no fields")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("load average %q is not a number: %w", fields[0], err)
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

// parseFreePctStrict is the truthful counterpart to parseMemoryPressureFreePct.
//
// The legacy parser returns 100 for every malformed shape -- no matching line,
// no percent sign, no digits, an out-of-range number. That is the fail-open the
// operator's crashes were reported through, so admission does not use it. Here
// every one of those shapes is an error and an error refuses.
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
		start := len(prefix)
		for start > 0 && prefix[start-1] >= '0' && prefix[start-1] <= '9' {
			start--
		}
		if start == len(prefix) {
			return 0, fmt.Errorf("%q line carried no digits before the percent sign: %q", label, line)
		}
		n, err := strconv.Atoi(prefix[start:])
		if err != nil {
			return 0, fmt.Errorf("%q percentage %q is not a number: %w", label, prefix[start:], err)
		}
		if n < 0 || n > 100 {
			return 0, fmt.Errorf("%q percentage %d is outside 0-100", label, n)
		}
		return n, nil
	}
	return 0, fmt.Errorf("probe output carried no %q line", label)
}

// ObserveCPU exposes this package's CPU observation to heavy-admission callers
// outside it (cmd/herd/capacity.go).
//
// FAC-826 asked for ONE policy, not one policy plus a second opinion: a caller
// that probed load itself would carry its own parsing, its own timeout and its
// own idea of what "busy" means, and the copies would drift exactly the way the
// three memory authorities drifted before this card.
func ObserveCPU(ctx context.Context) freshness.Reading[CPULoad] {
	return observeCPU(ctx, time.Now(), DefaultLimits())
}
