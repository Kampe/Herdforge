package security

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/envelope"
)

// TestLaunchAgent_SealWaitRequiresPreStartSeal_FailClosed: HERD_SEAL_WAIT without
// PreStartSeal must fail closed (no hang, no fallback success).
func TestLaunchAgent_SealWaitRequiresPreStartSeal_FailClosed(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	secret := "x"
	policy, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, secret, []string{"pkg/security"})
	if err != nil {
		t.Fatal(err)
	}
	eventLog := filepath.Join(wt, "ev.jsonl")
	_ = BindDurableEvents(policy, eventLog, &MemorySink{})
	st := StructureTask("FAC-X", "t", "d", RoleWorker, wt, "", "", false)
	grant, err := policy.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file"}, Structured: st,
		Env: map[string]string{"PATH": "/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	restore := SetTestClaimLookup(MapClaimLookup{
		"FAC-X": {TaskRef: "FAC-X", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)},
	})
	defer restore()
	sp := &seqSpawner{session: "ses_real_live_abc"}
	_, err = LaunchAgent(sp, AgentSpawnRequest{
		Policy: policy, Grant: grant, Name: "n", Kind: "true",
		Workspace: "w", Ambient: map[string]string{"HERD_SEAL_WAIT": "1", "PATH": "/usr/bin:/bin"},
		EventLogPath: eventLog, TaskRef: "FAC-X", LeaseGeneration: "1",
		ClaimLookup:     MapClaimLookup{"FAC-X": {TaskRef: "FAC-X", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}},
		SessionResolver: sp, SkipContainment: true,
		// PreStartSeal intentionally nil
	})
	if err == nil {
		t.Fatal("HERD_SEAL_WAIT without PreStartSeal must fail closed")
	}
	if !strings.Contains(err.Error(), "PreStartSeal") && !strings.Contains(err.Error(), "deadlock") {
		t.Fatalf("want deadlock-prevention error, got %v", err)
	}
}

// TestLaunchAgent_PackagesAppliedBeforeStart proves exclusive packages are on
// the policy/grant before StartAgent (kernel profile Install path).
func TestLaunchAgent_PackagesAppliedBeforeStart(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(filepath.Join(wt, "pkg", "security"), 0o755)
	secret := "pkg-before-start"
	policy, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, secret, []string{"pkg/security"})
	if err != nil {
		t.Fatal(err)
	}
	if !policy.ExclusivePackages || len(policy.PackageAllowlist) != 1 {
		t.Fatal("packages must be exclusive before launch")
	}
	eventLog := filepath.Join(wt, "ev.jsonl")
	_ = BindDurableEvents(policy, eventLog, &MemorySink{})
	st := StructureTask("FAC-PKG", "t", "d", RoleWorker, wt, "", "probe", false)
	grant, err := policy.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file"}, Structured: st,
		Env: map[string]string{"PATH": "/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(grant.PackageRoots) != 1 || grant.PackageRoots[0] != "pkg/security" {
		t.Fatalf("grant packages: %+v", grant.PackageRoots)
	}
	restore := SetTestClaimLookup(MapClaimLookup{
		"FAC-PKG": {TaskRef: "FAC-PKG", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)},
	})
	defer restore()

	var sawExclusive atomic.Bool
	sp := &seqSpawner{
		session: "ses_019fc450_live_pkg",
		onStart: func() {
			if policy.ExclusivePackages && len(policy.PackageAllowlist) == 1 {
				sawExclusive.Store(true)
			}
		},
	}
	res, err := LaunchAgent(sp, AgentSpawnRequest{
		Policy: policy, Grant: grant, Name: "pkg-agent", Kind: "true",
		Workspace: "w-test", Ambient: map[string]string{"PATH": "/usr/bin:/bin"},
		EventLogPath: eventLog, TaskRef: "FAC-PKG", LeaseGeneration: "1",
		ClaimLookup:     MapClaimLookup{"FAC-PKG": {TaskRef: "FAC-PKG", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}},
		SessionResolver: sp, SkipContainment: true, ControlSecret: secret,
	})
	if err != nil {
		t.Fatalf("LaunchAgent: %v", err)
	}
	if !sawExclusive.Load() {
		t.Fatal("exclusive packages must be set before StartAgent")
	}
	if err := RefuseProvisionalWorkerSession(res.AgentSessionID); err != nil {
		t.Fatalf("session must be non-provisional: %v", err)
	}
	if res.AgentSessionID != "ses_019fc450_live_pkg" {
		t.Fatalf("want real session, got %q", res.AgentSessionID)
	}
}

