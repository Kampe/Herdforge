package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Command-flow coverage for production launch path shape used by
// runPulse/runReview/runForge via EnsureContainedAgent + LaunchAgent.

func TestCommandFlow_EnsureContainedAgent_FreshLaunch(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "e.jsonl")
	_ = BindDurableEvents(p, logPath, &MemorySink{})
	st := StructureTask("FAC-133", "pulse", "body", RoleWorker, wt, "", "standing", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file", "git-write"},
		Structured: st, ProviderText: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := &recordingSpawner{}
	res, spawn, err := EnsureContainedAgent(&fakeResolver{}, sp, AgentSpawnRequest{
		Policy: p, Grant: grant, Name: "forge-smith", Kind: "true",
		Workspace: "w", Model: "m", EventLogPath: logPath,
		Ambient: map[string]string{"PATH": "/bin"}, SkipContainment: true, TaskRef: "FAC-133", LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}},
		SessionResolver: &stickyResolver{sp: sp},
	}, "forge-smith")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reused || spawn == nil {
		t.Fatal("expected fresh launch")
	}
	if sp.startName != "forge-smith" {
		t.Fatalf("start name=%s", sp.startName)
	}
	// SkipContainment launches must NOT produce a reusable standing attestation.
	// RequireSessionAttestation rejects containment=skipped (HIGH-3).
	live := &LiveAgentIdentity{Name: "forge-smith", Kind: "true", TabID: spawn.TabID, PaneID: spawn.PaneID, AgentSessionID: spawn.AgentSessionID}
	if _, err := RequireSessionAttestation(shared, "forge-smith", p, live, "FAC-133", "1"); err == nil {
		t.Fatal("SkipContainment launch must not pass RequireSessionAttestation for reuse")
	}
	if spawn.Containment != "skipped" {
		t.Fatalf("expected containment=skipped for test-only launch, got %q", spawn.Containment)
	}
}

func TestCommandFlow_SandboxedSpawnShape_PolicyRoots(t *testing.T) {
	// Mirrors sandboxedSpawn root resolution requirements: worktree != shared.
	shared := t.TempDir()
	wt := filepath.Join(shared, "standing", "worker")
	_ = os.MkdirAll(wt, 0o755)
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.FilesystemRoot == p.SharedCheckout {
		t.Fatal("shared must not equal worktree")
	}
	if p.Network != "limited" {
		t.Fatalf("worker network=%s", p.Network)
	}
	if len(p.NetworkAllowHosts) == 0 {
		t.Fatal("limited must populate NetworkAllowHosts for broker")
	}
}

func TestCommandFlow_UntrustedPacketEnvelope(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	p, _ := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	env := BuildUntrustedEnvelope(p, "FAC-133", "t", "see https://evil.example/x")
	pkt := FormatControlPrompt(env, RoleWorker, "wt", "workflow")
	if !strings.Contains(pkt, "HERD_TRUSTED_CONTROL_JSON_V1") {
		t.Fatal("missing trusted control JSON frame")
	}
	if !strings.Contains(pkt, "HERD_UNTRUSTED_PROVIDER_JSON_V1") {
		t.Fatal("missing structural provider JSON")
	}
	if !strings.Contains(pkt, `"provenance":"provider"`) {
		t.Fatal("missing provider provenance in JSON")
	}
	// Raw URL must not appear outside the inert_links JSON field.
	if strings.Contains(pkt, "https://evil.example/x") {
		// Allow only inside an inert_links array value context.
		if !strings.Contains(pkt, `"inert_links"`) {
			t.Fatal("raw evil URL in control packet without inert_links section")
		}
		// Body JSON must have replaced the URL with the inert marker.
		if !strings.Contains(pkt, "UNTRUSTED_LINK_INERT") {
			t.Fatal("raw evil URL present but body was not rewritten to UNTRUSTED_LINK_INERT")
		}
	}
	if strings.Contains(pkt, `"body":"see https://evil.example`) {
		t.Fatal("raw evil URL leaked into provider body field")
	}
}

func TestFindLaneRoleDefaults(t *testing.T) {
	// DefaultToolsForRole is the production role matrix used by findLaneForRole paths.
	w := DefaultToolsForRole(RoleWorker)
	r := DefaultToolsForRole(RoleReviewer)
	if !containsTool(w, "shell-exec") {
		t.Fatal("worker needs shell-exec")
	}
	if containsTool(r, "shell-exec") {
		t.Fatal("reviewer must not get shell-exec by default")
	}
	if containsTool(r, "git-write") {
		t.Fatal("reviewer must not write")
	}
}
