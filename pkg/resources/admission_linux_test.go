//go:build linux

package resources

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

// The Linux reading must come from /proc, never from the Darwin probes. Before
// FAC-826 a Linux host ran vm_stat and sysctl, both absent, and the failure
// returned 100% free: a measurement claim about a machine nobody had measured.
func TestParseMeminfoHeadroom(t *testing.T) {
	const meminfo = `MemTotal:       16384000 kB
MemFree:          512000 kB
MemAvailable:    8192000 kB
SwapTotal:       2097152 kB
SwapFree:        1048576 kB
`
	head, err := parseMeminfoHeadroom(meminfo)
	if err != nil {
		t.Fatalf("parseMeminfoHeadroom: %v", err)
	}
	// MemAvailable, not MemFree: MemFree excludes reclaimable page cache and
	// understates headroom the same way Darwin's Pages free does.
	if head.FreePct != 50 {
		t.Fatalf("FreePct = %d, want 50 from MemAvailable/MemTotal", head.FreePct)
	}
	if !head.SwapKnown || head.SwapMB != 1024 {
		t.Fatalf("swap = %dMB known=%v, want 1024MB known (informational only)", head.SwapMB, head.SwapKnown)
	}
}

func TestParseMeminfoHeadroomRefusesUnusableInput(t *testing.T) {
	for _, bad := range []string{
		"",
		"MemTotal:       16384000 kB\n",
		"MemAvailable:    8192000 kB\n",
		"MemTotal:              0 kB\nMemAvailable:    8192000 kB\n",
	} {
		if head, err := parseMeminfoHeadroom(bad); err == nil {
			t.Errorf("parseMeminfoHeadroom(%q) = %+v with no error; an unreadable /proc must refuse", bad, head)
		}
	}
}

// MemAvailable is allowed to decide on Linux, unlike Darwin's free percentage.
// The flag is what keeps the two platforms from borrowing each other's meaning.
func TestLinuxHeadroomGates(t *testing.T) {
	head, err := parseMeminfoHeadroom("MemTotal: 16384000 kB\nMemAvailable: 8192000 kB\n")
	if err != nil {
		t.Fatal(err)
	}
	if !head.FreePctGates {
		t.Fatal("linux MemAvailable must gate; a reserve against it means what it says")
	}
	if !head.Usable() {
		t.Fatal("a MemAvailable reading is a usable observation on its own, with or without PSI")
	}
}

// PSI is this platform's pressure signal where the kernel exposes it. Its
// ABSENCE is not a refusal -- plenty of healthy WSL kernels lack CONFIG_PSI --
// but a malformed file must never read as "no pressure".
func TestParsePSIPressure(t *testing.T) {
	calm := "some avg10=0.42 avg60=0.10 avg300=0.03 total=123\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	if level, err := parsePSIPressure(calm); err != nil || level != PressureNormal {
		t.Fatalf("parsePSIPressure(calm) = %v, %v; want normal, nil", level, err)
	}
	stalled := "some avg10=42.00 avg60=30.00 avg300=10.00 total=999\n"
	if level, err := parsePSIPressure(stalled); err != nil || level != PressureWarn {
		t.Fatalf("parsePSIPressure(stalled) = %v, %v; want warning, nil", level, err)
	}
	for _, bad := range []string{"", "full avg10=1.00\n", "some avg60=1.00\n", "some avg10=notanumber\n"} {
		if level, err := parsePSIPressure(bad); err == nil {
			t.Errorf("parsePSIPressure(%q) = %v with no error; an unreadable psi file must not assert calm", bad, level)
		}
	}
}

// PSI must not be able to claim calm from a malformed value. ParseFloat accepts
// "NaN" and "Inf", and NaN compared against the stall threshold is false, which
// returned PressureNormal.
func TestParsePSIRejectsNonFiniteAndOutOfRange(t *testing.T) {
	for _, bad := range []string{
		"some avg10=NaN avg60=0 avg300=0 total=1\n",
		"some avg10=Inf avg60=0 avg300=0 total=1\n",
		"some avg10=+Inf avg60=0 avg300=0 total=1\n",
		"some avg10=-Inf avg60=0 avg300=0 total=1\n",
		"some avg10=-1.0 avg60=0 avg300=0 total=1\n",
		"some avg10=101 avg60=0 avg300=0 total=1\n",
		"some avg60=1.00 avg300=0 total=1\n",
	} {
		level, err := parsePSIPressure(bad)
		if err == nil {
			t.Errorf("parsePSIPressure(%q) = %v with no error", bad, level)
		}
		if level != PressureUnknown {
			t.Errorf("parsePSIPressure(%q) = %v, want PressureUnknown on error", bad, level)
		}
	}
}

// Only a kernel that genuinely lacks PSI may fall back to MemAvailable alone.
// A PSI file that is present but broken is telemetry about a kernel that DOES
// support it, and broken telemetry may not be reported as calm.
func TestLinuxPSIPresentButBrokenIsNotCalm(t *testing.T) {
	meminfo := "MemTotal: 16384000 kB\nMemAvailable: 8192000 kB\n"

	// Genuinely absent: MemAvailable still decides, and the reading is usable.
	absent := observeLinuxMemory(context.Background(), time.Now(), DefaultLimits(),
		func(string) (string, error) { return meminfo, nil },
		func(string) (string, error) { return "", fs.ErrNotExist })
	head, ok := absent.Value()
	if !ok || !head.Usable() || head.Pressure.Known() {
		t.Fatalf("a kernel without PSI must still decide on MemAvailable: %+v ok=%v", head, ok)
	}

	// Present but malformed, and present but unreadable: both unknown.
	for name, psi := range map[string]func(string) (string, error){
		"malformed":  func(string) (string, error) { return "some avg10=NaN total=1\n", nil },
		"unreadable": func(string) (string, error) { return "", fs.ErrPermission },
	} {
		t.Run(name, func(t *testing.T) {
			r := observeLinuxMemory(context.Background(), time.Now(), DefaultLimits(),
				func(string) (string, error) { return meminfo, nil }, psi)
			if _, ok := r.Value(); ok {
				t.Fatalf("broken PSI telemetry produced a usable reading")
			}
			if r.State != freshness.StateUnknown {
				t.Fatalf("state = %q, want UNKNOWN", r.State)
			}
		})
	}
}

// MemAvailable above MemTotal is a broken reading, not a 100% healthy one.
// Clamping turned the most impossible input into the most reassuring output.
func TestMeminfoRejectsImpossibleAndHugeValues(t *testing.T) {
	if head, err := parseMeminfoHeadroom("MemTotal: 1000 kB\nMemAvailable: 5000 kB\n"); err == nil {
		t.Fatalf("available above total was accepted as %+v", head)
	}
	// A pathological but in-range pair must still compute without overflowing.
	head, err := parseMeminfoHeadroom("MemTotal: 9223372036854775000 kB\nMemAvailable: 4611686018427387500 kB\n")
	if err != nil {
		t.Fatalf("a large but valid pair failed: %v", err)
	}
	if head.FreePct != 50 {
		t.Fatalf("FreePct = %d, want 50; the percentage overflowed", head.FreePct)
	}
}
