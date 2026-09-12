//go:build linux

package resources

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

const (
	linuxCPUSource = "linux /proc/loadavg"
	linuxMemSource = "linux /proc/meminfo"
	linuxPSIPath   = "/proc/pressure/memory"
	// linuxPSIStallPct is the share of the last 10s that work stalled waiting on
	// memory, above which the kernel is reporting memory trouble. It matches the
	// threshold cmd/herd/capacity.go already uses, so the two cannot drift.
	linuxPSIStallPct = 10.0
)

// Linux and WSL read plain files: no subprocess, so nothing to time out or
// leak. Build tags keep them off the Darwin probes, which do not exist here and
// whose failure used to return a fabricated 100% free.
func observeCPU(ctx context.Context, at time.Time, limits Limits) freshness.Reading[CPULoad] {
	raw, err := readFileCtx(ctx, "/proc/loadavg")
	if err != nil {
		return unknownReading[CPULoad](linuxCPUSource, err, "verify /proc is mounted and readable")
	}
	return cpuReadingFrom(linuxCPUSource, at, raw, runtime.NumCPU())
}

// observeMemory reads MemAvailable, which unlike Darwin's free percentage IS
// the kernel's answer to "how much can a new workload get without swapping", so
// a reserve against it means what it says and FreePctGates is true here.
//
// PSI is this platform's pressure signal. Its ABSENCE is tolerated -- /proc
// needs CONFIG_PSI and healthy WSL kernels lack it, and MemAvailable still
// decides. Anything else is NOT tolerated: a PSI file that exists but cannot be
// read, or reads back malformed, is broken telemetry about a kernel that does
// support it, and broken telemetry may not be reported as calm.
func observeMemory(ctx context.Context, at time.Time, limits Limits) freshness.Reading[MemHeadroom] {
	read := func(path string) (string, error) { return readFileCtx(ctx, path) }
	return observeLinuxMemory(ctx, at, limits, read, read)
}

// observeLinuxMemory takes its readers injected so a fixture can drive every
// branch without depending on the host it runs on.
func observeLinuxMemory(ctx context.Context, at time.Time, limits Limits, readMeminfo, readPSI func(string) (string, error)) freshness.Reading[MemHeadroom] {
	raw, err := readMeminfo("/proc/meminfo")
	if err != nil {
		return unknownReading[MemHeadroom](linuxMemSource, err, "verify /proc is mounted and readable")
	}
	head, err := parseMeminfoHeadroom(raw)
	if err != nil {
		return unknownReading[MemHeadroom](linuxMemSource, err, "/proc/meminfo was not in the expected shape")
	}

	psi, psiErr := readPSI(linuxPSIPath)
	switch {
	case psiErr == nil:
		level, parseErr := parsePSIPressure(psi)
		if parseErr != nil {
			return unknownReading[MemHeadroom](linuxMemSource,
				fmt.Errorf("%s exists but is unusable: %w", linuxPSIPath, parseErr),
				"a kernel that exposes PSI but reports it malformed is broken telemetry; fix or disable it rather than reading it as calm")
		}
		head.Pressure = level
	case errors.Is(psiErr, fs.ErrNotExist):
		// Documented unavailable: this kernel has no PSI. MemAvailable gates.
	default:
		return unknownReading[MemHeadroom](linuxMemSource,
			fmt.Errorf("%s could not be read: %w", linuxPSIPath, psiErr),
			"the pressure file exists but is unreadable (permissions or a failing mount); that is not the same as a kernel without PSI")
	}
	return freshness.Fresh(linuxMemSource, at, head)
}

func readFileCtx(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parsePSIPressure reads "some avg10" from /proc/pressure/memory.
//
// A malformed, non-finite or out-of-range value is an error, never
// PressureNormal: ParseFloat accepts "NaN" and "Inf", and NaN compared against
// the stall threshold is false, which would have reported calm.
func parsePSIPressure(raw string) (PressureLevel, error) {
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "some ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			key, value, found := strings.Cut(field, "=")
			if !found || key != "avg10" {
				continue
			}
			stalled, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return PressureUnknown, fmt.Errorf("psi avg10 %q is not a number: %w", value, err)
			}
			if !finite(stalled) {
				return PressureUnknown, fmt.Errorf("psi avg10 %q is not finite", value)
			}
			if stalled < 0 || stalled > 100 {
				return PressureUnknown, fmt.Errorf("psi avg10 %v is outside 0-100", stalled)
			}
			if stalled >= linuxPSIStallPct {
				return PressureWarn, nil
			}
			return PressureNormal, nil
		}
		return PressureUnknown, fmt.Errorf("%s `some` line carried no avg10 field", linuxPSIPath)
	}
	return PressureUnknown, fmt.Errorf("%s carried no `some` line", linuxPSIPath)
}

// parseMeminfoHeadroom uses MemAvailable, the kernel's own answer to what a new
// workload can get without swapping, rather than MemFree, which excludes
// reclaimable page cache.
//
// Missing or impossible fields are errors. Available above total is rejected
// rather than clamped to 100: clamping turns a broken reading into the
// healthiest possible one.
func parseMeminfoHeadroom(raw string) (MemHeadroom, error) {
	fields := map[string]int64{}
	for _, line := range strings.Split(raw, "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		parts := strings.Fields(rest)
		if len(parts) == 0 {
			continue
		}
		v, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}
		fields[strings.TrimSpace(name)] = v
	}
	total, okTotal := fields["MemTotal"]
	avail, okAvail := fields["MemAvailable"]
	if !okTotal || !okAvail {
		return MemHeadroom{}, fmt.Errorf("/proc/meminfo lacked MemTotal and/or MemAvailable")
	}
	if total <= 0 {
		return MemHeadroom{}, fmt.Errorf("/proc/meminfo reported MemTotal=%d", total)
	}
	if avail < 0 {
		return MemHeadroom{}, fmt.Errorf("/proc/meminfo reported MemAvailable=%d", avail)
	}
	if avail > total {
		return MemHeadroom{}, fmt.Errorf("/proc/meminfo reported MemAvailable=%d above MemTotal=%d", avail, total)
	}
	pct, err := percentOf(avail, total)
	if err != nil {
		return MemHeadroom{}, err
	}
	head := MemHeadroom{FreePct: pct, FreePctGates: true}
	// Informational only, same contract as Darwin: swap never decides.
	if swapTotal, ok := fields["SwapTotal"]; ok {
		if swapFree, okFree := fields["SwapFree"]; okFree && swapTotal >= swapFree && swapFree >= 0 {
			head.SwapMB, head.SwapKnown = int((swapTotal-swapFree)/1024), true
		}
	}
	return head, nil
}

// percentOf computes part*100/whole without overflowing int64. part<=whole is
// the caller's guarantee, so the result is always 0-100.
func percentOf(part, whole int64) (int, error) {
	if whole <= 0 {
		return 0, fmt.Errorf("cannot take a percentage of %d", whole)
	}
	if part > math.MaxInt64/100 {
		// Divide first. Precision loss here is irrelevant at these magnitudes,
		// and an overflowed product is not.
		return int((part / whole) * 100), nil
	}
	return int(part * 100 / whole), nil
}
