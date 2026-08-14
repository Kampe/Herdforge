package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validUsableResult(kind string) HarnessProbeResult {
	return HarnessProbeResult{
		Kind: kind, Usable: true, VersionOK: true, ToolOK: true, ModelOK: true,
		PostParentAlive: true, ViaLaunchAgent: true, Contained: true, RealHerdrSession: true,
		ToolEvidence: "herdr_session=ses_live tool_sentinel=ok",
	}
}

func TestConsumeFleetAttestation_FailClosedMissing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_READINESS_ROOT", root)
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_CONTROL_SECRET", "attest-secret")
	t.Setenv("HERD_LIVE_HARNESS_PROOF", "")
	t.Setenv("HERD_REFRESH_READINESS", "")
	fr, err := EvaluateFleetReadiness()
	if err == nil {
		t.Fatal("missing attestation must BLOCK")
	}
	if fr == nil || !fr.Blocked {
		t.Fatalf("%+v", fr)
	}
}

func TestFleetAttestation_MACRequired(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_READINESS_ROOT", root)
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_CONTROL_SECRET", "attest-secret")
	// Unsigned attestation must not consume.
	a := &FleetAttestation{
		Version: fleetAttestVersion, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour),
		Generation: "g", Usable: 1, ContainmentBackend: "sandbox-exec",
		HerdBinaryDigest: "x", PolicyDigest: "y",
		Results: []HarnessProbeResult{validUsableResult("grok")},
	}
	path := FleetAttestationPath(root)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	raw, _ := json.MarshalIndent(a, "", "  ")
	_ = os.WriteFile(path, raw, 0o600)
	if _, err := ConsumeFleetAttestation(root); err == nil {
		t.Fatal("unsigned attestation must fail")
	}
}

func TestFleetAttestation_ForgedUsableRejected(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_READINESS_ROOT", root)
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_CONTROL_SECRET", "attest-secret")
	// Build then tamper Usable upward without re-sign.
	// Skip if no containment.
	if _, _, c, err := CurrentReadinessBinding(); err != nil || c == "unavailable" {
		t.Skip("containment required for full binding test")
	}
	// Manually craft with Usable=1 but empty results after sign.
	a, err := BuildFleetAttestationFromResults([]HarnessProbeResult{validUsableResult("grok")}, time.Hour)
	if err != nil {
		// May fail if digests for grok missing in CI without binary — still test forge path.
		t.Skipf("build: %v", err)
	}
	a.Usable = 99 // forge without re-sign
	if err := ValidateFleetAttestation(a, time.Now().UTC(), "attest-secret"); err == nil {
		t.Fatal("forged usable count must fail")
	}
}

func TestEvaluateFleetReadiness_NoDoubleLiveSpawn(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_READINESS_ROOT", root)
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_CONTROL_SECRET", "attest-secret")
	t.Setenv("HERD_LIVE_HARNESS_PROOF", "")
	fr1, err1 := EvaluateFleetReadiness()
	if err1 == nil {
		t.Fatal("expected block")
	}
	fr2, err2 := EvaluateFleetReadiness()
	if err2 == nil {
		t.Fatal("expected block")
	}
	if fr1.Reason != fr2.Reason {
		t.Fatalf("reasons should be stable without live: %q vs %q", fr1.Reason, fr2.Reason)
	}
	if _, err := os.Stat(FleetAttestationPath(root)); !os.IsNotExist(err) {
		t.Fatal("must not write attestation without live refresh flag")
	}
}

func TestRequireFleetReady_UsesAttestationOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_READINESS_ROOT", root)
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_CONTROL_SECRET", "attest-secret")
	t.Setenv("HERD_LIVE_HARNESS_PROOF", "")
	if err := RequireFleetReady(); err == nil {
		t.Fatal("no attestation → not ready")
	}
	restore := SetReadinessOverrideForTest(&FleetReadiness{Usable: 1, Blocked: false, Reason: "test"})
	defer restore()
	if err := RequireFleetReady(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildFleetAttestation_RefusesZeroUsable(t *testing.T) {
	t.Setenv("HERD_CONTROL_SECRET", "attest-secret")
	_, err := BuildFleetAttestationFromResults(nil, time.Hour)
	if err == nil {
		t.Fatal("zero usable must refuse attestation write")
	}
}

func TestTrustedReadinessRoot_Traversal(t *testing.T) {
	if _, err := TrustedReadinessRoot("../escape"); err == nil {
		t.Fatal("traversal must fail")
	}
}

func TestFleetAttestationPath_RepoRelative(t *testing.T) {
	p := FleetAttestationPath("shared-root")
	if !strings.Contains(p, filepath.Join(".herd", "readiness")) {
		t.Fatalf("%s", p)
	}
}

func TestBuildFleetAttestation_WithConfiguredKinds(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_READINESS_ROOT", root)
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_CONTROL_SECRET", "attest-secret")

	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(binDir, "agy")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho 1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	if _, _, c, err := CurrentReadinessBindingFor([]string{"agy"}); err != nil || c == "unavailable" {
		t.Skip("containment required for full binding test")
	}

	a, err := BuildFleetAttestationFromResultsFor([]HarnessProbeResult{validUsableResult("agy")}, time.Hour, []string{"agy"})
	if err != nil {
		t.Fatalf("BuildFleetAttestationFromResultsFor: %v", err)
	}
	if a.HarnessDigests["agy"] == "" {
		t.Fatal("expected non-empty digest for agy")
	}
	if err := ValidateFleetAttestation(a, time.Now().UTC(), "attest-secret"); err != nil {
		t.Fatalf("ValidateFleetAttestation: %v", err)
	}
}
