package confinement

import (
	"crypto/hmac"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func productionIdentity(t *testing.T, worktree, shared string) LaunchIdentity {
	t.Helper()
	return LaunchIdentity{
		Repository:        "github.com/Kampe/Herdforge",
		Task:              "FAC-190",
		LeaseGeneration:   7,
		Lane:              "smith",
		Session:           "session-fac-190",
		SessionGeneration: 1,
		HerdrTab:          "tab-fac-190",
		HerdrPane:         "pane-fac-190",
		ProcessIdentity:   "process-fac-190",
		Argv:              []string{"codex", "--model", "gpt-5.6-luna"},
		WorktreeRoot:      worktree,
		SharedRoot:        shared,
		AgentKind:         "codex",
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(dir string, args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s (%v)", args, out, err)
		}
	}
	run(dir, "git", "init")
	run(dir, "git", "config", "user.email", "fac190@test.local")
	run(dir, "git", "config", "user.name", "fac190")
	run(dir, "git", "commit", "--allow-empty", "-m", "init")
}

// linkedWorktreeFixture mirrors production: shared repo root + task worktree
// under a sibling path so common-dir is outside the worktree.
func linkedWorktreeFixture(t *testing.T) (shared, worktree string) {
	t.Helper()
	shared = t.TempDir()
	repo := filepath.Join(shared, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, repo)
	worktree = filepath.Join(shared, "task-wt")
	cmd := exec.Command("git", "worktree", "add", worktree, "-b", "task/fac-190-test", "HEAD")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %s (%v)", out, err)
	}
	return shared, worktree
}

func TestHMACIssuerSignsAndRejectsForgery(t *testing.T) {
	issuer, err := NewHMACIssuer([]byte("fac-190-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	root := filepath.Join(shared, "wt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel, err := InstallSentinel(root)
	if err != nil {
		t.Fatal(err)
	}
	tuple := fixtureTuple(root)
	proof, err := issuer.Issue(root, sentinel, tuple)
	if err != nil || len(proof.MAC) == 0 || proof.Nonce == "" {
		t.Fatalf("issue: %+v %v", proof, err)
	}
	if err := issuer.Verify(root, sentinel, tuple, proof); err != nil {
		t.Fatal(err)
	}
	bad := proof
	bad.MAC = append([]byte{}, proof.MAC...)
	bad.MAC[0] ^= 0xff
	if err := issuer.Verify(root, sentinel, tuple, bad); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("forged MAC: %v", err)
	}
	other := tuple
	other.Task = "FAC-999"
	if err := issuer.Verify(root, sentinel, other, proof); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("cross-task: %v", err)
	}
}

func TestIssuerFromEnvFailClosed(t *testing.T) {
	t.Setenv(SecretEnv, "")
	t.Setenv(SecretEnvFallback, "")
	if _, err := IssuerFromEnv(); !errors.Is(err, ErrMissingSecret) {
		t.Fatalf("want ErrMissingSecret, got %v", err)
	}
	t.Setenv(SecretEnv, "from-primary")
	if _, err := IssuerFromEnv(); err != nil {
		t.Fatal(err)
	}
}

