// Package resources provides a one-shot snapshot of system-resource headroom
// for the Herdforge fleet and a gate heavy operations call before spiking memory.
//
// Origin story: built from a live OOM (2026-07-24) that swapped 30GB on a 48GB
// host and killed the fleet — zero resource visibility.
//
// Operator directive 2026-07-29: ALERT keys on SWAP ONLY. macOS keeps unused
// pages near zero by design (file cache counts against "Pages free"), so low
// free% with zero swap is normal steady state — it must be TIGHT (warn), never
// ALERT. A free%-keyed refusal once blocked a healthy fleet at free=2%
// swap=0MB.
//
// FAC-826 (2026-09-12) supersedes the gate half of that directive without
// discarding its lesson. The lesson holds: low Darwin free% with no swap is
// normal, and swap residue is a scar, not a wound -- neither refuses anything
// here. What was wrong was the conclusion drawn from it. Because ALERT keyed on
// swap and swap had been removed as an input, Verdict could only return OK or
// TIGHT and GatePasses accepted both, so the gate could not refuse ANY host;
// meanwhile every probe failure and every malformed parse returned 100% free.
// The operator reported host instability and required CPU/memory safety over
// throughput. Whether any particular host failure coincided with a particular
// gate evaluation is NOT something we measured and is not claimed here; the
// fail-open implementation above is defect enough on its own. Admission
// (admission.go) is the one decision now: it reads the kernel's real pressure
// signal plus normalized CPU load, and an absence of measurement refuses
// instead of reporting headroom.
package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	VerdictOK    = "OK"
	VerdictTight = "TIGHT"
	VerdictAlert = "ALERT"

	defaultWarnFreePct = 20
	defaultSwapAlertMB = 2048
)

var errProbeFail = errors.New("probe failed")

type Snapshot struct {
	// FreePct is -1 when headroom could not be measured. It is NOT 100: an
	// unmeasured host used to render here as fully free, and a reporter that
	// prints 100 for "we do not know" is reporting a measurement it never
	// took (FAC-826).
	FreePct    int    `json:"free_pct"`
	SwapMB     int    `json:"swap_mb"`
	Verdict    string `json:"verdict"`
	Thresholds struct {
		WarnFreePct int `json:"warn_free_pct"`
		SwapAlertMB int `json:"swap_alert_mb"`
	} `json:"thresholds"`

	// Admission is the decision this verdict came from, carried whole so a
	// refusal reports its reasons and its observation postures instead of a
	// bare word.
	Admission *AdmissionReport `json:"admission,omitempty"`
}

type SelfTestResult struct {
	Name   string
	Pass   bool
	Detail string
}

func warnFreePct() int {
	v := os.Getenv("HERD_MEM_WARN_FREE_PCT")
	if v == "" {
		return defaultWarnFreePct
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultWarnFreePct
	}
	return n
}

func swapAlertMB() int {
	v := os.Getenv("HERD_SWAP_ALERT_MB")
	if v == "" {
		return defaultSwapAlertMB
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultSwapAlertMB
	}
	return n
}

// verdict grades free-memory headroom against explicit thresholds. Swap is
// deliberately excluded: macOS swap allocation is sticky and is retained as
// an informational trend in Snapshot only.
func verdict(freePct, swapMB, warnFreePct, swapAlertMB int) string {
	if freePct < warnFreePct {
		return VerdictTight
	}
	return VerdictOK
}

// Verdict grades free-memory headroom from free_pct. swap_mb is retained in
// the signature for API compatibility but is informational, not a gate input.
func Verdict(freePct, swapMB int) string {
	return verdict(freePct, swapMB, warnFreePct(), swapAlertMB())
}

// GatePasses reports whether a verdict allows heavy operations.
//
// FAC-826: it used to accept TIGHT as well as OK, and Verdict() could never
// return ALERT, so `herd resources --gate` could not exit nonzero on any real
// host and the ALERT arm of pkg/backfill's gate was unreachable. The gate was
// dead code, not a lenient policy. Only OK admits now, and a snapshot's verdict
// comes from the one Admission decision -- so TIGHT names a measured refusal
// and ALERT names a refusal because nothing could be measured.
func GatePasses(verdict string) bool {
	return verdict == VerdictOK
}

