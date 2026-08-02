package resources

import (
	"os"
	"testing"
)

func TestVerdict_Table(t *testing.T) {
	cases := []struct {
		name    string
		freePct int
		swapMB  int
		want    string
	}{
		{"plenty of memory, no swap", 80, 0, "OK"},
		{"moderate memory, no swap", 50, 0, "OK"},
		{"exactly at warn threshold", 20, 0, "OK"},
		{"just below warn threshold", 19, 0, "TIGHT"},
		{"very low free, no swap — TIGHT not ALERT", 2, 0, "TIGHT"},
		{"zero free, no swap — TIGHT not ALERT", 0, 0, "TIGHT"},
		{"good free, swap at alert threshold", 80, 2048, "OK"},
		{"good free, swap above alert", 80, 2049, "ALERT"},
		{"good free, huge swap", 80, 30720, "ALERT"},
		{"low free AND high swap — ALERT wins", 5, 5000, "ALERT"},
		{"exactly at swap alert — not alert (>)", 80, 2048, "OK"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearEnv(t)
			got := Verdict(c.freePct, c.swapMB)
			if got != c.want {
				t.Errorf("Verdict(%d, %d) = %q, want %q", c.freePct, c.swapMB, got, c.want)
			}
		})
	}
}

func TestVerdict_EnvOverrides(t *testing.T) {
	t.Run("custom warn threshold", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("HERD_MEM_WARN_FREE_PCT", "50")
		if got := Verdict(45, 0); got != "TIGHT" {
			t.Errorf("with warn=50, Verdict(45, 0) = %q, want TIGHT", got)
		}
		if got := Verdict(50, 0); got != "OK" {
			t.Errorf("with warn=50, Verdict(50, 0) = %q, want OK", got)
		}
	})
	t.Run("custom swap alert", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("HERD_SWAP_ALERT_MB", "512")
		if got := Verdict(80, 600); got != "ALERT" {
			t.Errorf("with swap_alert=512, Verdict(80, 600) = %q, want ALERT", got)
		}
		if got := Verdict(80, 512); got != "OK" {
			t.Errorf("with swap_alert=512, Verdict(80, 512) = %q, want OK", got)
		}
	})
	t.Run("both overrides together", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("HERD_MEM_WARN_FREE_PCT", "30")
		t.Setenv("HERD_SWAP_ALERT_MB", "100")
		if got := Verdict(10, 200); got != "ALERT" {
			t.Errorf("with warn=30 swap_alert=100, Verdict(10, 200) = %q, want ALERT", got)
		}
		if got := Verdict(10, 50); got != "TIGHT" {
			t.Errorf("with warn=30 swap_alert=100, Verdict(10, 50) = %q, want TIGHT", got)
		}
	})
	t.Run("invalid env falls back to default", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("HERD_MEM_WARN_FREE_PCT", "not-a-number")
		if got := Verdict(19, 0); got != "TIGHT" {
			t.Errorf("with invalid warn env, Verdict(19, 0) = %q, want TIGHT (default 20)", got)
		}
	})
}

func TestVerdict_Defaults(t *testing.T) {
	clearEnv(t)
	if got := warnFreePct(); got != 20 {
		t.Errorf("default warnFreePct = %d, want 20", got)
	}
	if got := swapAlertMB(); got != 2048 {
		t.Errorf("default swapAlertMB = %d, want 2048", got)
	}
}

func TestParseSwapUsedMB(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"total = 4096.00M  used = 2048.00M  free = 2048.00M", 2048},
		{"used = 0.00M", 0},
		{"used = 512.50M", 512},
		{"used = 30720.00M", 30720},
		{"used = 1.50G", 1536},
		{"used = 0.75G", 768},
		{"used = 2.00G", 2048},
		{"no swap line here", 0},
		{"", 0},
		{"garbage output", 0},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got := parseSwapUsedMB(c.input)
			if got != c.want {
				t.Errorf("parseSwapUsedMB(%q) = %d, want %d", c.input, got, c.want)
			}
		})
	}
}

func TestParseFreePct(t *testing.T) {
	cases := []struct {
		name    string
		vmStat  string
		memSize string
		want    int
	}{
		{"normal vm_stat", "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:                   500000.\nPages active:                 1600000.\n", "34359738368", 23},
		{"empty vm_stat", "", "34359738368", 100},
		{"missing pages free line", "Mach Virtual Memory Statistics:\nPages active: 100.\n", "34359738368", 100},
		{"invalid memsize", "Pages free: 100.\n", "not-a-number", 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseFreePct(c.vmStat, c.memSize)
			if got != c.want {
				t.Errorf("parseFreePct = %d, want %d", got, c.want)
			}
		})
	}
}

func TestSnapshot_SafeOnProbeFailure(t *testing.T) {
	clearEnv(t)
	snap := snapshotWithProbes(func() (string, error) {
		return "", errProbeFail
	}, func() (string, error) {
		return "", errProbeFail
	})
	if snap.FreePct != 100 {
		t.Errorf("expected safe FreePct=100 on probe failure, got %d", snap.FreePct)
	}
	if snap.SwapMB != 0 {
		t.Errorf("expected safe SwapMB=0 on probe failure, got %d", snap.SwapMB)
	}
	if v := Verdict(snap.FreePct, snap.SwapMB); v != "OK" {
		t.Errorf("expected OK verdict on safe values, got %s", v)
	}
}

func TestSelfTest(t *testing.T) {
	clearEnv(t)
	results := SelfTest()
	if len(results) == 0 {
		t.Fatal("expected at least one self-test case")
	}
	for _, r := range results {
		if !r.Pass {
			t.Errorf("self-test case %q failed: %s", r.Name, r.Detail)
		}
	}
}

// TestSelfTest_DeterministicUnderHostileEnv is the FAC-79 regression: a
// hostile HERD_MEM_WARN_FREE_PCT must not flip the selftest assertions.
// The selftest pins the pure core against the default thresholds.
func TestSelfTest_DeterministicUnderHostileEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("HERD_MEM_WARN_FREE_PCT", "99")
	t.Setenv("HERD_SWAP_ALERT_MB", "1")
	results := SelfTest()
	if len(results) == 0 {
		t.Fatal("expected at least one self-test case")
	}
	for _, r := range results {
		if !r.Pass {
			t.Errorf("self-test case %q failed under hostile env: %s", r.Name, r.Detail)
		}
	}
}

func TestGateDecision(t *testing.T) {
	cases := []struct {
		name    string
		verdict string
		wantOK  bool
	}{
		{"OK passes gate", "OK", true},
		{"TIGHT passes gate", "TIGHT", true},
		{"ALERT fails gate", "ALERT", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := GatePasses(c.verdict)
			if got != c.wantOK {
				t.Errorf("GatePasses(%q) = %v, want %v", c.verdict, got, c.wantOK)
			}
		})
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	os.Unsetenv("HERD_MEM_WARN_FREE_PCT")
	os.Unsetenv("HERD_SWAP_ALERT_MB")
	os.Unsetenv("HERD_RESOURCES_GATE")
}
