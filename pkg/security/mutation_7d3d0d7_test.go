package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Mutation tests that fail against the 7d3d0d7 candidate behaviors.

func TestMutation_ViaLaunchAgentNotVacuous(t *testing.T) {
	_ = os.Unsetenv("HERD_LIVE_HARNESS_PROOF")
	r, _ := ProbeHarnessSurvival("claude")
	if r != nil && r.Usable && !r.RealHerdrSession {
		t.Fatal("usable without RealHerdrSession is vacuous")
	}
	if err := AssertNotSyntheticallyUsable(r); err != nil {
		t.Fatal(err)
	}
	_ = strings.TrimSpace("")
}

func TestMutation_ReadinessHasProductionCaller(t *testing.T) {
	// 7d3d0d7: ProbeAll only called from unit tests.
	// RequireFleetReady must exist and evaluate ProbeAll.
	restore := SetReadinessOverrideForTest(&FleetReadiness{Blocked: true, Reason: "mutation-block", Usable: 0})
	defer restore()
	if err := RequireFleetReady(); err == nil {
		t.Fatal("RequireFleetReady must fail when blocked")
	}
	if !strings.Contains(errStringFleet(RequireFleetReady()), "BLOCKED") && RequireFleetReady() == nil {
		t.Fatal("want blocked")
	}
}

func errStringFleet(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestMutation_StopViaControlSiblingNoSilentSuccess(t *testing.T) {
	// 7d3d0d7: stopViaControlSibling ignored shutdown failures.
	dir := t.TempDir()
	// Alive PID with bad control URL must not succeed.
	ctrl := BrokerControlState{
		ControlToken: "x",
		ControlAddr:  "10.0.0.1:9", // non-loopback — validate fails
		ControlURL:   "http://10.0.0.1:9/__herd_control",
		Identity:     "id",
		PID:          1,
	}
	path := BrokerControlPath(dir, "orphan")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteBrokerControlState(path, &ctrl); err != nil {
		t.Fatal(err)
	}
	if err := stopViaControlSibling(BrokerStatePath(dir, "orphan")); err == nil {
		t.Fatal("non-loopback control must fail closed")
	}
}
