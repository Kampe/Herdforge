//go:build linux

package resources

import (
	"context"
	"fmt"
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
)

// Linux and WSL read plain files: no subprocess at all, so there is nothing to
// time out, hang, or leak. They must never reach the Darwin probes -- before
// this, a Linux host ran vm_stat and sysctl, both absent, and the failure
// returned free_pct=100: a fabricated healthy reading for a machine nobody had
// measured. Build tags make that unreachable rather than merely unlikely.
func observeCPU(ctx context.Context, at time.Time, limits Limits) freshness.Reading[CPULoad] {
	raw, err := readFileCtx(ctx, "/proc/loadavg")
	if err != nil {
		return unknownReading[CPULoad](linuxCPUSource, err, "verify /proc is mounted and readable")
	}
	return cpuReadingFrom(linuxCPUSource, at, raw, runtime.NumCPU())
}

func observeMemory(ctx context.Context, at time.Time, limits Limits) freshness.Reading[MemHeadroom] {
	raw, err := readFileCtx(ctx, "/proc/meminfo")
	if err != nil {
		return unknownReading[MemHeadroom](linuxMemSource, err, "verify /proc is mounted and readable")
	}
	head, err := parseMeminfoHeadroom(raw)
	if err != nil {
		return unknownReading[MemHeadroom](linuxMemSource, err, "/proc/meminfo was not in the expected shape")
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

// parseMeminfoHeadroom uses MemAvailable, which is the kernel's own answer to
// "how much can a new workload get without swapping", rather than MemFree,
// which excludes reclaimable page cache and understates headroom exactly the
// way Darwin's Pages free does.
//
// A missing or unusable field is an error. Zero is not a safe default here: it
// would refuse a healthy host, and an invented 100 would admit on a dying one.
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
	pct := avail * 100 / total
	if pct > 100 {
		pct = 100
	}
	head := MemHeadroom{FreePct: int(pct)}
	// Informational only, same contract as Darwin: swap never decides.
	if swapTotal, ok := fields["SwapTotal"]; ok {
		if swapFree, okFree := fields["SwapFree"]; okFree && swapTotal >= swapFree {
			head.SwapMB, head.SwapKnown = int((swapTotal-swapFree)/1024), true
		}
	}
	return head, nil
}
