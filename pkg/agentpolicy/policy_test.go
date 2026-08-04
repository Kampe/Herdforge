package agentpolicy

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	surface := SurfaceNestedAgent
	if op == OperationShell {
		surface = SurfaceShell
	}
	if op == OperationHerdrDispatch {
		surface = SurfaceHerdrDispatch
	}
	return Attempt{Operation: op, Child: child, Repository: "github.com/Kampe/Herdforge", Surface: surface, Family: "codex"}
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

func TestOpaqueHerdrIDsRemainCaseSensitive(t *testing.T) {
	key := []byte("fixture-key")
	a, err := NewContract("repo", "FAC-173", "w7iejxhmai2s8tn17u44usyp", "forge-smith", 1, "wF:s1", "wF:t6R", "wF:pC", "codex", SurfaceHerdrDispatch, key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewContract("repo", "FAC-173", "w7iejxhmai2s8tn17u44usyp", "forge-smith", 1, "wf:s1", "wf:t6r", "wf:pc", "codex", SurfaceHerdrDispatch, key)
	if err != nil {
		t.Fatal(err)
	}
	if a.PolicyDigest == b.PolicyDigest || a.AuthTag == b.AuthTag {
		t.Fatal("case-distinct Herdr identities must not collide")
	}
}

func TestRepositoryCanonicalizationPreservesCaseSensitivePaths(t *testing.T) {
	key := []byte("fixture-key")
	hostCase, err := NewContract("GitHub.com/Kampe/Herdforge", "task", "lane", "role", 1, "session", "tab", "pane", "codex", SurfaceHerdrDispatch, key)
	if err != nil {
		t.Fatal(err)
	}
	hostPathCase, err := NewContract("github.com/kampe/Herdforge", "task", "lane", "role", 1, "session", "tab", "pane", "codex", SurfaceHerdrDispatch, key)
	if err != nil {
		t.Fatal(err)
	}
	if hostCase.Repository != "github.com/Kampe/Herdforge" {
		t.Fatalf("host canonicalization changed path: %q", hostCase.Repository)
	}
	if hostCase.PolicyDigest == hostPathCase.PolicyDigest {
		t.Fatal("Git-host path case must remain identity-bearing")
	}
	localUpper, err := NewContract("/Volumes/Forge/CaseRepo", "task", "lane", "role", 1, "session", "tab", "pane", "codex", SurfaceHerdrDispatch, key)
	if err != nil {
		t.Fatal(err)
	}
	localLower, err := NewContract("/volumes/forge/caserepo", "task", "lane", "role", 1, "session", "tab", "pane", "codex", SurfaceHerdrDispatch, key)
	if err != nil {
		t.Fatal(err)
	}
	if localUpper.PolicyDigest == localLower.PolicyDigest {
		t.Fatal("local path case must remain identity-bearing")
	}
}

func TestExplicitHerdrSurfacesAreClosedAndExact(t *testing.T) {
	key := []byte("fixture-key")
	for _, surface := range []string{SurfaceHerdrDispatch, SurfaceHerdrSend, SurfaceHerdrReview} {
		c, err := NewContract("repo", "task", "lane", "role", 1, "session", "tab", "pane", "codex", surface, key)
		if err != nil {
			t.Fatal(err)
		}
		a := attempt(OperationHerdrDispatch, "")
		a.Repository = c.Repository
		a.Surface = surface
		if err := c.Decide(key, a); err != nil {
			t.Fatalf("%s: %v", surface, err)
		}
		for _, drift := range []string{"herd target", "herd payload", "shell"} {
			a.Surface = drift
			if err := c.Decide(key, a); !errors.Is(err, ErrDenied) {
				t.Fatalf("%s drift %s: %v", surface, drift, err)
			}
		}
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
	s, err := NewEvidenceStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Append(c, key, attempt(OperationNestedAgent, ChildClaudeAgent), ErrDenied)
	if err != nil || first.Sequence != 1 {
		t.Fatalf("first evidence: %+v %v", first, err)
	}
	if first.Repository != c.Repository || first.HerdrSession != c.HerdrSession || first.ParentExecutionFamily != c.ParentExecutionFamily || first.AllowedHerdrSurface != c.AllowedHerdrSurface || first.Child != ChildClaudeAgent {
		t.Fatal("evidence is not bound to exact contract context")
	}
	s.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewEvidenceStore(path, key)
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
	s, err := NewEvidenceStore(filepath.Join(t.TempDir(), "denials.jsonl"), key)
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
	if _, err := NewEvidenceStore(path, []byte("fixture-key")); !errors.Is(err, ErrEvidence) {
		t.Fatalf("expected stale receipt rejection, got %v", err)
	}
}

func TestInvalidAttemptNeverWritesEvidence(t *testing.T) {
	c, key := testContract(t)
	path := filepath.Join(t.TempDir(), "denials.jsonl")
	s, err := NewEvidenceStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bad := attempt(OperationNestedAgent, ChildKind("unknown-child"))
	if _, err := s.Append(c, key, bad, ErrDenied); !errors.Is(err, ErrInvalidAttempt) {
		t.Fatalf("expected invalid child rejection, got %v", err)
	}
	b, _ := os.ReadFile(path)
	if len(b) != 0 {
		t.Fatal("invalid attempt wrote evidence")
	}
}

func TestEvidenceRecordMACRejectsTamperingAndPartialTail(t *testing.T) {
	c, key := testContract(t)
	path := filepath.Join(t.TempDir(), "denials.jsonl")
	s, err := NewEvidenceStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(c, key, attempt(OperationNestedAgent, ChildClaudeAgent), ErrDenied); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b[:len(b)-1], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEvidenceStore(path, key); !errors.Is(err, ErrEvidence) {
		t.Fatal("complete JSON without trailing newline must fail closed")
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	var allowed DenialEvidence
	if err := json.Unmarshal(b, &allowed); err != nil {
		t.Fatal(err)
	}
	allowed.Operation, allowed.Child, allowed.AttemptedSurface = OperationShell, "", SurfaceShell
	allowed.RecordMAC = recordMAC(allowed, key)
	allowedBytes, _ := json.Marshal(allowed)
	if err := os.WriteFile(path, append(allowedBytes, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEvidenceStore(path, key); !errors.Is(err, ErrEvidence) {
		t.Fatal("readback must reject an allowed operation presented as denial")
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatal(err)
	}
	record["attempted_family"] = "tampered"
	tampered, _ := json.Marshal(record)
	if err := os.WriteFile(path, append(tampered, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEvidenceStore(path, key); !errors.Is(err, ErrEvidence) {
		t.Fatal("record MAC must reject tampering")
	}
	if err := os.WriteFile(path, append(b[:len(b)-1], '{'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEvidenceStore(path, key); !errors.Is(err, ErrEvidence) {
		t.Fatalf("partial tail must fail closed, got %v", err)
	}
}

func TestEvidenceSurfacesSyncAndUnlockFailures(t *testing.T) {
	c, key := testContract(t)
	s, err := NewEvidenceStore(filepath.Join(t.TempDir(), "denials.jsonl"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	oldUnlock, oldSync := evidenceUnlock, evidenceSync
	evidenceUnlock = func(fd int, how int) error {
		err := syscall.Flock(fd, how)
		if how == syscall.LOCK_UN {
			return errors.New("unlock fixture")
		}
		return err
	}
	if _, err := s.Append(c, key, attempt(OperationNestedAgent, ChildClaudeAgent), ErrDenied); err == nil || !strings.Contains(err.Error(), "unlock fixture") {
		t.Fatalf("unlock failure not surfaced: %v", err)
	}
	evidenceUnlock = oldUnlock
	evidenceSync = func(*os.File) error { return errors.New("sync fixture") }
	if _, err := s.Append(c, key, attempt(OperationNestedAgent, ChildRecovery), ErrDenied); err == nil || !strings.Contains(err.Error(), "sync fixture") {
		t.Fatalf("sync failure not surfaced: %v", err)
	}
	evidenceSync = oldSync
	if _, err := s.Append(c, key, attempt(OperationNestedAgent, ChildVerifier), ErrDenied); !errors.Is(err, ErrEvidence) {
		t.Fatalf("quarantined store must reject later append: %v", err)
	}
}

func TestEvidenceShortWriteFailsClosed(t *testing.T) {
	c, key := testContract(t)
	s, err := NewEvidenceStore(filepath.Join(t.TempDir(), "denials.jsonl"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if first, err := s.Append(c, key, attempt(OperationNestedAgent, ChildClaudeAgent), ErrDenied); err != nil || first.Sequence != 1 {
		t.Fatalf("valid prefix: %+v %v", first, err)
	}
	oldWrite := evidenceWrite
	evidenceWrite = func(f *os.File, b []byte) (int, error) {
		if _, err := f.Write(b[:len(b)-1]); err != nil {
			return len(b) - 1, err
		}
		return len(b) - 1, nil
	}
	if _, err := s.Append(c, key, attempt(OperationNestedAgent, ChildRecovery), ErrDenied); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write must fail closed: %v", err)
	}
	evidenceWrite = oldWrite
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := NewEvidenceStore(s.file.Name(), key); err != nil {
		t.Fatalf("restart after rolled-back short write: %v", err)
	} else {
		if second, appendErr := reopened.Append(c, key, attempt(OperationNestedAgent, ChildVerifier), ErrDenied); appendErr != nil || second.Sequence != 2 {
			t.Fatalf("valid prefix continuation: %+v %v", second, appendErr)
		}
		_ = reopened.Close()
	}
}

func TestRollbackFailureQuarantinesStore(t *testing.T) {
	c, key := testContract(t)
	s, err := NewEvidenceStore(filepath.Join(t.TempDir(), "denials.jsonl"), key)
	if err != nil {
		t.Fatal(err)
	}
	oldWrite, oldRollback := evidenceWrite, evidenceRollback
	evidenceWrite = func(_ *os.File, b []byte) (int, error) { return len(b) - 1, nil }
	evidenceRollback = func(*os.File, int64) error { return errors.New("rollback fixture") }
	if _, err := s.Append(c, key, attempt(OperationNestedAgent, ChildClaudeAgent), ErrDenied); err == nil || !strings.Contains(err.Error(), "rollback fixture") {
		t.Fatalf("rollback failure not surfaced: %v", err)
	}
	evidenceWrite, evidenceRollback = oldWrite, oldRollback
	if _, err := s.Append(c, key, attempt(OperationNestedAgent, ChildRecovery), ErrDenied); !errors.Is(err, ErrEvidence) {
		t.Fatalf("rollback uncertainty must quarantine store: %v", err)
	}
	_ = s.Close()
}