// TakeSnapshot reports the host through the one admission authority.
//
// FAC-826: the verdict no longer comes from a free-percentage grade that could
// only ever say OK or TIGHT. It comes from Admission, so an unmeasured or stale
// observation reports ALERT rather than 100% free and OK, and the reasons
// travel with it. The legacy free-percentage grade stays available as Verdict()
// for callers that want the informational reading.
func TakeSnapshot() Snapshot {
	a := Admit(context.Background())
	// Rendered at the clock it was decided at: a snapshot is the decision, not
	// a reuse of one.
	return SnapshotFrom(a, a.DecidedAt)
}

// SnapshotFrom renders an already-made decision. Split out so a fixture can
// assert the reporting shape without observing a host.
func SnapshotFrom(a Admission, at time.Time) Snapshot {
	report := a.Report(at)
	s := Snapshot{FreePct: -1, Verdict: report.Verdict, Admission: &report}
	// Only a reading the DECISION accepted may populate the legacy numeric
	// fields; the report is the authority on what was actually observed.
	if report.Memory.Known {
		if report.Memory.FreePct != nil {
			s.FreePct = *report.Memory.FreePct
		}
		if report.Memory.SwapMB != nil {
			s.SwapMB = *report.Memory.SwapMB
		}
	}
	s.Thresholds.WarnFreePct = warnFreePct()
	s.Thresholds.SwapAlertMB = swapAlertMB()
	return s
}

// gatherMetrics and gatherDarwinMetrics are GONE (FAC-826).
//
// They were the live fail-open: every probe failure returned free_pct=100,
// swap_mb=0, and on Linux the Darwin binaries do not exist at all, so a Linux
// host reported 100% free from probes that had never run. Admission observes
// through the platform-tagged observers in admission_{darwin,linux,other}.go,
// where an unmeasured host stays unmeasured.
//
// gatherDarwinMetricsWithProbes below is retained ONLY for the informational
// snapshotWithDarwinProbes reporting path and its existing fixtures. It still
// carries the old 100-on-failure shape, which is exactly why nothing that
// decides is allowed to call it.
func gatherDarwinMetricsWithProbes(memoryPressureFn, swapFn func() (string, error)) (int, int) {
	memoryPressureOut, err := memoryPressureFn()
	if err != nil {
		return 100, 0
	}
	freePct := parseMemoryPressureFreePct(memoryPressureOut)
	swapOut, err := swapFn()
	if err != nil {
		return freePct, 0
	}
	return freePct, parseSwapUsedMB(swapOut)
}

// runProbe shells out with a bounded default timeout.
//
// FAC-826: this used to be exec.Command with no context at all, so a probe that
// hung hung the caller with it and nothing cancelled the child. Every probe is
// bounded now, and cancellation reaches the process.
func runProbe(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultProbeTimeout)
	defer cancel()
	return runProbeCtx(ctx, defaultProbeTimeout, name, args...)
}

// runProbeCtx runs one probe under the caller's context and a hard timeout,
// whichever fires first. A timeout is an ERROR, never an empty string a parser
// could read as a healthy default.
func runProbeCtx(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, name, args...).Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return "", fmt.Errorf("probe %s timed out or was cancelled after %s: %w", name, timeout, probeCtx.Err())
		}
		return "", fmt.Errorf("probe %s failed: %w", name, err)
	}
	return string(out), nil
}

// snapshotWithProbes derives a snapshot through injectable probe funcs so the
// safe-on-failure path is unit-testable without shelling out. On any probe
// failure it defers to safeSnapshot.
func snapshotWithProbes(vmFreeFn, memSizeFn func() (string, error)) Snapshot {
	vmStatOut, err1 := vmFreeFn()
	memSizeOut, err2 := memSizeFn()
	if err1 != nil || err2 != nil {
		return safeSnapshot()
	}
	freePct := parseFreePct(vmStatOut, memSizeOut)
	s := Snapshot{FreePct: freePct}
	s.Verdict = Verdict(freePct, 0)
	s.Thresholds.WarnFreePct = warnFreePct()
	s.Thresholds.SwapAlertMB = swapAlertMB()
	return s
}

func snapshotWithDarwinProbes(memoryPressureFn, swapFn func() (string, error)) Snapshot {
	freePct, swapMB := gatherDarwinMetricsWithProbes(memoryPressureFn, swapFn)
	s := Snapshot{FreePct: freePct, SwapMB: swapMB}
	s.Verdict = Verdict(freePct, swapMB)
	s.Thresholds.WarnFreePct = warnFreePct()
	s.Thresholds.SwapAlertMB = swapAlertMB()
	return s
}

