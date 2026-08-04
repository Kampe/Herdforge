package agentpolicy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testContract(t *testing.T) (Contract, []byte) {
	t.Helper()
	key := []byte("fixture-key")
	c, err := NewContract("github.com/Kampe/Herdforge", "FAC-173", "w7iejxhmai2s8tn17u44usyp", "forge-smith", 7, "session-1", "tab-2", "pane-3", "codex", "herd dispatch", key)
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

func attempt(op Operation, child ChildKind) Attempt {
	return Attempt{Operation: op, Child: child, Repository: "github.com/kampe/herdforge", Surface: "herd dispatch", Family: "codex"}
}

func TestContractBindsAndAuthenticatesAllFields(t *testing.T) {
	c, key := testContract(t)
	if err := c.Verify(key); err != nil {
		t.Fatal(err)
	}
	c.Role = "reviewer"
	if !errors.Is(c.Verify(key), ErrInvalidContract) {
		t.Fatal("role mutation must invalidate contract")
	}
}

func TestDecideDeniesNestedContextsBeforeCreation(t *testing.T) {
	c, key := testContract(t)
	for _, child := range []ChildKind{ChildClaudeAgent, ChildClaudeTask, ChildCodexSubagent, ChildCodexCollaboration, ChildRecovery, ChildReviewer, ChildVerifier, ChildWorker, ChildCoordinator, ChildExternalRepository} {
		if err := c.Decide(key, attempt(OperationNestedAgent, child)); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: got %v", child, err)
		}
	}
}

func TestDecideAllowsShellAndHerdrOnly(t *testing.T) {
	c, key := testContract(t)
	for _, op := range []Operation{OperationShell, OperationHerdrDispatch} {
		if err := c.Decide(key, attempt(op, "")); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	badShell := attempt(OperationShell, "")
	badShell.Repository = "other/repo"
	if err := c.Decide(key, badShell); !errors.Is(err, ErrDenied) {
		t.Fatal("shell must not bypass repository binding")
	}
	badHerdr := attempt(OperationHerdrDispatch, "")
	badHerdr.Surface = "other-surface"
	if err := c.Decide(key, badHerdr); !errors.Is(err, ErrDenied) {
		t.Fatal("Herdr dispatch must not bypass surface binding")
	}
	bad := attempt(OperationNestedAgent, ChildExternalRepository)
	bad.Repository = "other/repo"
	if err := c.Decide(key, bad); !errors.Is(err, ErrDenied) {
		t.Fatal("external repository must be denied")
	}
}

func TestDecideUnknownOperationFailsClosed(t *testing.T) {
	c, key := testContract(t)
	if err := c.Decide(key, attempt(Operation("invented"), "")); !errors.Is(err, ErrDenied) {
		t.Fatal("invented operation must be denied")
	}
	if err := c.Decide(key, attempt("", "")); !errors.Is(err, ErrDenied) {
		t.Fatal("empty operation must be denied")
	}
}

func TestMissingOrStalePolicyFailsClosed(t *testing.T) {
	c, key := testContract(t)
	c.PolicyDigest = "stale"
	if err := c.Decide(key, attempt(OperationShell, "")); !errors.Is(err, ErrInvalidContract) {
		t.Fatal("stale policy must fail closed")
	}
	c, _ = testContract(t)
	if err := c.Decide([]byte("wrong"), attempt(OperationShell, "")); !errors.Is(err, ErrInvalidContract) {
		t.Fatal("wrong key must fail closed")
	}
}

func TestEvidenceIsDurableMonotonicAndBound(t *testing.T) {
	c, key := testContract(t)
	path := filepath.Join(t.TempDir(), "denials.jsonl")
	s, err := NewEvidenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Append(c, key, attempt(OperationNestedAgent, ChildClaudeAgent), ErrDenied)
	if err != nil || first.Sequence != 1 {
		t.Fatalf("first evidence: %+v %v", first, err)
	}
	if first.Repository != c.Repository || first.HerdrSession != c.HerdrSession || first.Child != ChildClaudeAgent {
		t.Fatal("evidence is not bound to exact contract context")
	}
	s.Close()
	s, err = NewEvidenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	second, err := s.Append(c, key, attempt(OperationNestedAgent, ChildRecovery), ErrDenied)
	if err != nil || second.Sequence != 2 {
		t.Fatalf("second evidence: %+v %v", second, err)
	}
	b, _ := os.ReadFile(path)
	if len(b) == 0 {
		t.Fatal("durable evidence file is empty")
	}
}

func TestEvidenceRequiresExactDeniedDecision(t *testing.T) {
	c, key := testContract(t)
	s, err := NewEvidenceStore(filepath.Join(t.TempDir(), "denials.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Append(c, key, attempt(OperationShell, ""), ErrDenied); !errors.Is(err, ErrEvidence) {
		t.Fatal("allowed operation must not become denial evidence")
	}
	if _, err := s.Append(c, key, attempt(OperationNestedAgent, ChildClaudeAgent), errors.New("caller invented reason")); !errors.Is(err, ErrEvidence) {
		t.Fatal("arbitrary reason must not become denial evidence")
	}
}

func TestEvidenceReadbackRejectsMissingOrStaleReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "denials.jsonl")
	if err := os.WriteFile(path, []byte(`{"repository":"repo","task":"task","herdr_session":"session","sequence":2,"child":"claude-agent","policy_digest":"digest","operation":"nested-agent","attempted_repository":"repo","attempted_surface":"surface","attempted_family":"family"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEvidenceStore(path); !errors.Is(err, ErrEvidence) {
		t.Fatalf("expected stale receipt rejection, got %v", err)
	}
}