// TestRefuseProvisionalWorkerSession covers pane/term/pending.
func TestRefuseProvisionalWorkerSession(t *testing.T) {
	for _, w := range []string{"", "pending-x", "herdr-pane:p1", "herdr-term:t1", "ses_probe_1", "ses_real_1", "test-session-x"} {
		if err := RefuseProvisionalWorkerSession(w); err == nil {
			t.Errorf("%q must be refused", w)
		}
	}
	if err := RefuseProvisionalWorkerSession("ses_03cada7f7ffe0a0iK2e9LvZ1eT"); err != nil {
		t.Fatal(err)
	}
}

// TestStartBarrier_StaleBarrierNotReplayed ensures ClearStaleBarriers + versioned paths.
func TestStartBarrier_StaleBarrierNotReplayed(t *testing.T) {
	shared := t.TempDir()
	secret := "stale-barrier"
	iss, err := envelope.NewIssuer(secret, envelope.RoleCoordinator, "c")
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := iss.Issue(envelope.IssueOpts{
		Kind: envelope.KindScopeCorrection, TargetTask: "T",
		LeaseGeneration: 3, TargetWorkerSession: "ses_live_old",
		Body: "old", Scope: &envelope.Scope{PackageAllowlist: []string{"pkg/a"}, Exclusive: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	st := &EnforcedControlState{Control: ctrl, Task: "T", WorkerSession: "ses_live_old", LeaseGeneration: 3, EnvelopeID: ctrl.ID}
	oldPath := SealedControlBarrierPath(shared, "T", 3, "v1")
	if err := WriteSealedControlTo(oldPath, st); err != nil {
		t.Fatal(err)
	}
	if err := ClearStaleBarriers(shared, "T", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("stale barrier must be gone")
	}
	newPath := SealedControlBarrierPath(shared, "T", 3, "v2")
	if oldPath == newPath {
		t.Fatal("versioned paths must differ")
	}
	if err := WorkerVerifySealed(oldPath, secret, "T", "ses_live_old", 3); err == nil {
		t.Fatal("missing stale barrier must not verify")
	}
}

// TestWriteControlMACSecret_DurableReadback proves flock/fsync/readback path.
func TestWriteControlMACSecret_DurableReadback(t *testing.T) {
	shared := t.TempDir()
	if err := WriteControlMACSecret(shared, "super-secret-mac"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadControlMACSecret(shared)
	if err != nil || got != "super-secret-mac" {
		t.Fatalf("readback: %q %v", got, err)
	}
}

// TestValidatePackageAllowlist_Traversal refuses unsafe roots.
func TestValidatePackageAllowlist_Traversal(t *testing.T) {
	if err := ValidatePackageAllowlist([]string{"../etc"}); err == nil {
		t.Fatal("traversal must fail")
	}
	if err := ValidatePackageAllowlist([]string{"/abs/path"}); err == nil {
		t.Fatal("absolute must fail")
	}
	if err := ValidatePackageAllowlist(nil); err == nil {
		t.Fatal("empty must fail")
	}
	if _, err := NormalizePackageAllowlist([]string{"pkg/security", "cmd/herd"}); err != nil {
		t.Fatal(err)
	}
}

// seqSpawner returns a distinct real-looking session — never pane-as-session.
type seqSpawner struct {
	onStart func()
	tab     string
	pane    string
	session string
	mu      sync.Mutex
}

func (b *seqSpawner) CreateTab(workspace, label, cwd string, env []string, noFocus bool) (string, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tab = "tab-1"
	b.pane = "pane-1"
	return b.tab, b.pane, nil
}

func (b *seqSpawner) StartAgent(name, kind, paneID string, agentArgs []string) error {
	if b.onStart != nil {
		b.onStart()
	}
	return nil
}

func (b *seqSpawner) CloseTab(tabID string) error { return nil }

func (b *seqSpawner) Lookup(name string) (*LiveAgentIdentity, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sid := b.session
	if sid == "" {
		sid = "ses_live_default"
	}
	// Session is NEVER equal to pane id (regression guard).
	if sid == b.pane || strings.HasPrefix(sid, "herdr-pane:") {
		return nil, os.ErrInvalid
	}
	return &LiveAgentIdentity{
		Name: name, TabID: b.tab, PaneID: b.pane,
		AgentSessionID: sid,
		TerminalID:     "term-1",
	}, nil
}