func TestBindAndProvePolicyDeniesIncidentPath(t *testing.T) {
	issuer, err := NewHMACIssuer([]byte("fac-190-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	root := filepath.Join(shared, "task-wt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &FakeOS{}
	enf := &Enforcer{Issuer: issuer, OS: fake, ReceiptDir: filepath.Join(root, ".herd", "receipts")}
	prep, err := enf.PrepareOS(root, shared, "FAC-190", 7, "task/fac-190", "codex", "codex")
	if err != nil {
		t.Fatal(err)
	}
	// Integrity store must live outside the worktree.
	if prep.Session.Root == "" || isPathPrefix(prep.Session.Root, root) {
		t.Fatalf("session inside worktree: %+v", prep.Session)
	}
	// Bind must not create FAC-188 residual under shared.
	binding, err := enf.BindAndProve(productionIdentity(t, root, shared), prep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shared, filepath.FromSlash(SharedRootIncidentRel))); err == nil {
		t.Fatal("Bind created residual under shared")
	}
	if !binding.OSProved || !binding.AgentWrapped || binding.ProfileDigest == "" || binding.ReceiptMACHex == "" {
		t.Fatalf("incomplete binding: %+v", binding)
	}
	if err := binding.VerifyReceiptMAC(issuer); err != nil {
		t.Fatal(err)
	}
	// Forged receipt fails MAC
	forged := *binding
	forged.AgentWrapped = false
	forged.ReceiptDigest, _ = forged.digest()
	if err := forged.VerifyReceiptMAC(issuer); err == nil {
		t.Fatal("forged receipt MAC accepted")
	}
	incident := filepath.Join(shared, filepath.FromSlash(SharedRootIncidentRel))
	if err := binding.Boundary.AuthorizeWrite(binding.Capability, incident); err == nil {
		t.Fatal("policy accepted shared-root incident path")
	}
	// Shared-root observation drift: create incident path
	if err := os.MkdirAll(filepath.Dir(incident), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(incident, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := binding.CheckSharedRoot(); err == nil {
		t.Fatal("incident path under shared accepted")
	}
}

func TestBindRejectsMissingIdentity(t *testing.T) {
	issuer, err := NewHMACIssuer([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	id := productionIdentity(t, root, "")
	id.LeaseGeneration = 0
	if _, err := Bind(id, issuer); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("want unauthenticated, got %v", err)
	}
}

func TestBindAndProveRequiresPreparedOS(t *testing.T) {
	issuer, err := NewHMACIssuer([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	enf := &Enforcer{Issuer: issuer, OS: &FakeOS{}}
	if _, err := enf.BindAndProve(productionIdentity(t, root, ""), nil); err == nil {
		t.Fatal("expected fail closed without PreparedOS")
	}
}

func TestDarwinSeatbeltProveWriteDenials(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	if _, err := RequireOS(); err != nil {
		t.Skip(err.Error())
	}
	shared, root := linkedWorktreeFixture(t)
	session, err := NewSessionPaths(shared, "FAC-190", 7)
	if err != nil {
		t.Fatal(err)
	}
	osb := DarwinSeatbelt{}
	profile, err := osb.Prepare(root, shared, "task/fac-190", session)
	if err != nil {
		t.Fatal(err)
	}
	// Session integrity store is under shared/.herd/confine-sessions (allowed);
	// residual .herd/FAC-188 must not appear.
	if _, err := os.Stat(filepath.Join(shared, filepath.FromSlash(SharedRootIncidentRel))); err == nil {
		t.Fatal("Prepare created residual under shared")
	}
	if err := osb.ProveWriteDenials(root, shared, profile, session); err != nil {
		t.Fatal(err)
	}
	// Confined rewrite of session profile must not stick (re-read digest).
	d1, _ := ProfileDigest(profile)
	_ = osb.writeUnder(profile, root, profile, "(allow default)\n")
	d2, _ := ProfileDigest(profile)
	if d1 != d2 {
		t.Fatal("session profile was rewritten by confined write")
	}
}

func TestDarwinFirstMatchProfileDeniesSharedParent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	if _, err := RequireOS(); err != nil {
		t.Skip(err.Error())
	}
	shared, root := linkedWorktreeFixture(t)
	session, err := NewSessionPaths(shared, "FAC-190", 7)
	if err != nil {
		t.Fatal(err)
	}
	osb := DarwinSeatbelt{}
	profile, err := osb.Prepare(root, shared, "task/fac-190", session)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	absShared, err := realPath(shared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "(allow file-write* (subpath \""+absShared+"\"))") {
		t.Fatal("profile grants shared parent writes")
	}
	if !strings.Contains(body, "(allow network*)") {
		t.Fatal("profile missing network grant required for agents")
	}
	if !strings.Contains(body, "objects") {
		t.Fatal("profile missing common objects grant")
	}
	outside := filepath.Join(shared, "outside-shared.txt")
	if err := osb.writeUnder(profile, root, outside, "nope\n"); err == nil {
		if _, statErr := os.Stat(outside); statErr == nil {
			t.Fatal("shared-parent write succeeded")
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("outside inode under shared parent")
	}
	if err := osb.proveGitObjectWrite(profile, root); err != nil {
		t.Fatalf("git object write: %v", err)
	}
	gitDir, err := absoluteGitDir(root)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(gitDir, "fac190-test.lock")
	if err := osb.writeUnder(profile, root, lock, "ok\n"); err != nil {
		t.Fatalf("gitdir write denied: %v", err)
	}
	_ = os.Remove(lock)
	common, err := absoluteGitCommonDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if isPathPrefix(common, root) {
		t.Fatal("fixture is not linked topology")
	}
	hook := filepath.Join(common, "hooks", "fac190-test-hook")
	if err := osb.writeUnder(profile, root, hook, "evil\n"); err == nil {
		if _, e := os.Stat(hook); e == nil {
			t.Fatal("hook write under common-dir succeeded")
		}
	}
	if _, err := os.Stat(hook); err == nil {
		t.Fatal("hook inode created")
	}
}

func TestRealPathFailsClosedOnMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-leaf")
	if _, err := realPath(missing); err == nil {
		t.Fatal("realPath accepted missing path — fail-open regression")
	}
	if _, err := realPath("/tmp/foo\nbar"); err == nil {
		t.Fatal("control char path accepted")
	}
}

func TestWrapperNamesCoversProviderAndArgv0(t *testing.T) {
	names := WrapperNames("ollama", "opencode")
	if len(names) != 2 {
		t.Fatalf("want provider+argv0, got %v", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	if !seen["ollama"] || !seen["opencode"] {
		t.Fatalf("missing names: %v", names)
	}
}

func TestVerifyAgentWrappersDetectsSwap(t *testing.T) {
	issuer, err := NewHMACIssuer([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	root := filepath.Join(shared, "wt")
	_ = os.MkdirAll(root, 0o755)
	enf := &Enforcer{Issuer: issuer, OS: &FakeOS{}}
	prep, err := enf.PrepareOS(root, shared, "FAC-190", 7, "task/fac-190", "codex", "codex")
	if err != nil {
		t.Fatal(err)
	}
	// Unfreeze bin for the swap test.
	_ = os.Chmod(prep.BinDir, 0o755)
	wrapper := filepath.Join(prep.BinDir, "codex")
	_ = os.Chmod(wrapper, 0o755)
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n# swapped\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAgentWrappers(prep.BinDir, prep.ProfilePath, prep.ProfileDigest, prep.Names); err == nil {
		t.Fatal("swapped wrapper accepted")
	}
	if _, err := enf.BindAndProve(productionIdentity(t, root, shared), prep); err == nil {
		t.Fatal("BindAndProve accepted swapped wrapper")
	}
}

func TestPrepareOSInstallsBothOllamaAndOpencode(t *testing.T) {
	issuer, err := NewHMACIssuer([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	root := filepath.Join(shared, "wt")
	_ = os.MkdirAll(root, 0o755)
	enf := &Enforcer{Issuer: issuer, OS: &FakeOS{}}
	prep, err := enf.PrepareOS(root, shared, "FAC-190", 7, "task/fac-190", "ollama", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ollama", "opencode"} {
		if !prep.WrapperResolves(name) {
			t.Fatalf("wrapper %s missing", name)
		}
	}
	if isPathPrefix(prep.Session.Root, root) {
		t.Fatal("session inside worktree")
	}
	env, err := prep.TabEnv(root, "/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "ZDOTDIR=") || !strings.Contains(joined, "PATH=") {
		t.Fatalf("TabEnv incomplete: %v", env)
	}
	if !strings.Contains(joined, prep.Session.ZdotDir) {
		t.Fatal("ZDOTDIR not session-local")
	}
}

func TestObserveSharedRootStableUnderHerdChurn(t *testing.T) {
	shared := t.TempDir()
	d1, err := ObserveSharedRoot(shared)
	if err != nil {
		t.Fatal(err)
	}
	// Coordinator-like volatile files under .herd must not drift the digest.
	_ = os.MkdirAll(filepath.Join(shared, ".herd"), 0o755)
	_ = os.WriteFile(filepath.Join(shared, ".herd", "launch-claims.db-wal"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(shared, ".herd", "mail.lock"), []byte("y"), 0o644)
	d2, err := ObserveSharedRoot(shared)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatal("ObserveSharedRoot drifted on coordinator .herd churn")
	}
}

func TestProductionEnforcerRequiresOSWhenSecretPresent(t *testing.T) {
	t.Setenv(SecretEnv, "present-secret")
	// On Darwin with sandbox-exec this may succeed; on Linux must be ErrOSUnavailable.
	_, err := ProductionEnforcer()
	if runtime.GOOS != "darwin" {
		if !errors.Is(err, ErrOSUnavailable) {
			t.Fatalf("want ErrOSUnavailable on non-darwin, got %v", err)
		}
		return
	}
	if err != nil && !errors.Is(err, ErrOSUnavailable) {
		// secret present — only OS unavailability is acceptable failure
		t.Fatalf("unexpected: %v", err)
	}
}

func TestDigestIncludesCreatedAtAndMarshalErrors(t *testing.T) {
	issuer, err := NewHMACIssuer([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := InstallSentinel(root); err != nil {
		t.Fatal(err)
	}
	b1, err := Bind(productionIdentity(t, root, ""), issuer)
	if err != nil {
		t.Fatal(err)
	}
	b2 := *b1
	b2.CreatedAt = b1.CreatedAt.Add(time.Second)
	d1, err := b1.digest()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := b2.digest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatal("digest ignores CreatedAt — replay collision")
	}
}

func TestProductionEnforcerRequiresSecret(t *testing.T) {
	t.Setenv(SecretEnv, "")
	t.Setenv(SecretEnvFallback, "")
	if _, err := ProductionEnforcer(); !errors.Is(err, ErrMissingSecret) {
		t.Fatalf("want ErrMissingSecret, got %v", err)
	}
}

func TestMutationHMACUsesSecret(t *testing.T) {
	if _, err := NewHMACIssuer(nil); !errors.Is(err, ErrMissingSecret) {
		t.Fatal(err)
	}
	a, _ := NewHMACIssuer([]byte("a"))
	b, _ := NewHMACIssuer([]byte("b"))
	root := t.TempDir()
	s, err := InstallSentinel(root)
	if err != nil {
		t.Fatal(err)
	}
	tuple := fixtureTuple(root)
	pa, err := a.Issue(root, s, tuple)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Verify(root, s, tuple, pa); err == nil {
		t.Fatal("cross-secret verify accepted")
	}
	if len(pa.MAC) != 32 {
		t.Fatalf("mac len %d", len(pa.MAC))
	}
	want, _ := a.Issue(root, s, tuple)
	if hmac.Equal(pa.MAC, want.MAC) {
		t.Fatal("identical MAC across nonces — nonce not mixed")
	}
}

func TestInstallSentinelIdempotentAndRejectsTamper(t *testing.T) {
	root := t.TempDir()
	p1, err := InstallSentinel(root)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := InstallSentinel(root)
	if err != nil || p1 != p2 {
		t.Fatalf("idempotent: %v %q %q", err, p1, p2)
	}
	if err := os.WriteFile(p1, []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallSentinel(root); !errors.Is(err, ErrInvalidSentinel) {
		t.Fatalf("tamper: %v", err)
	}
}

func TestObserveSharedRootReadOnly(t *testing.T) {
	shared := t.TempDir()
	before, _ := os.ReadDir(shared)
	dig, err := ObserveSharedRoot(shared)
	if err != nil || dig == "" {
		t.Fatalf("observe: %v %q", err, dig)
	}
	after, _ := os.ReadDir(shared)
	if len(after) != len(before) {
		t.Fatal("ObserveSharedRoot wrote under shared")
	}
	// Incident present fails.
	p := filepath.Join(shared, filepath.FromSlash(SharedRootIncidentRel))
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("x"), 0o600)
	if _, err := ObserveSharedRoot(shared); err == nil {
		t.Fatal("incident path not rejected")
	}
}
