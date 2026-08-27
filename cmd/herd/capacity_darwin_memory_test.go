package main

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// FAC-718: a Mac reports no /proc/meminfo, so hostMemoryMiB returned -1 and
// derivedReviewLimit collapsed the whole host's ceiling to ONE reviewer. That
// is not "unknown is not a refusal" -- it refuses every reviewer after the
// first, and that is how a single settled reviewer blocked another project's
// admission entirely while the host had 48GiB free.
//
// These go through hostMemoryMiB and readHostMemory -- the functions the
// capacity decision actually calls -- rather than darwinMemoryMiB directly.
// Testing the probe alone would prove the probe works while the shipped path
// still returned -1, which is precisely the vacuous-test shape that made
// FAC-578 necessary.

func TestHostMemoryIsMeasurableOnThisPlatform(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("covers the Darwin fallback; /proc/meminfo already serves Linux")
	}
	total, avail, _ := hostMemoryMiB()
	if total <= 0 {
		t.Fatalf("total memory unmeasured on darwin (got %d); derivedReviewLimit collapses the host to 1 reviewer when this is <= 0", total)
	}
	if avail <= 0 {
		t.Fatalf("available memory unmeasured on darwin (got %d); the MemAvailable gate cannot protect a host it cannot read", avail)
	}
	if avail > total {
		t.Fatalf("available %dMiB exceeds total %dMiB; the page arithmetic is wrong", avail, total)
	}
}

// The defect was never about the probe -- it was that the CEILING was 1. This
// pins the consequence, so a future change that leaves hostMemoryMiB working
// but stops feeding derivedReviewLimit still fails.
func TestReviewCeilingExceedsOneOnAMeasurableHost(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("covers the Darwin fallback")
	}
	total, _, _ := hostMemoryMiB()
	if got := derivedReviewLimit(total, 4096); got < 2 {
		t.Fatalf("review ceiling is %d on a host with %dMiB; the whole fleet serialises to one review at a time", got, total)
	}
}

// Swap must stay UNMEASURED on darwin. macOS pages into a dynamically grown
// compressed swap file as normal operation, so its swap LEVEL is not the Linux
// exhaustion the 75% backstop was calibrated against. Reporting it would fire
// that gate on a healthy machine and refuse every review -- converting a
// too-small cap into a total outage.
func TestDarwinSwapStaysUnmeasuredRatherThanTrippingALinuxThreshold(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("covers the Darwin fallback")
	}
	_, _, swapUsed := hostMemoryMiB()
	if swapUsed >= 0 {
		t.Fatalf("darwin reported swapUsed=%d; a macOS swap level is not comparable to the Linux exhaustion signal swapExhaustedPct=%d encodes, and this host sits above that threshold while healthy",
			swapUsed, swapExhaustedPct)
	}
}

// A number is only actionable if an operator can find where it came from.
func TestMemorySourceNamesTheInterfaceItActuallyRead(t *testing.T) {
	got := readHostMemory().MustExplain(time.Now())
	if runtime.GOOS == "darwin" {
		if !strings.Contains(got, "sysctl") {
			t.Fatalf("darwin memory source does not name sysctl/vm_stat: %q", got)
		}
		if strings.Contains(got, "/proc/meminfo") {
			t.Fatalf("darwin memory source claims /proc/meminfo, a file that does not exist on this host: %q", got)
		}
		return
	}
	if !strings.Contains(got, "/proc/meminfo") {
		t.Fatalf("linux memory source does not name /proc/meminfo: %q", got)
	}
}
