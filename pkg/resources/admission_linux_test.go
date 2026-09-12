//go:build linux

package resources

import "testing"

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
