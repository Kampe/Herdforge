package agentpolicy

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestBindLaunchAndRequireGeneration(t *testing.T) {
	key := []byte("fixture-key")
	b, c, err := BindLaunch("github.com/Kampe/Herdforge", "FAC-173", "lane-1", "forge-smith", 9, "sess", "tab", "pane", "codex", SurfaceHerdrDispatch, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Verify(key); err != nil {
		t.Fatal(err)
	}
	if err := RequireLaunchBinding(b, key, 9); err != nil {
		t.Fatal(err)
	}
	if err := RequireLaunchBinding(b, key, 8); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("generation drift must fail closed: %v", err)
	}
	// Recovery after a Herdr server generation must re-verify the same
	// authenticated binding; a stale digest is refused before task mutation.
	stale := b
	stale.PolicyDigest = "stale"
	if err := RequireLaunchBinding(stale, key, 9); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("stale binding must fail closed: %v", err)
	}
	if c.LeaseGeneration != 9 || c.PolicyDigest != b.PolicyDigest {
		t.Fatal("contract/binding drift")
	}
}

func TestMissingBindingFailsClosed(t *testing.T) {
	if err := (LaunchBinding{}).Verify([]byte("k")); !errors.Is(err, ErrMissingBinding) {
		t.Fatalf("empty binding: %v", err)
	}
	if err := RequireLaunchBinding(LaunchBinding{}, []byte("k"), 1); !errors.Is(err, ErrMissingBinding) {
		t.Fatalf("require empty: %v", err)
	}
}

func TestEnforceDeniesNestedAndRecordsEvidence(t *testing.T) {
	c, key := testContract(t)
	path := filepath.Join(t.TempDir(), "denials.jsonl")
	store, err := NewEvidenceStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ev, err := Enforce(c, key, attempt(OperationNestedAgent, ChildCodexSubagent), store)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Outcome != "denied" || ev.Child != ChildCodexSubagent || ev.LeaseGeneration != c.LeaseGeneration {
		t.Fatalf("bad evidence: %+v", ev)
	}
	// Shell and explicit Herdr surfaces remain allowed and write no evidence.
	if _, err := Enforce(c, key, attempt(OperationShell, ""), store); err != nil {
		t.Fatal(err)
	}
	if _, err := Enforce(c, key, attempt(OperationHerdrDispatch, ""), store); err != nil {
		t.Fatal(err)
	}
}

func TestEnforceFailsClosedWithoutStoreOnDenial(t *testing.T) {
	c, key := testContract(t)
	_, err := Enforce(c, key, attempt(OperationNestedAgent, ChildClaudeAgent), nil)
	if err == nil || !errors.Is(err, ErrDenied) {
		t.Fatalf("missing store must fail closed on denial: %v", err)
	}
}

func TestContributorProofExactSHANoHiddenChildren(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	p, err := ProveNoHiddenContributors(sha, "codex", "session-1", "FAC-173", 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
	// Hidden child must block review admission.
	dirty, err := ProveNoHiddenContributors(sha, "codex", "session-1", "FAC-173", 3, []ChildKind{ChildCodexCollaboration})
	if err != nil {
		t.Fatal(err)
	}
	if err := dirty.Verify(); err == nil {
		t.Fatal("hidden child must fail Verify")
	}
	// Digest tamper fails closed.
	p.ParentSession = "other"
	if err := p.Verify(); err == nil {
		t.Fatal("tampered proof must fail")
	}
}

func TestKeyFromEnvFailClosed(t *testing.T) {
	t.Setenv(SecretEnv, "")
	t.Setenv(SecretEnvFallback, "")
	if _, err := KeyFromEnv(); !errors.Is(err, ErrMissingSecret) {
		t.Fatalf("expected missing secret, got %v", err)
	}
	t.Setenv(SecretEnvFallback, "control-secret")
	key, err := KeyFromEnv()
	if err != nil || string(key) != "control-secret" {
		t.Fatalf("fallback: %q %v", key, err)
	}
}
