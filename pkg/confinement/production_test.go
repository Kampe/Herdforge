package confinement

import (
	"crypto/hmac"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	// Forged MAC
	bad := proof
	bad.MAC = append([]byte{}, proof.MAC...)
	bad.MAC[0] ^= 0xff
	if err := issuer.Verify(root, sentinel, tuple, bad); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("forged MAC: %v", err)
	}
	// Cross-task replay
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
	binding, err := enf.BindAndProve(productionIdentity(t, root, shared))
	if err != nil {
		t.Fatal(err)
	}
	if !fake.Proved || !binding.OSProved || binding.OSBackend != "fake-os" {
		t.Fatalf("OS proof not recorded: %+v fake=%+v", binding, fake)
	}
	incident := filepath.Join(shared, ".herd", "FAC-188-R2-RESIDUAL.md")
	if err := binding.Boundary.AuthorizeWrite(binding.Capability, incident); err == nil {
		t.Fatal("policy accepted shared-root incident path")
	}
	if err := binding.AuthorizeRelativeWrite("pkg/ok.go"); err != nil {
		t.Fatalf("relative write: %v", err)
	}
	// Receipt persisted
	receipts := filepath.Join(root, ".herd", "receipts", "confinement-receipts.jsonl")
	if data, err := os.ReadFile(receipts); err != nil || !strings.Contains(string(data), binding.ReceiptDigest) {
		t.Fatalf("receipt missing: %v %q", err, data)
	}
	// Shared-root sentinel revalidation
	if err := binding.CheckSharedRoot(); err != nil {
		t.Fatal(err)
	}
	// Dirty shared root sentinel → fail closed
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
	// seed an allow path inside worktree
	if err := os.WriteFile(filepath.Join(root, ".herd", "seed"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	osb := DarwinSeatbelt{}
	if err := osb.ProveWriteDenials(root, shared); err != nil {
		t.Fatal(err)
	}
}

func TestProductionEnforcerRequiresSecretAndOS(t *testing.T) {
	t.Setenv(SecretEnv, "")
	t.Setenv(SecretEnvFallback, "")
	if _, err := ProductionEnforcer(); !errors.Is(err, ErrMissingSecret) {
		// On hosts without sandbox-exec we may fail OS first only after secret;
		// without secret must be missing secret.
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
	// Non-vacuous: empty secret cannot issue.
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
	// Ensure MAC is real HMAC-SHA256 shaped (32 bytes)
	if len(pa.MAC) != 32 {
		t.Fatalf("mac len %d", len(pa.MAC))
	}
	// Manual recompute
	want, _ := a.Issue(root, s, tuple) // different nonce
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
