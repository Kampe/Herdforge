package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/reviewack"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

func TestNativeRetirementCorruptJournalFailsClosed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "retirement-phases.jsonl")
	if err := os.WriteFile(p, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	op := &NativeReviewRetirementOp{Root: t.TempDir(), JournalPath: p}
	if _, err := op.Completed(ReviewRetirementManifest{Generation: "g", CandidateSHA: strings.Repeat("a", 40), Reviewer: "r", BindingDigest: "d"}); err == nil {
		t.Fatal("corrupt retirement journal was silently ignored")
	}
}

func TestCanonicalRetirementVerdictSelectsLatestReassessment(t *testing.T) {
	sha := strings.Repeat("a", 40)
	base := reviewledger.LedgerRow{Event: string(reviewledger.EventVerdict), SHA: sha, CandidateSHA: sha, Reviewer: "reviewer", Verdict: string(reviewledger.VerdictFAIL), ArtifactDigest: strings.Repeat("1", 64)}
	next := base
	next.Verdict = string(reviewledger.VerdictPASS)
	next.ArtifactDigest = strings.Repeat("2", 64)
	next.Reassesses = reviewledger.VerdictEventDigest(base)
	got, err := canonicalRetirementVerdict([]reviewledger.LedgerRow{base, next}, sha, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if got.Reassesses != next.Reassesses || got.Verdict != string(reviewledger.VerdictPASS) {
		t.Fatalf("selected verdict=%+v, want latest reassessment", got)
	}
}

func TestCanonicalRetirementVerdictRejectsConflictingReassessmentBranches(t *testing.T) {
	sha := strings.Repeat("a", 40)
	base := reviewledger.LedgerRow{Event: string(reviewledger.EventVerdict), SHA: sha, CandidateSHA: sha, Reviewer: "reviewer", Verdict: string(reviewledger.VerdictFAIL), ArtifactDigest: strings.Repeat("1", 64)}
	left := base
	left.Verdict = string(reviewledger.VerdictPASS)
	left.ArtifactDigest = strings.Repeat("2", 64)
	left.Reassesses = reviewledger.VerdictEventDigest(base)
	right := left
	right.Verdict = string(reviewledger.VerdictBLOCKED)
	right.ArtifactDigest = strings.Repeat("3", 64)
	if _, err := canonicalRetirementVerdict([]reviewledger.LedgerRow{base, left, right}, sha, "reviewer"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("conflicting reassessment branch accepted: %v", err)
	}
}

func TestLaunchSelectionIgnoresLaterHistoricalDuplicate(t *testing.T) {
	sha := strings.Repeat("a", 40)
	m := retirementManifest(t, "launch-bound")
	m.CandidateSHA = sha
	m.RecordedAt = "2026-09-10T05:44:34.504025Z"
	rows := []reviewledger.LedgerRow{
		{Event: string(reviewledger.EventRecord), SHA: sha, Reviewer: m.Reviewer, Lease: m.Nonce, Branch: m.TaskRef, Timestamp: "2026-09-10T05:44:34.000000Z"},
		{Event: string(reviewledger.EventRecord), SHA: sha, Reviewer: m.Reviewer, Lease: m.Nonce, Branch: "wrong-history", Timestamp: "2026-09-10T05:44:35.000000Z"},
	}
	var selected reviewledger.LedgerRow
	for _, row := range rows {
		if row.Event == string(reviewledger.EventRecord) && row.SHA == m.CandidateSHA && row.Reviewer == m.Reviewer && row.Lease == m.Nonce && launchBeforeManifest(row, m.RecordedAt) {
			if selected.Event != "" && !reflect.DeepEqual(selected, row) {
				t.Fatal("historical launch selection became ambiguous")
			}
			selected = row
		}
	}
	if selected.Branch != m.TaskRef {
		t.Fatalf("selected launch=%+v", selected)
	}
}

func TestRetireReviewLanesRetainsSupersededButContinuesEligible(t *testing.T) {
	old, current := retirementManifest(t, "old"), retirementManifest(t, "current")
	f := &retirementFake{evidence: map[string]ReviewRetirementEvidence{"old": {}, "current": retirementEvidence(current)}}
	// The fake models a closed historical manifest whose named slot is now
	// owned by another lease; the current lane remains independently eligible.
	oldErr := errRetirementSuperseded
	f.observeErr = map[string]error{"old": oldErr}
	r, err := RetireReviewLanes(f, []ReviewRetirementManifest{old, current}, false)
	if err != nil || r.Failed != 0 || r.Blocked != 1 || r.Retired != 1 {
		t.Fatalf("report=%+v err=%v events=%v", r, err, f.events)
	}
}

func TestRetireReviewLanesAggregatesHardObservationFailure(t *testing.T) {
	bad, good := retirementManifest(t, "bad-observation"), retirementManifest(t, "good-observation")
	f := &retirementFake{evidence: map[string]ReviewRetirementEvidence{"good-observation": retirementEvidence(good)}, observeErr: map[string]error{"bad-observation": errors.New("active mismatched lease")}}
	r, err := RetireReviewLanes(f, []ReviewRetirementManifest{bad, good}, false)
	if err == nil || r.Failed != 1 || len(f.events) != 0 {
		t.Fatalf("report=%+v err=%v events=%v", r, err, f.events)
	}
}

func TestNativeRetirementUsesLegacyIncarnationAndExactPoolAfterClosedPane(t *testing.T) {
	foreignCWD := t.TempDir()
	t.Chdir(foreignCWD)
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", "source.txt").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", root, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "source").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	shaBytes, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaBytes))
	poolRoot := filepath.Join(root, ".herd", "pool-fac708")
	slotPath := filepath.Join(poolRoot, "pool-01")
	if err := os.MkdirAll(poolRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", slotPath, "HEAD").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	nonce, generation := "lease-legacy-1", int64(7)
	state := []byte(`{"version":1,"slots":[{"name":"pool-01","path":".herd/pool-fac708/pool-01","lease_id":"` + nonce + `","leased_at":"1970-01-01T00:00:00.000000007Z","base":"HEAD"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolRoot, "pool.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(root, ".herd", "review", "ledger.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reviewer := "forge-mender-fac708-nat-d4b3b8dc"
	rows := []string{
		`{"event":"record","sha":"` + sha + `","reviewer":"` + reviewer + `","lease":"` + nonce + `","branch":"FAC-708"}`,
		`{"event":"verdict","sha":"` + sha + `","candidate_sha":"` + sha + `","reviewer":"` + reviewer + `","verdict":"PASS","artifact_digest":"artifact"}`,
	}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reviewack.Emit(root, reviewack.Ack{SHA: sha, Reviewer: reviewer, LaunchIdentity: reviewer, ArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	m := NewReviewRetirementManifest(time.Now(), ReviewRetirementManifest{
		Repository: "fixture-repo", TaskRef: "FAC-708", TaskID: "task-708", CandidateSHA: sha, BaseSHA: sha, Branch: "master",
		Worktree: ".herd/pool-fac708/pool-01", Pool: ".herd/pool-fac708", Slot: "pool-01", LeaseGeneration: generation,
		Workspace: "wK", TabID: "wK:t15T", PaneID: "wK:p15T", TerminalID: "term_fixture", SessionID: "01a08822-494f-79e3-ac9d-4fcac02e7305",
		Reviewer: reviewer, ReviewerFamily: "openai", ReviewerModel: "gpt-5.6-luna", PromptArtifact: ".herd/review/prompts/p.md", Generation: "legacy-1", Nonce: nonce,
	})
	oldRunHerdr := runHerdr
	t.Cleanup(func() { runHerdr = oldRunHerdr })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"error":{"code":"pane_not_found","message":"pane not found"}}`, errors.New("exit status 1")
		}
		return "", errors.New("unexpected fake Herdr command")
	}
	ledger, err := reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	op := &NativeReviewRetirementOp{Root: root, RepositoryIdentity: "fixture-repo", Ledger: ledger}
	evidence, err := op.Observe(m)
	if err != nil {
		t.Fatal(err)
	}
	if d := EvaluateReviewRetirement(evidence); !d.Eligible {
		t.Fatalf("closed generationless lane refused: %+v", d)
	}
	absoluteState := []byte(`{"version":1,"slots":[{"name":"pool-01","path":"` + slotPath + `","lease_id":"` + nonce + `","leased_at":"1970-01-01T00:00:00.000000007Z","base":"HEAD"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolRoot, "pool.json"), absoluteState, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := op.Observe(m); err != nil {
		t.Fatalf("equivalent absolute pool path was refused: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "pool-01")
	outsideState := []byte(`{"version":1,"slots":[{"name":"pool-01","path":"` + outside + `","lease_id":"` + nonce + `","leased_at":"1970-01-01T00:00:00.000000007Z","base":"HEAD"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolRoot, "pool.json"), outsideState, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := op.Observe(m); err == nil || !strings.Contains(err.Error(), "path differs") {
		t.Fatalf("outside-root pool path was accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(poolRoot, "pool.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(append(rows, `{"event":"record","sha":"`+sha+`","reviewer":"`+reviewer+`","lease":"`+nonce+`","branch":"FAC-708","pane":"different"}`), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := op.Observe(m); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("duplicate launch provenance was accepted: %v", err)
	}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := op.ReleaseLease(t.Context(), m); err != nil {
		t.Fatalf("exact legacy lease release: %v", err)
	}
	if released, err := op.LeaseReleased(m); err != nil || !released {
		t.Fatalf("release readback=%v err=%v", released, err)
	}
	// A later lease/release of the same named slot must not authorize the old
	// manifest: release history is an incarnation fence, not an absent-state
	// guess.
	stale := []byte(`{"version":1,"slots":[{"name":"pool-01","path":".herd/pool-fac708/pool-01","last_release_lease_id":"lease-new","last_release_generation":8,"last_release_path":".herd/pool-fac708/pool-01"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolRoot, "pool.json"), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := op.Observe(m); err == nil || !strings.Contains(err.Error(), "release history") {
		t.Fatalf("reused released slot was not refused: %v", err)
	}
	var agent AgentEntry
	if err := json.Unmarshal([]byte(`{"name":"r","revision":2,"state_change_seq":3}`), &agent); err != nil {
		t.Fatal(err)
	}
	var tab TabRecord
	if err := json.Unmarshal([]byte(`{"tab_id":"wK:t15T","workspace_id":"wK","number":1210,"pane_count":1,"focused":false}`), &tab); err != nil {
		t.Fatal(err)
	}
	if agent.TabGeneration != 0 || tab.Generation != "" || tab.TabGeneration != "" {
		t.Fatalf("fixture unexpectedly supplied generation: agent=%+v tab=%+v", agent, tab)
	}
}

func TestNativeRetirementClosesLiveSettledReviewerAndProvesAbsence(t *testing.T) {
	// This is a disposable process owned by this test, not a fleet process.
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	closed := false
	oldRunHerdr := runHerdr
	t.Cleanup(func() { runHerdr = oldRunHerdr })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			if closed {
				return `{"result":{"agents":[]}}`, nil
			}
			return `{"result":{"agents":[{"name":"forge-mender-fac708-nat-d4b3b8dc","agent_status":"idle","pane_id":"wK:p15T","tab_id":"wK:t15T","workspace_id":"wK","terminal_id":"term_fixture","focused":false,"agent_session":{"value":"session-fixture"}}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			if closed {
				return `{"error":{"code":"pane_not_found","message":"pane not found"}}`, errors.New("exit status 1")
			}
			return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"wK:p15T","shell_pid":0,"foreground_processes":[{"pid":%d,"name":"sleep","argv":["sleep","60"]}]}}}`, child.Process.Pid), nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			if closed {
				return `{"result":{"tabs":[]}}`, nil
			}
			return `{"result":{"tabs":[{"tab_id":"wK:t15T","workspace_id":"wK","number":15,"pane_count":1,"focused":false}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "compare-close" {
			return `unknown command: compare-close`, errors.New("exit status 1")
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "close" {
			closed = true
			return `{"result":{}}`, nil
		}
		return "", fmt.Errorf("unexpected fake Herdr command %v", args)
	}

	oldStartToken := readPIDStartToken
	readPIDStartToken = func(pid int) (string, error) {
		if pid != child.Process.Pid {
			return "", fmt.Errorf("unexpected pid %d", pid)
		}
		return "fixture-start-token", nil
	}
	t.Cleanup(func() { readPIDStartToken = oldStartToken })

	root := t.TempDir()
	op := &NativeReviewRetirementOp{Root: root, RepositoryIdentity: "fixture-repo"}
	m := ReviewRetirementManifest{
		Repository: "fixture-repo", TaskRef: "FAC-708", TaskID: "task-708",
		CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Branch: "refs/herd/reviews/fac-708",
		Worktree: ".herd/pool-fac708/pool-01", Pool: ".herd/pool-fac708", Slot: "pool-01", LeaseGeneration: 7,
		Workspace: "wK", TabID: "wK:t15T", PaneID: "wK:p15T", TerminalID: "term_fixture", SessionID: "session-fixture",
		Reviewer: "forge-mender-fac708-nat-d4b3b8dc", ReviewerFamily: "openai", ReviewerModel: "gpt-5.6-luna",
		PromptArtifact: ".herd/review/prompts/fac-708.md", Generation: "live-close-1", Nonce: "lease-live-1",
		RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := op.Close(m); err != nil {
		t.Fatalf("live settled close: %v", err)
	}
	if !closed {
		t.Fatal("fake Herdr close transition was not invoked")
	}
	if _, err := AgentList(); err != nil {
		t.Fatalf("agent absence readback: %v", err)
	}
}

func TestNativeRetirementWithTaskLaunchProvenanceAndArtifactAck(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", "source.txt").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", root, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "source").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	shaBytes, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaBytes))
	poolRoot := filepath.Join(root, ".herd", "pool-fac790")
	slotPath := filepath.Join(poolRoot, "pool-01")
	if err := os.MkdirAll(poolRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", slotPath, "HEAD").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	nonce := "pool-01-1789024038488630000"
	state := []byte(`{"version":1,"slots":[{"name":"pool-01","path":".herd/pool-fac790/pool-01","lease_id":"` + nonce + `","leased_at":"2026-09-10T07:07:18.488630000Z","base":"HEAD"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolRoot, "pool.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(root, ".herd", "review", "ledger.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reviewer := "review-fac-790-df7a47623fb2"
	digest := "271b6c2e9cc5ddf5808d75464cca32a2e8f742cbec88589d7582181809b8c8e9"
	rows := []string{
		`{"ts":"2026-09-10T07:07:18.960880Z","event":"record","sha":"` + sha + `","reviewer":"` + reviewer + `","gate":"provenance-unrecorded","task":"FAC-790","lease":"` + nonce + `"}`,
		`{"ts":"2026-09-10T07:36:30.926981Z","event":"verdict","sha":"` + sha + `","candidate_sha":"` + sha + `","reviewer":"` + reviewer + `","verdict":"PASS","artifact_digest":"` + digest + `","task":"FAC-790"}`,
	}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Emit schema version 2 artifact ack
	if err := reviewack.EmitArtifact(root, reviewack.Ack{SHA: sha, Reviewer: reviewer, LaunchIdentity: reviewer, ArtifactDigest: digest}); err != nil {
		t.Fatal(err)
	}
	m := NewReviewRetirementManifest(time.Now(), ReviewRetirementManifest{
		Repository: "github.com/Kampe/Herdforge", TaskRef: "FAC-790", TaskID: "task-790", CandidateSHA: sha, BaseSHA: sha, Branch: "master",
		Worktree: ".herd/pool-fac790/pool-01", Pool: ".herd/pool-fac790", Slot: "pool-01", LeaseGeneration: 1789024038488630000,
		Workspace: "wK", TabID: "wK:t18N", PaneID: "wK:p18N", TerminalID: "term_fixture", SessionID: "ses_fixture",
		Reviewer: reviewer, ReviewerFamily: "open-weight", ReviewerModel: "litellm/lazer/claude-haiku-4.5", PromptArtifact: ".herd/review/prompts/p.md", Generation: "pool-01-1789024038488630000", Nonce: nonce,
		RecordedAt: "2026-09-10T07:07:26.919588Z",
	})
	oldRunHerdr := runHerdr
	t.Cleanup(func() { runHerdr = oldRunHerdr })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"error":{"code":"pane_not_found","message":"pane not found"}}`, errors.New("exit status 1")
		}
		return "", errors.New("unexpected fake Herdr command")
	}
	ledger, err := reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	op := &NativeReviewRetirementOp{Root: root, RepositoryIdentity: "github.com/Kampe/Herdforge", Ledger: ledger}
	evidence, err := op.Observe(m)
	if err != nil {
		t.Fatal(err)
	}
	if d := EvaluateReviewRetirement(evidence); !d.Eligible {
		t.Fatalf("canonical task-provenance launch and artifact ack should be eligible: %+v", d)
	}
}

func TestNativeReviewRetirement_AdvancedOriginMainPostReleaseProof(t *testing.T) {
	root := t.TempDir()
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v (%s)", strings.Join(args, " "), dir, err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out))
	}
	runGit(root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("base v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(root, "add", "base.txt")
	runGit(root, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "initial base")
	oldBaseSHA := runGit(root, "rev-parse", "HEAD")

	runGit(root, "remote", "add", "origin", "https://example.invalid/fixture.git")
	runGit(root, "update-ref", "refs/remotes/origin/main", oldBaseSHA)

	poolDir := filepath.Join(root, ".herd", "pool-fac792")
	p := worktree.NewPool(root, poolDir, 1)
	p.DefaultBase = "origin/main"
	if err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("pool ensure: %v", err)
	}
	slot, err := p.Lease(context.Background(), "review-fac-792")
	if err != nil {
		t.Fatalf("pool acquire: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "cand.txt"), []byte("cand commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(root, "add", "cand.txt")
	runGit(root, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "candidate commit")
	candSHA := runGit(root, "rev-parse", "HEAD")

	reviewRef := "refs/herd/reviews/fac-792-branch"
	runGit(root, "update-ref", reviewRef, candSHA)

	slotPath := slot.Path
	if !filepath.IsAbs(slotPath) {
		slotPath = filepath.Join(root, filepath.FromSlash(slotPath))
	}
	runGit(slotPath, "reset", "--hard", candSHA)

	relWorktree, _ := filepath.Rel(root, slotPath)

	// Advance origin/main to newMainSHA (simulates concurrent PR merges landing on main)
	if err := os.WriteFile(filepath.Join(root, "main2.txt"), []byte("main v2 commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(root, "add", "main2.txt")
	runGit(root, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "advanced main commit")
	newMainSHA := runGit(root, "rev-parse", "HEAD")
	runGit(root, "update-ref", "refs/remotes/origin/main", newMainSHA)

	promptRel := ".herd/review/prompts/fac-792.md"
	promptBody := []byte("review prompt for FAC-792\n")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(promptRel))), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(promptRel)), promptBody, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestRel := ".herd/review/manifests/fac-792.json"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(manifestRel))), 0o700); err != nil {
		t.Fatal(err)
	}

	launchTS := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339Nano)
	manifestTS := time.Now().UTC().Format(time.RFC3339Nano)

	reviewer := "review-fac-792-canary"
	m := NewReviewRetirementManifest(time.Now(), ReviewRetirementManifest{
		Repository: "example.invalid/fixture", TaskRef: "FAC-792", TaskID: "task-fac-792",
		CandidateSHA: candSHA, BaseSHA: oldBaseSHA, Branch: reviewRef, ReviewRef: reviewRef,
		Worktree: filepath.ToSlash(relWorktree), Pool: ".herd/pool-fac792", Slot: slot.Name,
		LeaseGeneration: slot.LeasedAt.UnixNano(), Nonce: slot.LeaseID,
		Workspace: "wK", TabID: "wK:t792", PaneID: "wK:p792", TerminalID: "term_792", SessionID: "ses_792",
		Reviewer: reviewer, ReviewerFamily: "open-weight", ReviewerModel: "litellm/lazer/claude-haiku-4.5",
		PromptArtifact: promptRel, PromptDigest: reviewack.ArtifactDigest(promptBody),
		ManifestArtifact: manifestRel, Generation: "gen-fac-792",
		RecordedAt: manifestTS,
	})
	mJSON, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifestRel)), append(mJSON, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	ledgerPath := filepath.Join(root, ".herd", "review", "ledger.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	artifactDigest := strings.Repeat("a", 64)
	ledgerRows := []string{
		`{"ts":"` + launchTS + `","event":"record","sha":"` + candSHA + `","reviewer":"` + reviewer + `","branch":"` + reviewRef + `","lease":"` + slot.LeaseID + `"}`,
		`{"ts":"` + manifestTS + `","event":"verdict","sha":"` + candSHA + `","candidate_sha":"` + candSHA + `","reviewer":"` + reviewer + `","verdict":"PASS","artifact_digest":"` + artifactDigest + `"}`,
	}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(ledgerRows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reviewack.EmitArtifact(root, reviewack.Ack{SHA: candSHA, Reviewer: reviewer, LaunchIdentity: reviewer, ArtifactDigest: artifactDigest}); err != nil {
		t.Fatal(err)
	}

	oldRunHerdr := runHerdr
	t.Cleanup(func() { runHerdr = oldRunHerdr })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"error":{"code":"pane_not_found","message":"pane not found"}}`, errors.New("exit status 1")
		}
		return `{"result":{}}`, nil
	}

	ledger, err := reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	op := &NativeReviewRetirementOp{Root: root, RepositoryIdentity: "example.invalid/fixture", Ledger: ledger}

	res, err := RetireReviewLanesContext(context.Background(), op, []ReviewRetirementManifest{m}, false)
	if err != nil {
		t.Fatalf("RetireReviewLanesContext failed: %v", err)
	}
	if res.Retired != 1 || res.Failed != 0 || res.Blocked != 0 {
		t.Fatalf("unexpected retirement report: %+v", res)
	}

	if _, err := os.Stat(slotPath); !os.IsNotExist(err) {
		t.Fatalf("expected slotPath removed upon retirement, but exists")
	}

	cmd := exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", reviewRef)
	if err := cmd.Run(); err == nil {
		t.Fatalf("review ref still exists")
	}

	// Second tick: verify idempotence
	res2, err := RetireReviewLanesContext(context.Background(), op, []ReviewRetirementManifest{m}, false)
	if err != nil {
		t.Fatalf("second tick RetireReviewLanesContext failed: %v", err)
	}
	if res2.Failed != 0 || res2.Blocked != 0 || len(res2.Candidates) != 1 || !res2.Candidates[0].Completed {
		t.Fatalf("unexpected second tick report: %+v", res2)
	}
}

func TestNativeReviewRetirement_NegativeSafetyFailsClosed(t *testing.T) {
	root := t.TempDir()
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v (%s)", strings.Join(args, " "), dir, err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out))
	}
	runGit(root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("base v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(root, "add", "base.txt")
	runGit(root, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "initial base")
	baseSHA := runGit(root, "rev-parse", "HEAD")

	poolDir := filepath.Join(root, ".herd", "pool-safety")
	p := worktree.NewPool(root, poolDir, 1)
	p.DefaultBase = "HEAD"
	if err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("pool ensure: %v", err)
	}
	slot, err := p.Lease(context.Background(), "review-safety")
	if err != nil {
		t.Fatalf("pool acquire: %v", err)
	}
	slotPath := slot.Path
	if !filepath.IsAbs(slotPath) {
		slotPath = filepath.Join(root, filepath.FromSlash(slotPath))
	}
	relWorktree, _ := filepath.Rel(root, slotPath)

	m := NewReviewRetirementManifest(time.Now(), ReviewRetirementManifest{
		Repository: "example.invalid/fixture", TaskRef: "FAC-792", TaskID: "task-fac-792",
		CandidateSHA: baseSHA, BaseSHA: baseSHA, Branch: "refs/herd/reviews/fac-792-safety", ReviewRef: "refs/herd/reviews/fac-792-safety",
		Worktree: filepath.ToSlash(relWorktree), Pool: ".herd/pool-safety", Slot: slot.Name,
		LeaseGeneration: slot.LeasedAt.UnixNano(), Nonce: slot.LeaseID,
		Workspace: "wK", TabID: "wK:t792", PaneID: "wK:p792", TerminalID: "term_792", SessionID: "ses_792",
		Reviewer: "reviewer-safety", ReviewerFamily: "open-weight", ReviewerModel: "litellm/lazer/claude-haiku-4.5",
		PromptArtifact: ".herd/review/prompts/fac-792.md", PromptDigest: strings.Repeat("a", 64),
		Generation: "gen-safety", RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})

	op := &NativeReviewRetirementOp{Root: root, RepositoryIdentity: "example.invalid/fixture"}

	// 1. Negative: dirty worktree before worktree phase fails closed
	if err := os.WriteFile(filepath.Join(slotPath, "dirty.txt"), []byte("dirty file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := op.Revalidate(m, "worktree"); err == nil || !strings.Contains(err.Error(), "worktree changed before destructive removal") {
		t.Fatalf("expected dirty worktree to fail revalidation, got %v", err)
	}
	_ = os.Remove(filepath.Join(slotPath, "dirty.txt"))

	// 2. Negative: unexpected HEAD fails closed
	if err := os.WriteFile(filepath.Join(slotPath, "new.txt"), []byte("new commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(slotPath, "add", "new.txt")
	runGit(slotPath, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "new commit in slot")
	if err := op.Revalidate(m, "worktree"); err == nil || !strings.Contains(err.Error(), "worktree changed before destructive removal") {
		t.Fatalf("expected unexpected HEAD to fail revalidation, got %v", err)
	}
}
