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
	darwinMemSource = "darwin memory_pressure"
	// darwinFreeLabel is the kernel's own headroom figure. Deliberately NOT
	// "Pages free": macOS keeps free pages near zero by design because the file
	// cache counts against them, so low pagesfree is normal steady state and
	// not danger on its own.
	darwinFreeLabel = "memory free percentage:"
)

func observeCPU(ctx context.Context, at time.Time, limits Limits) freshness.Reading[CPULoad] {
	out, err := runProbeCtx(ctx, limits.ProbeTimeout, "sysctl", "-n", "vm.loadavg")
	if err != nil {
		return unknownReading[CPULoad](darwinCPUSource, err, "verify sysctl is on PATH and vm.loadavg is readable")
	}
	return cpuReadingFrom(darwinCPUSource, at, out, runtime.NumCPU())
}

func observeMemory(ctx context.Context, at time.Time, limits Limits) freshness.Reading[MemHeadroom] {
	out, err := runProbeCtx(ctx, limits.ProbeTimeout, "memory_pressure", "-Q")
	if err != nil {
		return unknownReading[MemHeadroom](darwinMemSource, err, "verify memory_pressure is on PATH; do not infer headroom from Pages free")
	}
	freePct, err := parseFreePctStrict(out, darwinFreeLabel)
	if err != nil {
		return unknownReading[MemHeadroom](darwinMemSource, err, "memory_pressure output was not in the expected shape")
	}
	head := MemHeadroom{FreePct: freePct}
	// Swap is carried for the report and NEVER decides. FAC-693: a sticky swap
	// scar from a long-finished spike read as current pressure and refused
	// every launch on a healthy host, and only a manual swapoff could clear it.
	// A failed swap probe therefore degrades the swap field, not the decision.
	if swapOut, swapErr := runProbeCtx(ctx, limits.ProbeTimeout, "sysctl", "-n", "vm.swapusage"); swapErr == nil {
		head.SwapMB, head.SwapKnown = parseSwapUsedMB(swapOut), true
	}
	return freshness.Fresh(darwinMemSource, at, head)
}
