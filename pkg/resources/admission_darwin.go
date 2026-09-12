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

// observeMemory asks the Darwin kernel directly.
//
// The AUTHORITATIVE signal is kern.memorystatus_vm_pressure_level: 1 normal,
// 2 warning, 4 critical. That is the same value the memorystatus subsystem
// notifies on, so it is what the OS itself means by "under memory pressure".
//
// It is NOT the free percentage, and the first version of this file got that
// wrong (followup-3022). memory_pressure -Q reports "System-wide memory free
// percentage", which is a page count: the file cache and the compressor both
// count against it, so it sits near zero on a perfectly healthy Mac. The
// performance guard measured exactly that at 2026-09-12T18:26:31Z -- 1993MiB
// unused with 10GiB in the compressor and zero swap, while the pressure level
// read 1. Any reserve worth having would have refused that host.
//
// So the percentage is carried for the report with FreePctGates false, beside
// swap, which has never been allowed to decide since FAC-693. If the pressure
// probe fails there is NO fallback: the percentage cannot stand in for a signal
// it does not measure, and an unknown memory reading refuses.
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
