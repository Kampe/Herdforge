package confinement

import (
	"crypto/hmac"
	"errors"
	"os"
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
	if !fake.Proved || prep.BinDir == "" {
		t.Fatalf("prepare incomplete: %+v fake=%+v", prep, fake)
	}
	binding, err := enf.BindAndProve(productionIdentity(t, root, shared), prep)
	if err != nil {
		t.Fatal(err)
	}
	if !fake.Proved || !binding.OSProved || !binding.AgentWrapped || binding.OSBackend != "fake-os" {
		t.Fatalf("OS/agent wrap not recorded: %+v fake=%+v", binding, fake)
	}
	if binding.WrapperBinDir == "" || binding.ProfilePath == "" {
		t.Fatalf("missing wrap paths: %+v", binding)
	}
	// Wrapper binary installed
	if _, err := os.Stat(filepath.Join(binding.WrapperBinDir, "codex")); err != nil {
		t.Fatalf("wrapper missing: %v", err)
	}
	incident := filepath.Join(shared, ".herd", "FAC-188-R2-RESIDUAL.md")
	if err := binding.Boundary.AuthorizeWrite(binding.Capability, incident); err == nil {
		t.Fatal("policy accepted shared-root incident path")
	}
	if err := binding.AuthorizeRelativeWrite("pkg/ok.go"); err != nil {
		t.Fatalf("relative write: %v", err)
	}
	// Unique receipt file persisted (not interleaved JSONL)
	entries, err := os.ReadDir(filepath.Join(root, ".herd", "receipts"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("receipts: %v %v", err, entries)
	}
	found := false
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(root, ".herd", "receipts", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), binding.ReceiptDigest) && strings.Contains(string(data), `"agent_wrapped":true`) {
			found = true
		}
	}
	if !found {
		t.Fatal("receipt missing agent_wrapped digest")
	}
	if err := binding.CheckSharedRoot(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, filepath.FromSlash(SharedRootSentinelRelPath)), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := binding.CheckSharedRoot(); err == nil {
		t.Fatal("dirty shared-root sentinel accepted")
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
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "seed"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	osb := DarwinSeatbelt{}
	profile, err := osb.Prepare(root, shared)
	if err != nil {
		t.Fatal(err)
	}
	// Prepare must not create shared/.herd (only EnsureSharedRootSentinel does).
	if _, err := os.Stat(filepath.Join(shared, ".herd")); err == nil {
		t.Fatal("Prepare created .herd under shared root")
	}
	// Snapshot shared tree entries before prove — must be unchanged after.
	before, err := os.ReadDir(shared)
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := map[string]struct{}{}
	for _, e := range before {
		beforeNames[e.Name()] = struct{}{}
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
			t.Fatalf("ProveWriteDenials mutated shared root with new entry %q", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(shared, ".herd", "FAC-188-R2-RESIDUAL.md")); err == nil {
		t.Fatal("proof left residual under shared root")
	}
	// No fac190-probe leftovers under shared (including worktrees parent).
	entries, _ := os.ReadDir(shared)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "fac190-probe-") {
			t.Fatalf("probe dir under shared: %s", e.Name())
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
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	osb := DarwinSeatbelt{}
	profile, err := osb.Prepare(root, shared)
	if err != nil {
		t.Fatal(err)
	}
	// Profile must not contain an allow for the shared parent path.
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Count(body, "(allow file-write*") != 1 {
		t.Fatalf("want exactly one file-write allow (worktree only), got:\n%s", body)
	}
	// Explicit: no allow line quoting the shared parent.
	absShared, err := realPath(shared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "(allow file-write* (subpath \""+absShared+"\"))") {
		t.Fatal("profile grants shared parent writes — first-match inverted")
	}
	// Live denial: write under shared but outside worktree must fail.
	outside := filepath.Join(shared, "outside-shared.txt")
	if err := osb.writeUnder(profile, root, outside, "nope\n"); err == nil {
		if _, statErr := os.Stat(outside); statErr == nil {
			t.Fatal("shared-parent write succeeded under first-match profile")
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("outside inode under shared parent")
	}
}

func TestRealPathFailsClosedOnMissing(t *testing.T) {
	// Missing path must error. A fail-open reimplementation (EvalSymlinks err →
	// return abs, nil) would return nil err and must turn this test red.
	missing := filepath.Join(t.TempDir(), "missing-leaf")
	if _, err := realPath(missing); err == nil {
		t.Fatal("realPath accepted missing path — fail-open regression")
	}
	// Control characters rejected.
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
	// Same name once.
	if got := WrapperNames("codex", "codex"); len(got) != 1 || got[0] != "codex" {
		t.Fatalf("dedupe: %v", got)
	}
}

func TestVerifyAgentWrappersDetectsSwap(t *testing.T) {
	issuer, err := NewHMACIssuer([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fake := &FakeOS{}
	enf := &Enforcer{Issuer: issuer, OS: fake}
	prep, err := enf.PrepareOS(root, "", "codex", "codex")
	if err != nil {
		t.Fatal(err)
	}
	// Swap wrapper contents after prepare.
	wrapper := filepath.Join(prep.BinDir, "codex")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n# swapped\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAgentWrappers(prep.BinDir, prep.ProfilePath, prep.Names); err == nil {
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
		if _, err := os.Stat(filepath.Join(prep.BinDir, name)); err != nil {
			t.Fatalf("wrapper %s: %v", name, err)
		}
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
	// Force distinct CreatedAt
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
	t.Setenv(SecretEnv, "prod-secret")
	if runtime.GOOS == "darwin" {
		if err := execLookSandbox(); err != nil {
			t.Skip("no sandbox-exec")
		}
		enf, err := ProductionEnforcer()
		if err != nil {
			t.Fatal(err)
		}
		if enf.OS == nil || enf.OS.Name() != "sandbox-exec" {
			t.Fatalf("backend: %+v", enf.OS)
		}
	}
}

func execLookSandbox() error {
	_, err := os.Stat("/usr/bin/sandbox-exec")
	return err
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
