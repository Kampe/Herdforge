package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbeHarnessSurvival_EmptyKindFailClosed(t *testing.T) {
	r, err := ProbeHarnessSurvival("")
	if err == nil {
		t.Fatal("empty kind must fail closed")
	}
	if r == nil || !strings.Contains(r.TicketScopedBlocker, "FAC-133") {
		t.Fatalf("want FAC-133 blocker, got %+v", r)
	}
	if r.Usable {
		t.Fatal("must not be usable")
	}
}

func TestProbeAll_HonestBlockedWithoutLiveHerdr(t *testing.T) {
	// Default: no HERD_LIVE_HARNESS_PROOF → zero usable (honest).
	_ = os.Unsetenv("HERD_LIVE_HARNESS_PROOF")
	results, err := ProbeAllSupportedHarnesses()
	if err == nil {
		t.Fatal("expected BLOCKED without live Herdr proof")
	}
	for _, r := range results {
		t.Logf("%s usable=%v herdr=%v blocker=%q", r.Kind, r.Usable, r.RealHerdrSession, r.TicketScopedBlocker)
		if r.Usable {
			t.Fatalf("%s must not be usable without live Herdr session", r.Kind)
		}
		if err := AssertNotSyntheticallyUsable(&r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProbeHarnessSurvival_NotUsableSynthetically(t *testing.T) {
	_ = os.Unsetenv("HERD_LIVE_HARNESS_PROOF")
	r, err := ProbeHarnessSurvival("claude")
	if r == nil {
		t.Fatal("nil")
	}
	if r.Usable || r.RealHerdrSession {
		t.Fatal("must not mark usable/herdr without live proof")
	}
	if err == nil {
		t.Fatal("want error when not usable")
	}
	if err := AssertNotSyntheticallyUsable(r); err != nil {
		t.Fatal(err)
	}
}

func TestMutation_SyntheticSpawnerCannotMarkUsable(t *testing.T) {
	// Even if a test injects a fake live proof, evidence must be RealHerdrSession.
	r := &HarnessProbeResult{Kind: "claude", Usable: true, RealHerdrSession: false}
	if err := AssertNotSyntheticallyUsable(r); err == nil {
		t.Fatal("synthetic usable must fail AssertNotSyntheticallyUsable")
	}
	r.RealHerdrSession = true
	r.ToolEvidence = "parent-wrote-sentinel"
	if err := AssertNotSyntheticallyUsable(r); err == nil {
		t.Fatal("parent-written sentinel must fail")
	}
}

func TestRequireFleetReady_Wired(t *testing.T) {
	restore := SetReadinessOverrideForTest(&FleetReadiness{Blocked: true, Reason: "test block", Usable: 0})
	defer restore()
	if err := RequireFleetReady(); err == nil {
		t.Fatal("override blocked must fail RequireFleetReady")
	}
}

func TestHostCreds_NotInAgentEnv(t *testing.T) {
	b, err := StartHostAllowBroker([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	secret := "Bearer never-in-builder"
	b.SetHostCredential("127.0.0.1", secret)
	if b.hostCred("127.0.0.1") != secret {
		t.Fatal("host cred not stored")
	}
	if strings.Contains(b.ProxyURL(), "never-in-builder") {
		t.Fatal("proxy URL leaked coordinator secret")
	}
}

func TestProcessTreeTimeout_KillsGroup(t *testing.T) {
	if err := proveProcessTreeTimeoutKill(t.TempDir(), []string{"PATH=/usr/bin:/bin"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateTaskRefAndLease(t *testing.T) {
	if err := ValidateTaskRef(""); err == nil {
		t.Fatal("empty")
	}
	if err := ValidateTaskRef("cli-launch"); err == nil {
		t.Fatal("cli-launch")
	}
	if err := ValidateLiveTaskLease(t.Context(), MapClaimLookup{
		"FAC-1": {TaskRef: "FAC-1", Generation: 3, ExpiresAt: time.Now().Add(time.Hour)},
	}, "FAC-1", "99", false, "", ""); err == nil {
		t.Fatal("wrong gen")
	}
	if err := ValidateLiveTaskLease(t.Context(), nil, "FAC-1", "3", false, "", ""); err == nil {
		t.Fatal("nil lookup must fail")
	}
	_ = os.WriteFile(filepath.Join(t.TempDir(), "x"), []byte("1"), 0o600)
}

func TestParentDeath_RealLauncherExit(t *testing.T) {
	ok, ev, err := proveParentDeathBrokerSurvival(t.TempDir())
	if err != nil || !ok {
		t.Fatalf("parent death proof: ok=%v err=%v ev=%s", ok, err, ev)
	}
}

func TestStopViaControlSibling_PropagatesFailures(t *testing.T) {
	if err := stopViaControlSibling(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctrl := filepath.Join(dir, "x.ctrl.json")
	if err := os.WriteFile(ctrl, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stopViaControlSibling(filepath.Join(dir, "x.json")); err == nil {
		t.Fatal("corrupt control must fail")
	}
}

func TestClaimAuthority_NoAssumedClaimsDB(t *testing.T) {
	restore := SetTestClaimLookup(nil)
	defer restore()
	ClearClaimAuthority()
	if ResolveClaimLookup() != nil {
		t.Fatal("must not open assumed claims.db")
	}
	path := CanonicalLeaseDBPath(t.TempDir())
	if !strings.Contains(path, filepath.Join(".herd", "claim", "leases.db")) {
		t.Fatalf("want claim/leases.db, got %s", path)
	}
}
