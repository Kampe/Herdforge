//go:build darwin

package resources

import (
	"context"
	"runtime"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

const (
	darwinCPUSource = "darwin sysctl vm.loadavg"
	darwinMemSource = "darwin kern.memorystatus_vm_pressure_level"
	// darwinFreeLabel is memory_pressure's free-page figure. It is read for the
	// REPORT only. See observeMemory.
	darwinFreeLabel = "memory free percentage:"
)

func observeCPU(ctx context.Context, at time.Time, limits Limits) freshness.Reading[CPULoad] {
	out, err := runProbeCtx(ctx, limits.ProbeTimeout, "sysctl", "-n", "vm.loadavg")
	if err != nil {
		return unknownReading[CPULoad](darwinCPUSource, err, "verify sysctl is on PATH and vm.loadavg is readable")
	}
	return cpuReadingFrom(darwinCPUSource, at, out, runtime.NumCPU())
}

// observeMemory reads the Darwin kernel's own pressure level:
// kern.memorystatus_vm_pressure_level, 1 normal / 2 warning / 4 critical, the
// value the memorystatus subsystem notifies on. That OID is not listed by
// `sysctl -a` and must be queried by name.
//
// The free percentage from memory_pressure -Q is a PAGE COUNT -- file cache and
// compressor count against it, so it sits near zero on a healthy Mac -- and is
// carried for the report with FreePctGates false, beside swap. A failed
// pressure probe has NO fallback: the percentage cannot stand in for a signal
// it does not measure, so the reading is unknown and refuses.
func observeMemory(ctx context.Context, at time.Time, limits Limits) freshness.Reading[MemHeadroom] {
	out, err := runProbeCtx(ctx, limits.ProbeTimeout, "sysctl", "-n", "kern.memorystatus_vm_pressure_level")
	if err != nil {
		return unknownReading[MemHeadroom](darwinMemSource, err,
			"kern.memorystatus_vm_pressure_level is not listed by `sysctl -a` and must be queried by name; verify sysctl is on PATH")
	}
	level, err := parseDarwinPressureLevel(out)
	if err != nil {
		return unknownReading[MemHeadroom](darwinMemSource, err, "the kernel pressure level was unreadable; it has no substitute here")
	}

	head := MemHeadroom{Pressure: level, FreePct: -1, FreePctGates: false}
	// Informational only, and best-effort: a failed report probe must not turn
	// a known-good pressure level into an unknown reading.
	if freeOut, freeErr := runProbeCtx(ctx, limits.ProbeTimeout, "memory_pressure", "-Q"); freeErr == nil {
		if pct, pctErr := parseFreePctStrict(freeOut, darwinFreeLabel); pctErr == nil {
			head.FreePct = pct
		}
	}
	if swapOut, swapErr := runProbeCtx(ctx, limits.ProbeTimeout, "sysctl", "-n", "vm.swapusage"); swapErr == nil {
		head.SwapMB, head.SwapKnown = parseSwapUsedMB(swapOut), true
	}
	return freshness.Fresh(darwinMemSource, at, head)
}
