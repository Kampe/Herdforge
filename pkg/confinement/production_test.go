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
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s (%v)", args, out, err)
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "fac190@test.local")
	run("git", "config", "user.name", "fac190")
	run("git", "commit", "--allow-empty", "-m", "init")
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
	prep, err := enf.PrepareOS(root, shared, "codex", "codex")
	if err != nil {
		t.Fatal(err)
	}
	// Bind must NOT create files under shared root.
	before, _ := os.ReadDir(shared)
	binding, err := enf.BindAndProve(productionIdentity(t, root, shared), prep)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadDir(shared)
	if len(after) != len(before) {
		t.Fatalf("Bind mutated shared root entries: before=%d after=%d", len(before), len(after))
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
	shared := t.TempDir()
	root := filepath.Join(shared, "wt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, root)
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	osb := DarwinSeatbelt{}
	before, err := os.ReadDir(shared)
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := map[string]struct{}{}
	for _, e := range before {
		beforeNames[e.Name()] = struct{}{}
	}
	profile, err := osb.Prepare(root, shared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shared, ".herd")); err == nil {
		t.Fatal("Prepare created .herd under shared root")
	}
	if err := osb.ProveWriteDenials(root, shared, profile); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(shared)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after {
		if _, ok := beforeNames[e.Name()]; !ok {
			t.Fatalf("ProveWriteDenials mutated shared root with %q", e.Name())
		}
		if strings.HasPrefix(e.Name(), "fac190-probe-") || e.Name() == "FAC190_DENY_PROBE" {
			t.Fatalf("probe residue under shared: %s", e.Name())
		}
	}
}

func TestDarwinFirstMatchProfileDeniesSharedParent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	if _, err := RequireOS(); err != nil {
		t.Skip(err.Error())
	}
	shared := t.TempDir()
	root := filepath.Join(shared, "task-wt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, root)
	osb := DarwinSeatbelt{}
	profile, err := osb.Prepare(root, shared)
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
	// Shared parent must never be a file-write subpath grant.
	if strings.Contains(body, "(allow file-write* (subpath \""+absShared+"\"))") {
		t.Fatal("profile grants shared parent writes")
	}
	if !strings.Contains(body, "(allow network*)") {
		t.Fatal("profile missing network grant required for agents")
	}
	// Live denial of shared residual + live allow of gitdir (real evaluation).
	outside := filepath.Join(shared, "outside-shared.txt")
	if err := osb.writeUnder(profile, root, outside, "nope\n"); err == nil {
		if _, statErr := os.Stat(outside); statErr == nil {
			t.Fatal("shared-parent write succeeded")
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("outside inode under shared parent")
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
	root := t.TempDir()
	enf := &Enforcer{Issuer: issuer, OS: &FakeOS{}}
	prep, err := enf.PrepareOS(root, "", "codex", "codex")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(prep.BinDir, "codex")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n# swapped\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAgentWrappers(prep.BinDir, prep.ProfilePath, prep.ProfileDigest, prep.Names); err == nil {
		t.Fatal("swapped wrapper accepted")
	}
	if _, err := enf.BindAndProve(productionIdentity(t, root, ""), prep); err == nil {
		t.Fatal("BindAndProve accepted swapped wrapper")
	}
}

func TestPrepareOSInstallsBothOllamaAndOpencode(t *testing.T) {
	issuer, err := NewHMACIssuer([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	enf := &Enforcer{Issuer: issuer, OS: &FakeOS{}}
	prep, err := enf.PrepareOS(root, "", "ollama", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ollama", "opencode"} {
		if !prep.WrapperResolves(name) {
			t.Fatalf("wrapper %s missing", name)
		}
	}
	env, err := prep.TabEnv(root, "/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "ZDOTDIR=") || !strings.Contains(joined, "PATH=") {
		t.Fatalf("TabEnv incomplete: %v", env)
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

func TestProductionEnforcerRequiresSecretAndOS(t *testing.T) {
	t.Setenv(SecretEnv, "")
	t.Setenv(SecretEnvFallback, "")
	if _, err := ProductionEnforcer(); !errors.Is(err, ErrMissingSecret) {
		if err == nil || !errors.Is(err, ErrMissingSecret) {
			t.Fatalf("got %v", err)
		}
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
