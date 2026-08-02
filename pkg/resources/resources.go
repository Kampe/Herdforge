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
package resources

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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
	FreePct    int    `json:"free_pct"`
	SwapMB     int    `json:"swap_mb"`
	Verdict    string `json:"verdict"`
	Thresholds struct {
		WarnFreePct int `json:"warn_free_pct"`
		SwapAlertMB int `json:"swap_alert_mb"`
	} `json:"thresholds"`
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

// verdict grades free-memory headroom against explicit thresholds. It is
// the pure, side-effect-free core: swap_mb above the alert threshold =>
// ALERT; otherwise free_pct below the warn threshold => TIGHT; otherwise
// OK. ALERT keys on swap only (operator directive 2026-07-29).
func verdict(freePct, swapMB, warnFreePct, swapAlertMB int) string {
	if swapMB > swapAlertMB {
		return VerdictAlert
	}
	if freePct < warnFreePct {
		return VerdictTight
	}
	return VerdictOK
}

// Verdict grades free-memory headroom. swap_mb above the alert threshold
// => ALERT; otherwise free_pct below the warn threshold => TIGHT; otherwise
// OK. Thresholds come from the ambient environment
// (HERD_MEM_WARN_FREE_PCT / HERD_SWAP_ALERT_MB) with package defaults.
func Verdict(freePct, swapMB int) string {
	return verdict(freePct, swapMB, warnFreePct(), swapAlertMB())
}

// GatePasses reports whether a verdict allows heavy operations (OK or TIGHT).
func GatePasses(verdict string) bool {
	return verdict == VerdictOK || verdict == VerdictTight
}

func TakeSnapshot() Snapshot {
	freePct, swapMB := gatherMetrics()
	s := Snapshot{FreePct: freePct, SwapMB: swapMB}
	s.Verdict = Verdict(freePct, swapMB)
	s.Thresholds.WarnFreePct = warnFreePct()
	s.Thresholds.SwapAlertMB = swapAlertMB()
	return s
}

// gatherMetrics shells out to macOS probes. On any probe failure it returns a
// SAFE value (free_pct=100, swap_mb=0) so a broken probe never falsely refuses
// heavy operations.
func gatherMetrics() (freePct, swapMB int) {
	vmStatOut, err1 := runProbe("vm_stat")
	memSizeOut, err2 := runProbe("sysctl", "-n", "hw.memsize")
	if err1 != nil || err2 != nil {
		return 100, 0
	}
	freePct = parseFreePct(vmStatOut, memSizeOut)
	swapOut, err := runProbe("sysctl", "-n", "vm.swapusage")
	if err != nil {
		swapMB = 0
	} else {
		swapMB = parseSwapUsedMB(swapOut)
	}
	return freePct, swapMB
}

func runProbe(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
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

func safeSnapshot() Snapshot {
	s := Snapshot{FreePct: 100, SwapMB: 0}
	s.Verdict = Verdict(s.FreePct, s.SwapMB)
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
		{"ALERT: swap above alert", 80, 2049, VerdictAlert},
		{"ALERT wins over TIGHT", 5, 5000, VerdictAlert},
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
