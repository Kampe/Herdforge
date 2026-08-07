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
	if !binding.OSProved || !binding.WrapperInstalled || binding.ProfileDigest == "" || binding.ReceiptMACHex == "" {
		t.Fatalf("incomplete binding: %+v", binding)
	}
	if err := binding.VerifyReceiptMAC(issuer); err != nil {
		t.Fatal(err)
	}
	// Forged receipt fails MAC
	forged := *binding
	forged.WrapperInstalled = false
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
	if !strings.Contains(body, "file-write-create") || !strings.Contains(body, "file-write-data") {
		t.Fatal("objects must use create+data grants")
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
	if strings.Contains(body, "(allow file-write* (subpath \""+filepath.Join(common, "objects")+"\"))") {
		t.Fatal("objects file-write* would allow rm -rf of shared store")
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

	// Profile must grant ONLY literal branch ref paths — never parent Dir subpaths
	// (refs/heads/task would allow every sibling task/* lane). Prepare used branch "task/fac-190".
	branchLiteral := filepath.Join(common, "refs", "heads", "task", "fac-190")
	if !strings.Contains(body, "(allow file-write* (literal \""+branchLiteral+"\"))") {
		t.Fatalf("profile missing literal branch ref grant for %s\n--- profile ---\n%s", branchLiteral, body)
	}
	taskDirGrant := "(allow file-write* (subpath \"" + filepath.Join(common, "refs", "heads", "task") + "\"))"
	if strings.Contains(body, taskDirGrant) {
		t.Fatal("profile grants sibling task/* ref namespace via Dir subpath")
	}
	if strings.Contains(body, "(allow file-write* (subpath \""+filepath.Join(common, "refs", "heads")+"\"))") {
		t.Fatal("profile grants entire refs/heads namespace")
	}
	// packed-refs / shared HEAD must not be write-granted.
	if strings.Contains(body, filepath.Join(common, "packed-refs")) {
		t.Fatal("profile mentions packed-refs write grant")
	}
	headLit := "(allow file-write* (literal \"" + filepath.Join(common, "HEAD") + "\"))"
	if strings.Contains(body, headLit) {
		t.Fatal("profile grants common HEAD write")
	}

	// Live deny: sibling branch ref.
	siblingRef := filepath.Join(common, "refs", "heads", "task", "fac-188-sibling")
	_ = os.MkdirAll(filepath.Dir(siblingRef), 0o755)
	_ = os.Remove(siblingRef)
	if err := osb.writeUnder(profile, root, siblingRef, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n"); err == nil {
		if _, e := os.Stat(siblingRef); e == nil {
			t.Fatal("SIBLING-REF: write succeeded on sibling lane branch")
		}
	}
	if _, err := os.Stat(siblingRef); err == nil {
		t.Fatal("SIBLING-REF: inode created")
	}

	// Live deny: packed-refs and shared HEAD mutation.
	for _, name := range []string{"packed-refs", "HEAD"} {
		target := filepath.Join(common, name)
		before, _ := os.ReadFile(target)
		marker := "fac190-review-deny\n"
		if err := osb.writeUnder(profile, root, target, marker); err == nil {
			after, _ := os.ReadFile(target)
			if strings.Contains(string(after), marker) {
				if before != nil {
					_ = os.WriteFile(target, before, 0o644)
				}
				t.Fatalf("%s: write succeeded under confinement", name)
			}
		}
		if after, err := os.ReadFile(target); err == nil && strings.Contains(string(after), marker) {
			if before != nil {
				_ = os.WriteFile(target, before, 0o644)
			}
			t.Fatalf("%s: mutated under confinement", name)
		}
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

func TestWriteSeatbeltProfileBranchLiteralOnly(t *testing.T) {
	// Platform-independent: profile text must never grant Dir(ref) subpaths.
	dir := t.TempDir()
	profile := filepath.Join(dir, "profile.sb")
	wt := filepath.Join(dir, "wt")
	gitDir := filepath.Join(dir, "gitdir")
	common := filepath.Join(dir, "common")
	path, err := writeSeatbeltProfile(wt, gitDir, common, "task/fac-190", profile)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	wantRef := filepath.Join(common, "refs", "heads", "task", "fac-190")
	if !strings.Contains(s, "(allow file-write* (literal \""+wantRef+"\"))") {
		t.Fatalf("missing literal ref grant for %s", wantRef)
	}
	if !strings.Contains(s, "(allow file-write* (literal \""+wantRef+".lock\"))") {
		t.Fatal("missing literal ref.lock grant")
	}
	// The round-5 CRITICAL regression: Dir("refs/heads/task/fac-190") == "refs/heads/task".
	bad := "(allow file-write* (subpath \"" + filepath.Join(common, "refs", "heads", "task") + "\"))"
	if strings.Contains(s, bad) {
		t.Fatal("Dir subpath grant would allow every sibling task/* branch")
	}
	if strings.Contains(s, filepath.Join(common, "packed-refs")) {
		t.Fatal("packed-refs write grant present")
	}
	if strings.Contains(s, "(allow file-write* (literal \""+filepath.Join(common, "HEAD")+"\"))") {
		t.Fatal("common HEAD write grant present")
	}
	// Objects must not use file-write* (includes unlink → rm -rf objects).
	objects := filepath.Join(common, "objects")
	if strings.Contains(s, "(allow file-write* (subpath \""+objects+"\"))") {
		t.Fatal("objects granted file-write* (unlink/destroy surface)")
	}
	if !strings.Contains(s, "(allow file-write-create (subpath \""+objects+"\"))") {
		t.Fatal("objects missing file-write-create")
	}
	if !strings.Contains(s, "(allow file-write-data (subpath \""+objects+"\"))") {
		t.Fatal("objects missing file-write-data")
	}
	// Bare branch without slash must not grant entire refs/heads.
	path2 := filepath.Join(dir, "profile-main.sb")
	if _, err := writeSeatbeltProfile(wt, gitDir, common, "main", path2); err != nil {
		t.Fatal(err)
	}
	body2, _ := os.ReadFile(path2)
	s2 := string(body2)
	if strings.Contains(s2, "(allow file-write* (subpath \""+filepath.Join(common, "refs", "heads")+"\"))") {
		t.Fatal("bare branch Dir grant covers entire refs/heads")
	}
	if !strings.Contains(s2, "(allow file-write* (literal \""+filepath.Join(common, "refs", "heads", "main")+"\"))") {
		t.Fatal("missing literal main ref")
	}
}

func TestPrepareOSSameLeaseRelaunch(t *testing.T) {
	// FreezeSession must not brick coordinator rewrite of the same (task, lease).
	issuer, err := NewHMACIssuer([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	root := filepath.Join(shared, "wt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	enf := &Enforcer{Issuer: issuer, OS: &FakeOS{}}
	prep1, err := enf.PrepareOS(root, shared, "FAC-190", 7, "task/fac-190", "codex", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prep1.TabEnv(root, "/usr/bin"); err != nil {
		t.Fatalf("first TabEnv: %v", err)
	}
	// Second PrepareOS + TabEnv on the same generation must succeed (thaw).
	prep2, err := enf.PrepareOS(root, shared, "FAC-190", 7, "task/fac-190", "codex", "codex")
	if err != nil {
		t.Fatalf("same-lease PrepareOS relaunch: %v", err)
	}
	if _, err := prep2.TabEnv(root, "/usr/bin"); err != nil {
		t.Fatalf("same-lease TabEnv relaunch: %v", err)
	}
	if !prep2.WrapperResolves("codex") {
		t.Fatal("wrapper missing after relaunch")
	}
}

func TestCheckSharedRootResidualStableUnderHerdChurn(t *testing.T) {
	shared := t.TempDir()
	if err := CheckSharedRootResidual(shared); err != nil {
		t.Fatal(err)
	}
	// Coordinator-like volatile files under .herd must not cause false rejection.
	// Residual check only Lstats the FAC-188 incident path — not WAL/locks.
	_ = os.MkdirAll(filepath.Join(shared, ".herd"), 0o755)
	_ = os.WriteFile(filepath.Join(shared, ".herd", "launch-claims.db-wal"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(shared, ".herd", "mail.lock"), []byte("y"), 0o644)
	if err := CheckSharedRootResidual(shared); err != nil {
		t.Fatalf("residual check failed on coordinator churn: %v", err)
	}
	// Live failure mode: incident path present must be rejected (non-tautological).
	incident := filepath.Join(shared, filepath.FromSlash(SharedRootIncidentRel))
	if err := os.MkdirAll(filepath.Dir(incident), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(incident, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckSharedRootResidual(shared); err == nil {
		t.Fatal("incident path under shared accepted")
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

func TestCheckSharedRootResidualReadOnly(t *testing.T) {
	shared := t.TempDir()
	before, _ := os.ReadDir(shared)
	if err := CheckSharedRootResidual(shared); err != nil {
		t.Fatalf("residual: %v", err)
	}
	after, _ := os.ReadDir(shared)
	if len(after) != len(before) {
		t.Fatal("CheckSharedRootResidual wrote under shared")
	}
	p := filepath.Join(shared, filepath.FromSlash(SharedRootIncidentRel))
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("x"), 0o600)
	if err := CheckSharedRootResidual(shared); err == nil {
		t.Fatal("incident path not rejected")
	}
}
