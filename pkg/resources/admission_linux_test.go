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
