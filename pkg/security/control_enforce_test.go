package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/envelope"
)

func TestVerifyAndEnforceControl_MachineBoundary(t *testing.T) {
	secret := "enforce-secret"
	iss, err := envelope.NewIssuer(secret, envelope.RoleCoordinator, "coord")
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := iss.Issue(envelope.IssueOpts{
		Kind: envelope.KindScopeCorrection, TargetTask: "FAC-133",
		LeaseGeneration: 1, TargetWorkerSession: "ses_w",
		Body: "bind", Scope: &envelope.Scope{
			PackageAllowlist: []string{"pkg/envelope"},
			Exclusive:        true,
			Note:             "launch",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	shared := filepath.Join(wt, "shared")
	// worktree is wt itself for policy
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, secret, nil)
	if err != nil {
		// shared may need to exist
		_ = err
		p, err = PolicyForLane(RoleWorker, wt, wt, "herdforge", []string{"herdforge"}, secret, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	st := StructureTask("FAC-133", "t", "d", RoleWorker, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file"}, Structured: st,
		Env: map[string]string{"PATH": "/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sharedRoot := filepath.Join(t.TempDir(), "shared-root")
	_ = os.MkdirAll(sharedRoot, 0o755)
	enforced, err := VerifyAndEnforceControl(secret, ctrl, p, grant, wt, sharedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if enforced.EnvelopeID != ctrl.ID || enforced.Control == nil {
		t.Fatalf("%+v", enforced)
	}
	if !p.ExclusivePackages || len(p.PackageAllowlist) != 1 || p.PackageAllowlist[0] != "pkg/envelope" {
		t.Fatalf("policy not machine-enforced: %+v", p.PackageAllowlist)
	}
	// Load must re-verify MAC (no Verified:true trust).
	sealed := SealedControlPath(sharedRoot, ctrl.TargetTask, ctrl.TargetWorkerSession)
	loaded, err := LoadSealedControl(sealed, secret, "FAC-133", "ses_w", 1)
	if err != nil || loaded == nil {
		t.Fatalf("load: %v %+v", err, loaded)
	}
	// Tamper seal fails closed.
	_ = os.WriteFile(sealed, []byte(`{"control":{"sig":"sha256=00"}}`), 0o600)
	if _, err := LoadSealedControl(sealed, secret, "", "", 0); err == nil {
		t.Fatal("tampered seal must fail MAC re-verify")
	}
	// Exclusive empty allowlist refused.
	if err := ApplyControlScopeToPolicy(p, grant, &envelope.Scope{Exclusive: true}); err == nil {
		t.Fatal("exclusive empty PackageAllowlist must fail")
	}
	_ = time.Now()
}

func TestWorkerVerifySealed_RequiresExactBinding(t *testing.T) {
	secret := "verify-exact-secret"
	iss, err := envelope.NewIssuer(secret, envelope.RoleCoordinator, "coord")
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := iss.Issue(envelope.IssueOpts{
		Kind: envelope.KindScopeCorrection, TargetTask: "FAC-133",
		LeaseGeneration: 7, TargetWorkerSession: "ses_live_abc",
		Body: "bind", Scope: &envelope.Scope{
			PackageAllowlist: []string{"pkg/security"},
			Exclusive:        true,
			Note:             "exact",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	st := &EnforcedControlState{
		Control: ctrl, EnvelopeID: ctrl.ID, Sequence: ctrl.Sequence,
		Task: ctrl.TargetTask, WorkerSession: ctrl.TargetWorkerSession, LeaseGeneration: 7,
	}
	if err := WriteSealedControl(shared, st); err != nil {
		t.Fatal(err)
	}
	if err := WriteControlMACSecret(shared, secret); err != nil {
		t.Fatal(err)
	}
	path := SealedControlPath(shared, "FAC-133", "ses_live_abc")

	// Empty task/worker/lease must fail closed (never MAC-only accept).
	if err := WorkerVerifySealed(path, secret, "", "", 0); err == nil {
		t.Fatal("empty binding must fail")
	}
	if err := WorkerVerifySealed(path, secret, "FAC-133", "", 7); err == nil {
		t.Fatal("empty worker must fail")
	}
	if err := WorkerVerifySealed(path, secret, "FAC-133", "ses_live_abc", 0); err == nil {
		t.Fatal("zero lease must fail")
	}
	// Wrong bindings.
	if err := WorkerVerifySealed(path, secret, "OTHER", "ses_live_abc", 7); err == nil {
		t.Fatal("wrong task must fail")
	}
	if err := WorkerVerifySealed(path, secret, "FAC-133", "ses_other", 7); err == nil {
		t.Fatal("wrong worker must fail")
	}
	if err := WorkerVerifySealed(path, secret, "FAC-133", "ses_live_abc", 99); err == nil {
		t.Fatal("wrong lease must fail")
	}
	// Provisional expected worker refused.
	if err := WorkerVerifySealed(path, secret, "FAC-133", "pending-FAC-133-7", 7); err == nil {
		t.Fatal("provisional expected worker must fail")
	}
	// Exact match OK.
	if err := WorkerVerifySealed(path, secret, "FAC-133", "ses_live_abc", 7); err != nil {
		t.Fatalf("exact: %v", err)
	}
	// File helper with env fallbacks.
	t.Setenv("HERD_EXPECTED_TASK", "FAC-133")
	t.Setenv("HERD_EXPECTED_WORKER", "ses_live_abc")
	t.Setenv("HERD_EXPECTED_LEASE", "7")
	if err := WorkerVerifySealedFile(path, "", "", 0); err != nil {
		t.Fatalf("file+env: %v", err)
	}
}

func TestWriteSealedControl_RefusesProvisionalAndDurable(t *testing.T) {
	secret := "seal-dur"
	iss, err := envelope.NewIssuer(secret, envelope.RoleCoordinator, "coord")
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := iss.Issue(envelope.IssueOpts{
		Kind: envelope.KindScopeCorrection, TargetTask: "T",
		LeaseGeneration: 1, TargetWorkerSession: "pending-T-1",
		Body: "x", Scope: &envelope.Scope{PackageAllowlist: []string{"pkg/a"}, Exclusive: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	st := &EnforcedControlState{Control: ctrl, Task: "T", WorkerSession: "pending-T-1", LeaseGeneration: 1}
	if err := WriteSealedControlTo(filepath.Join(t.TempDir(), "s.json"), st); err == nil {
		t.Fatal("provisional pending-* must not seal")
	}
	// Live session seals with unique tmp + fsync path (no error).
	ctrl2, err := iss.Issue(envelope.IssueOpts{
		Kind: envelope.KindScopeCorrection, TargetTask: "T",
		LeaseGeneration: 1, TargetWorkerSession: "ses_real",
		Body: "x", Scope: &envelope.Scope{PackageAllowlist: []string{"pkg/a"}, Exclusive: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	st2 := &EnforcedControlState{Control: ctrl2, Task: "T", WorkerSession: "ses_real", LeaseGeneration: 1}
	path := filepath.Join(t.TempDir(), "live.json")
	if err := WriteSealedControlTo(path, st2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// Barrier path also works (versioned).
	shared := t.TempDir()
	st2.Control = ctrl2
	if err := WriteSealedControlTo(SealedControlBarrierPath(shared, "T", 1, "nonce1"), st2); err != nil {
		t.Fatal(err)
	}
	// Stale clear removes prior versioned barriers.
	_ = WriteSealedControlTo(SealedControlBarrierPath(shared, "T", 1, "old"), st2)
	if err := ClearStaleBarriers(shared, "T", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SealedControlBarrierPath(shared, "T", 1, "old")); !os.IsNotExist(err) {
		t.Fatal("stale barrier must be removed")
	}
}

func TestUpsertEnvFileKeys_PublishesWorker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env.list")
	if err := WriteEnvFile(path, []string{"HERD_EXPECTED_TASK=FAC-133", "HERD_SEAL_WAIT=1"}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEnvFileKeys(path, map[string]string{"HERD_EXPECTED_WORKER": "ses_xyz"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "HERD_EXPECTED_WORKER=ses_xyz") || !strings.Contains(s, "HERD_EXPECTED_TASK=FAC-133") {
		t.Fatalf("upsert lost keys: %s", s)
	}
}

func TestLiveWorkerBinding_SessionOptional(t *testing.T) {
	if _, err := LiveWorkerBinding("a", "t", "p", "ses_spawn_x"); err == nil {
		t.Fatal("ses_spawn must be refused")
	}
	id, err := LiveWorkerBinding("task-fac-x", "wF:t1", "wF:p1", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "live-agent:task-fac-x|wF:t1|wF:p1" {
		t.Fatalf("got %s", id)
	}
	if err := RefuseProvisionalWorkerSession(id); err != nil {
		t.Fatal(err)
	}
	sid, err := LiveWorkerBinding("n", "t", "p", "ses_ok_real_abc")
	if err != nil || sid != "ses_ok_real_abc" {
		t.Fatalf("sid=%s err=%v", sid, err)
	}
}