// safeSnapshot is the UNKNOWN snapshot, and it is no longer "safe" in the sense
// that word used to carry here.
//
// It returned free_pct=100, swap_mb=0 and an OK verdict, reasoning that a broken
// probe must never falsely refuse. That reasoning is inverted: a false refusal
// costs a delayed job, a false admission spends resources the host may not
// have. It reports
// an unmeasured host as unmeasured now -- free_pct=-1, ALERT -- so no consumer
// can render it as headroom.
func safeSnapshot() Snapshot {
	s := Snapshot{FreePct: -1, SwapMB: 0, Verdict: VerdictAlert}
	s.Thresholds.WarnFreePct = warnFreePct()
	s.Thresholds.SwapAlertMB = swapAlertMB()
	return s
}

func parseFreePct(vmStat, memSize string) int {
	var pagesFree int64
	for _, line := range strings.Split(vmStat, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Pages free:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				return 100
			}
			val := strings.TrimSpace(parts[1])
			val = strings.TrimSuffix(val, ".")
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return 100
			}
			pagesFree = n
			break
		}
	}
	if pagesFree == 0 {
		return 100
	}
	totalBytes, err := strconv.ParseInt(strings.TrimSpace(memSize), 10, 64)
	if err != nil || totalBytes <= 0 {
		return 100
	}
	pageSize := int64(16384)
	freeBytes := pagesFree * pageSize
	pct := freeBytes * 100 / totalBytes
	if pct < 0 {
		return 100
	}
	if pct > 100 {
		pct = 100
	}
	return int(pct)
}

func parseMemoryPressureFreePct(output string) int {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(strings.ToLower(line), "memory free percentage:") {
			continue
		}
		percent := strings.IndexByte(line, '%')
		if percent < 0 {
			return 100
		}
		prefix := strings.TrimSpace(line[:percent])
		start := len(prefix)
		for start > 0 && prefix[start-1] >= '0' && prefix[start-1] <= '9' {
			start--
		}
		if start == len(prefix) {
			return 100
		}
		n, err := strconv.Atoi(prefix[start:])
		if err != nil || n < 0 || n > 100 {
			return 100
		}
		return n
	}
	return 100
}

func parseSwapUsedMB(swapOutput string) int {
	for _, line := range strings.Split(swapOutput, "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, "used = ")
		if idx < 0 {
			continue
		}
		rest := line[idx+len("used = "):]
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		token := fields[0]
		if strings.HasSuffix(token, "G") {
			valStr := strings.TrimSuffix(token, "G")
			val, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				return 0
			}
			return int(val * 1024)
		}
		if strings.HasSuffix(token, "M") {
			valStr := strings.TrimSuffix(token, "M")
			val, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				return 0
			}
			return int(val)
		}
		return 0
	}
	return 0
}

func SelfTest() []SelfTestResult {
	cases := []struct {
		name    string
		freePct int
		swapMB  int
		want    string
	}{
		{"OK: high free, no swap", 80, 0, VerdictOK},
		{"OK: at warn threshold", 20, 0, VerdictOK},
		{"TIGHT: below warn, no swap", 10, 0, VerdictTight},
		{"TIGHT: zero free, no swap (not ALERT)", 0, 0, VerdictTight},
		{"OK: high free, swap at alert threshold", 80, 2048, VerdictOK},
		{"swap is informational", 80, 30720, VerdictOK},
	}
	// Assert the pure core against the pinned defaults so the selftest is
	// deterministic regardless of ambient env (a hostile
	// HERD_MEM_WARN_FREE_PCT must not flip the assertions).
	pure := func(freePct, swapMB int) string {
		return verdict(freePct, swapMB, defaultWarnFreePct, defaultSwapAlertMB)
	}
	var results []SelfTestResult
	for _, c := range cases {
		got := pure(c.freePct, c.swapMB)
		pass := got == c.want
		detail := ""
		if !pass {
			detail = fmt.Sprintf("verdict(%d, %d, warn=%d, alert=%d) = %q, want %q", c.freePct, c.swapMB, defaultWarnFreePct, defaultSwapAlertMB, got, c.want)
		}
		results = append(results, SelfTestResult{Name: c.name, Pass: pass, Detail: detail})
	}
	return results
}
