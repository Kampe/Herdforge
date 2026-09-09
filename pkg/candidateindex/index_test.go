package candidateindex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

type mockProvider struct {
	tasks []*provider.Task
}

func (m *mockProvider) GetTask(_ context.Context, id string) (*provider.Task, error) {
	for _, t := range m.tasks {
		if t.ID == id || t.Ref == id {
			return t, nil
		}
	}
	return nil, os.ErrNotExist
}

func (m *mockProvider) ListTasks(_ context.Context, _ string, _ string) ([]*provider.Task, error) {
	return m.tasks, nil
}

func (m *mockProvider) ClaimTask(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockProvider) UpdateStatus(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockProvider) AddComment(_ context.Context, _ string, _ string) error {
	return nil
}

func TestCandidateIndex_DeterministicSorting(t *testing.T) {
	cands := []*Candidate{
		{Ref: "FAC-100", Priority: provider.PriorityLow, CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{Ref: "FAC-10", Priority: provider.PriorityUrgent, CandidateSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{Ref: "FAC-2", Priority: provider.PriorityHigh, CandidateSHA: "cccccccccccccccccccccccccccccccccccccccc"},
		{Ref: "FAC-20", Priority: provider.PriorityHigh, CandidateSHA: "dddddddddddddddddddddddddddddddddddddddd"},
	}

	SortCandidates(cands)

	expectedOrder := []string{"FAC-10", "FAC-2", "FAC-20", "FAC-100"}
	for i, exp := range expectedOrder {
		if cands[i].Ref != exp {
			t.Fatalf("expected index %d to be %s, got %s", i, exp, cands[i].Ref)
		}
	}
}

func TestAuthoritativeCandidate_PrefersHighestCompletedLeaseGeneration(t *testing.T) {
	oldSHA := "283f5c958818126e82bf38db4d5127c3f933cf4c"
	newSHA := "cb6b6c55b8aa799357d78129f2a61a7f6ce25cb9"
	when := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	old := &Candidate{Ref: "FAC-703", CandidateSHA: oldSHA, LeaseGeneration: 8, CompletionCallback: true}
	new := &Candidate{Ref: "FAC-703", CandidateSHA: newSHA, LeaseGeneration: 9, CompletionCallback: true}
	evidence := map[candidateKey]candidateEvidence{
		{ref: "FAC-703", sha: oldSHA}: {observedAt: when, source: SourceReviewLedger, sequence: 10},
		{ref: "FAC-703", sha: newSHA}: {observedAt: when, source: SourceReviewLedger, sequence: 10},
	}

	got := authoritativeCandidate([]*Candidate{old, new}, evidence)
	if got != new {
		t.Fatalf("selected %s, want newest lease-generation candidate %s", got.CandidateSHA, newSHA)
	}
}

func TestCandidateIndexDiscoversForgeTaskInConfiguredLaneWorktree(t *testing.T) {
	dir := t.TempDir()
	lane := filepath.Join(dir, "chainseer-forge-worker-live")
	if err := os.MkdirAll(lane, 0755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-C", lane}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "candidate-index@example.invalid")
	git("config", "user.name", "candidate-index")
	if err := os.WriteFile(filepath.Join(lane, "change.txt"), []byte("forge change\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", "change.txt")
	git("commit", "-qm", "forge candidate")
	out, err := exec.Command("git", "-C", lane, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	wantSHA := strings.TrimSpace(string(out))
	tc := dispatch.TaskContext{
		ProviderType: "memory", ProjectID: "project", Repository: "repo", Role: dispatch.RoleWorker,
		TaskRef: "FAC-1723", TaskID: "task-1723", Branch: "standing/worker", BaseSHA: wantSHA,
		LeaseID: "1", LeaseGeneration: 1, LeaseTaskRef: "FAC-1723", SessionID: "worker-session",
		AllowedOps: dispatch.WorkerOps, ExpiresAt: time.Now().Add(time.Hour),
	}
	body, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lane, dispatch.TaskContextFile), body, 0644); err != nil {
		t.Fatal(err)
	}

	idx := New(IndexOptions{
		RepoRoot:     dir,
		Config:       &config.Config{TaskProvider: config.TaskProvider{ProjectID: "project"}, Lanes: []config.LaneDef{{Name: "forge-worker", Worktree: "chainseer-forge-worker-live"}}},
		TaskProvider: &mockProvider{tasks: []*provider.Task{{ID: "task-1723", Ref: "FAC-1723", Status: "in-progress", Priority: provider.PriorityHigh}}},
	})
	cands, err := idx.BuildIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].CandidateSHA != wantSHA || cands[0].WorktreePath != lane {
		t.Fatalf("lane worktree candidate not discovered: %+v", cands)
	}
}

func TestCandidateIndex_BuildIndexMergesSourcesAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	mailPath := filepath.Join(dir, "mail.jsonl")
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	queuePath := filepath.Join(dir, "queue.jsonl")
	inboxDir := filepath.Join(dir, "inbox")
	worktreesDir := filepath.Join(dir, "worktrees")
	_ = os.MkdirAll(inboxDir, 0755)
	_ = os.MkdirAll(worktreesDir, 0755)

	validSHA := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	// 1. Setup mock provider with tasks
	tp := &mockProvider{
		tasks: []*provider.Task{
			{ID: "t-1", Ref: "FAC-101", Title: "Fix build gate", Priority: provider.PriorityHigh},
			{ID: "t-2", Ref: "FAC-102", Title: "Harden auth", Priority: provider.PriorityUrgent},
		},
	}

	// 2. Setup mail callback with candidate SHA
	mb := mail.NewMailbox(mailPath)
	cb := mail.Callback{
		Ref:             "FAC-101",
		Kind:            mail.CallbackComplete,
		SHA:             validSHA,
		LeaseGeneration: 3,
	}
	body, _ := json.Marshal(cb)
	_, _ = mb.SendMessage("worker", mail.CoordinatorInbox, "complete: FAC-101", string(body))

	// 3. Setup ledger with verdict
	ledger, err := reviewledger.NewReviewLedger(dir, ledgerPath)
	if err != nil {
		t.Fatalf("NewReviewLedger failed: %v", err)
	}
	ledger.Now = func() time.Time { return time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC) }
	_ = ledger.Record(reviewledger.RecordOpts{
		SHA:           validSHA,
		BuilderFamily: "openai",
		Task:          "FAC-101",
		Tier:          "R1",
	})
	_, _ = ledger.Verdict(reviewledger.VerdictOpts{
		SHA:            validSHA,
		Reviewer:       "assayer-1",
		Verdict:        reviewledger.VerdictPASS,
		ReviewerFamily: "anthropic",
		BuilderFamily:  "openai",
		Task:           "FAC-101",
		VfyDigest:      "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		PatchURL:       "patch-101",
		Lease:          "lease-101",
	})

	idx := New(IndexOptions{
		RepoRoot:     dir,
		Config:       &config.Config{TaskProvider: config.TaskProvider{ProjectID: "proj-1"}},
		TaskProvider: tp,
		MailPath:     mailPath,
		LedgerPath:   ledgerPath,
		QueuePath:    queuePath,
		InboxDir:     inboxDir,
		WorktreesDir: worktreesDir,
	})

	cands, err := idx.BuildIndex(context.Background())
	if err != nil {
		t.Fatalf("BuildIndex failed: %v", err)
	}

	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(cands))
	}

	// Priority order: Urgent (FAC-102) first, High (FAC-101) second
	if cands[0].Ref != "FAC-102" {
		t.Errorf("expected first candidate to be FAC-102, got %s", cands[0].Ref)
	}
	if cands[0].State != StateBlocked {
		t.Errorf("expected FAC-102 without SHA to be StateBlocked, got %s", cands[0].State)
	}
	if len(cands[0].BlockedReasons) == 0 || cands[0].BlockedReasons[0] != BlockedMissingCandidateSHA {
		t.Errorf("expected BlockedMissingCandidateSHA for FAC-102, got %v", cands[0].BlockedReasons)
	}

	if cands[1].Ref != "FAC-101" {
		t.Errorf("expected second candidate to be FAC-101, got %s", cands[1].Ref)
	}
	if cands[1].CandidateSHA != validSHA {
		t.Errorf("expected SHA %s for FAC-101, got %s", validSHA, cands[1].CandidateSHA)
	}
	if cands[1].State != StateEligible {
		t.Errorf("expected FAC-101 to be StateEligible, got %s", cands[1].State)
	}
	if len(cands[1].Sources) < 3 {
		t.Errorf("expected FAC-101 to merge at least 3 sources, got %v", cands[1].Sources)
	}
}

func TestCandidateIndex_BlockedOnMalformedAndVetoVerdicts(t *testing.T) {
	dir := t.TempDir()
	mailPath := filepath.Join(dir, "mail.jsonl")
	inboxDir := filepath.Join(dir, "inbox")
	_ = os.MkdirAll(inboxDir, 0755)

	// Blocked callback in mail
	mb := mail.NewMailbox(mailPath)
	cb := mail.Callback{
		Ref:    "FAC-200",
		Kind:   mail.CallbackBlocked,
		SHA:    "1111222233334444555566667777888899990000",
		Detail: "circular imports detected",
	}
	body, _ := json.Marshal(cb)
	_, _ = mb.SendMessage("worker", mail.CoordinatorInbox, "blocked: FAC-200", string(body))

	// Malformed artifact in inbox
	malformedArtifact := `sha: 123-bad-sha
reviewed-head: 123-bad-sha
---
`
	_ = os.WriteFile(filepath.Join(inboxDir, "FAC-201"), []byte(malformedArtifact), 0644)

	idx := New(IndexOptions{
		RepoRoot: dir,
		MailPath: mailPath,
		InboxDir: inboxDir,
	})

	cands, err := idx.BuildIndex(context.Background())
	if err != nil {
		t.Fatalf("BuildIndex failed: %v", err)
	}

	for _, c := range cands {
		if c.State != StateBlocked {
			t.Errorf("candidate %s expected to be StateBlocked, got %s", c.Ref, c.State)
		}
		if len(c.BlockedReasons) == 0 {
			t.Errorf("candidate %s expected blocked reasons, got empty", c.Ref)
		}
	}
}

func TestCandidateIndex_SelectsNewestCandidateEvidenceDeterministically(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	queuePath := filepath.Join(dir, "queue.jsonl")

	oldSHA := "1111111111111111111111111111111111111111"
	newSHA := "2222222222222222222222222222222222222222"
	rows := []reviewledger.LedgerRow{
		{Timestamp: "2026-08-16T00:00:00Z", Event: string(reviewledger.EventRecord), SHA: oldSHA, Task: "FAC-312"},
		{Timestamp: "2026-08-16T01:00:00Z", Event: string(reviewledger.EventVerdict), SHA: oldSHA, Task: "FAC-312", Verdict: string(reviewledger.VerdictFAIL)},
		{Timestamp: "2026-08-16T02:00:00Z", Event: string(reviewledger.EventRecord), SHA: newSHA, Task: "FAC-312"},
		{Timestamp: "2026-08-16T03:00:00Z", Event: string(reviewledger.EventVerdict), SHA: newSHA, Task: "FAC-312", Verdict: string(reviewledger.VerdictPASS)},
	}
	f, err := os.Create(ledgerPath)
	if err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatalf("write ledger row: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close ledger: %v", err)
	}
	if err := os.WriteFile(queuePath, nil, 0644); err != nil {
		t.Fatalf("create queue: %v", err)
	}

	idx := New(IndexOptions{RepoRoot: dir, LedgerPath: ledgerPath, QueuePath: queuePath})
	for run := 0; run < 100; run++ {
		cands, err := idx.BuildIndex(context.Background())
		if err != nil {
			t.Fatalf("BuildIndex run %d: %v", run, err)
		}
		if len(cands) != 1 {
			t.Fatalf("run %d: expected one candidate, got %d", run, len(cands))
		}
		if got := cands[0].CandidateSHA; got != newSHA {
			t.Fatalf("run %d: expected newest SHA %s, got %s", run, newSHA, got)
		}
		if got := cands[0].Verdict; got != string(reviewledger.VerdictPASS) {
			t.Fatalf("run %d: expected newest PASS verdict, got %s", run, got)
		}
		if got := cands[0].State; got != StateEligible {
			t.Fatalf("run %d: expected newest candidate eligible, got %s", run, got)
		}
	}
}

func TestCandidateIndex_WorktreeHEADSupersedesStaleBlockedCallback(t *testing.T) {
	dir := t.TempDir()
	mailPath := filepath.Join(dir, "mail.jsonl")
	worktreesDir := filepath.Join(dir, "worktrees")
	worktree := filepath.Join(worktreesDir, "fac-304")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-C", worktree}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("config", "user.email", "test@example.invalid")
	runGit("config", "user.name", "candidate-index-test")
	if err := os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("current\n"), 0644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	runGit("add", "candidate.txt")
	runGit("commit", "-qm", "current candidate")
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve current candidate: %v", err)
	}
	currentSHA := strings.TrimSpace(string(out))
	anchorSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	callbackBody, err := json.Marshal(mail.Callback{
		Ref:             "FAC-304",
		Kind:            mail.CallbackBlocked,
		SHA:             anchorSHA,
		Detail:          "stale anchor callback",
		LeaseGeneration: 10,
	})
	if err != nil {
		t.Fatalf("marshal callback: %v", err)
	}
	mailFile, err := os.Create(mailPath)
	if err != nil {
		t.Fatalf("create mail: %v", err)
	}
	if err := json.NewEncoder(mailFile).Encode(mail.Envelope{
		ID:        "stale-anchor",
		Sequence:  1,
		Sender:    "worker",
		Recipient: mail.CoordinatorInbox,
		Subject:   "blocked: FAC-304",
		Body:      string(callbackBody),
		Timestamp: time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("write callback: %v", err)
	}
	if err := mailFile.Close(); err != nil {
		t.Fatalf("close mail: %v", err)
	}

	idx := New(IndexOptions{RepoRoot: dir, MailPath: mailPath, WorktreesDir: worktreesDir})
	cands, err := idx.BuildIndex(context.Background())
	if err != nil {
		t.Fatalf("BuildIndex failed: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected one candidate, got %d", len(cands))
	}
	c := cands[0]
	if c.CandidateSHA != currentSHA {
		t.Fatalf("expected live worktree SHA %s, got %s", currentSHA, c.CandidateSHA)
	}
	if c.State == StateBlocked || len(c.BlockedReasons) != 0 {
		t.Fatalf("stale callback blocked current candidate: state=%s reasons=%v evidence=%v", c.State, c.BlockedReasons, c.BlockedEvidence)
	}
	if !containsCandidateSource(c.Sources, SourceWorktree) {
		t.Fatalf("expected worktree provenance, got %v", c.Sources)
	}
	if c.WorktreePath != worktree {
		t.Fatalf("expected worktree path %s, got %s", worktree, c.WorktreePath)
	}
}

func TestCandidateIndex_SelectsNewestAcrossCallbackLedgerAndWorktree(t *testing.T) {
	dir := t.TempDir()
	mailPath := filepath.Join(dir, "mail.jsonl")
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	queuePath := filepath.Join(dir, "queue.jsonl")
	worktree := filepath.Join(dir, "worktrees", "fac-312")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-C", worktree}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2026-08-15T00:00:00Z", "GIT_COMMITTER_DATE=2026-08-15T00:00:00Z")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("config", "user.email", "test@example.invalid")
	runGit("config", "user.name", "candidate-index-test")
	if err := os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("old worktree candidate\n"), 0644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	runGit("add", "candidate.txt")
	runGit("commit", "-qm", "old worktree candidate")
	worktreeOut, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve worktree candidate: %v", err)
	}
	worktreeSHA := strings.TrimSpace(string(worktreeOut))

	callbackSHA := "1111111111111111111111111111111111111111"
	ledgerSHA := "2222222222222222222222222222222222222222"
	callbackBody, err := json.Marshal(mail.Callback{Ref: "FAC-312", Kind: mail.CallbackBlocked, SHA: callbackSHA, Detail: "old callback failure"})
	if err != nil {
		t.Fatalf("marshal callback: %v", err)
	}
	callbackFile, err := os.Create(mailPath)
	if err != nil {
		t.Fatalf("create mail: %v", err)
	}
	if err := json.NewEncoder(callbackFile).Encode(mail.Envelope{ID: "old-callback", Sequence: 1, Sender: "worker", Recipient: mail.CoordinatorInbox, Body: string(callbackBody), Timestamp: time.Date(2026, 8, 16, 1, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("write callback: %v", err)
	}
	if err := callbackFile.Close(); err != nil {
		t.Fatalf("close mail: %v", err)
	}

	ledgerFile, err := os.Create(ledgerPath)
	if err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	if err := json.NewEncoder(ledgerFile).Encode(reviewledger.LedgerRow{Timestamp: "2026-08-16T02:00:00Z", Event: string(reviewledger.EventVerdict), SHA: ledgerSHA, Task: "FAC-312", Verdict: string(reviewledger.VerdictPASS)}); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	if err := ledgerFile.Close(); err != nil {
		t.Fatalf("close ledger: %v", err)
	}
	if err := os.WriteFile(queuePath, nil, 0644); err != nil {
		t.Fatalf("create queue: %v", err)
	}

	idx := New(IndexOptions{RepoRoot: dir, MailPath: mailPath, LedgerPath: ledgerPath, QueuePath: queuePath, WorktreesDir: filepath.Join(dir, "worktrees")})
	for run := 0; run < 100; run++ {
		cands, err := idx.BuildIndex(context.Background())
		if err != nil {
			t.Fatalf("BuildIndex run %d: %v", run, err)
		}
		if len(cands) != 1 {
			t.Fatalf("run %d: expected one candidate, got %d", run, len(cands))
		}
		c := cands[0]
		if c.CandidateSHA != ledgerSHA {
			t.Fatalf("run %d: expected newest ledger SHA %s, got %s (worktree=%s)", run, ledgerSHA, c.CandidateSHA, worktreeSHA)
		}
		if c.State != StateEligible || c.Verdict != string(reviewledger.VerdictPASS) {
			t.Fatalf("run %d: newest candidate lost its PASS evidence: state=%s verdict=%s", run, c.State, c.Verdict)
		}
		if len(c.BlockedReasons) != 0 || len(c.BlockedEvidence) != 0 {
			t.Fatalf("run %d: old callback evidence leaked onto newest SHA: reasons=%v evidence=%v", run, c.BlockedReasons, c.BlockedEvidence)
		}
	}
}

func TestCandidateIndex_LaterCompleteCallbackClearsSameLeaseBlock(t *testing.T) {
	dir := t.TempDir()
	mailPath := filepath.Join(dir, "mail.jsonl")
	sha := "a0e39c7b67456121199180aba5fb758c9e03bf32"
	mailFile, err := os.Create(mailPath)
	if err != nil {
		t.Fatalf("create mail: %v", err)
	}
	writeCallback := func(sequence int64, kind mail.CallbackKind, detail string) {
		t.Helper()
		body, marshalErr := json.Marshal(mail.Callback{
			Ref:             "FAC-312",
			Kind:            kind,
			SHA:             sha,
			Detail:          detail,
			LeaseGeneration: 2,
		})
		if marshalErr != nil {
			t.Fatalf("marshal callback: %v", marshalErr)
		}
		if encodeErr := json.NewEncoder(mailFile).Encode(mail.Envelope{
			ID:        fmt.Sprintf("callback-%d", sequence),
			Sequence:  sequence,
			Sender:    "worker",
			Recipient: mail.CoordinatorInbox,
			Subject:   string(kind) + ": FAC-312",
			Body:      string(body),
			Timestamp: time.Unix(sequence, 0).UTC(),
		}); encodeErr != nil {
			t.Fatalf("write callback: %v", encodeErr)
		}
	}
	writeCallback(10, mail.CallbackBlocked, "transient test failure")
	writeCallback(11, mail.CallbackComplete, "")
	if err := mailFile.Close(); err != nil {
		t.Fatalf("close mail: %v", err)
	}

	cands, err := New(IndexOptions{RepoRoot: dir, MailPath: mailPath}).BuildIndex(context.Background())
	if err != nil {
		t.Fatalf("BuildIndex failed: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected one candidate, got %d", len(cands))
	}
	c := cands[0]
	if c.CandidateSHA != sha || c.LeaseGeneration != 2 {
		t.Fatalf("candidate identity changed while coalescing callbacks: %+v", c)
	}
	if c.State == StateBlocked || len(c.BlockedReasons) != 0 || len(c.BlockedEvidence) != 0 {
		t.Fatalf("later complete callback did not clear same-lease block: state=%s reasons=%v evidence=%v", c.State, c.BlockedReasons, c.BlockedEvidence)
	}
}

func TestCandidateIndex_DefaultCallbackMailboxClearsBlockedEvidence(t *testing.T) {
	dir := t.TempDir()
	mailPath := mail.CallbackMailPath(dir)
	sha := "b0e39c7b67456121199180aba5fb758c9e03bf32"
	if err := os.MkdirAll(filepath.Dir(mailPath), 0o755); err != nil {
		t.Fatalf("create callback mailbox directory: %v", err)
	}
	mailFile, err := os.Create(mailPath)
	if err != nil {
		t.Fatalf("create canonical callback mailbox: %v", err)
	}
	writeCallback := func(sequence int64, kind mail.CallbackKind, detail string) {
		t.Helper()
		body, marshalErr := json.Marshal(mail.Callback{
			Ref:             "FAC-360",
			Kind:            kind,
			SHA:             sha,
			Detail:          detail,
			LeaseGeneration: 2,
		})
		if marshalErr != nil {
			t.Fatalf("marshal callback: %v", marshalErr)
		}
		if encodeErr := json.NewEncoder(mailFile).Encode(mail.Envelope{
			ID:        fmt.Sprintf("fac-360-callback-%d", sequence),
			Sequence:  sequence,
			Sender:    "worker",
			Recipient: mail.CoordinatorInbox,
			Subject:   string(kind) + ": FAC-360",
			Body:      string(body),
			Timestamp: time.Unix(sequence, 0).UTC(),
		}); encodeErr != nil {
			t.Fatalf("write callback: %v", encodeErr)
		}
	}
	writeCallback(10, mail.CallbackBlocked, "transient test failure")
	writeCallback(11, mail.CallbackComplete, "")
	if err := mailFile.Close(); err != nil {
		t.Fatalf("close callback mailbox: %v", err)
	}

	cands, err := New(IndexOptions{RepoRoot: dir}).BuildIndex(context.Background())
	if err != nil {
		t.Fatalf("BuildIndex failed: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected one candidate from the canonical callback mailbox, got %d", len(cands))
	}
	c := cands[0]
	if c.CandidateSHA != sha || c.LeaseGeneration != 2 {
		t.Fatalf("candidate identity changed while coalescing canonical callbacks: %+v", c)
	}
	if c.State == StateBlocked || len(c.BlockedReasons) != 0 || len(c.BlockedEvidence) != 0 {
		t.Fatalf("complete callback did not clear same-lease block on canonical mailbox: state=%s reasons=%v evidence=%v", c.State, c.BlockedReasons, c.BlockedEvidence)
	}
}

func TestCandidateIndex_LaterGenerationClearsEarlierSameSHABlockedCallback(t *testing.T) {
	dir := t.TempDir()
	mailPath := filepath.Join(dir, "mail.jsonl")
	sha := "46be267dd2cc0a42acb70141838d0e3f5645605b"
	base := "b42b69763598436c29d49e7922b27f011ae168a5"
	mailFile, err := os.Create(mailPath)
	if err != nil {
		t.Fatalf("create mail: %v", err)
	}
	writeCallback := func(sequence, generation int64, kind mail.CallbackKind, detail string) {
		t.Helper()
		body, marshalErr := json.Marshal(mail.Callback{
			Ref: "FAC-618", Kind: kind, SHA: sha, Detail: detail,
			LeaseGeneration: generation,
		})
		if marshalErr != nil {
			t.Fatalf("marshal callback: %v", marshalErr)
		}
		if encodeErr := json.NewEncoder(mailFile).Encode(mail.Envelope{
			ID: fmt.Sprintf("fac-618-callback-%d", sequence), Sequence: sequence,
			Sender: "worker", Recipient: mail.CoordinatorInbox,
			Subject: string(kind) + ": FAC-618", Body: string(body),
			Timestamp: time.Unix(sequence, 0).UTC(),
		}); encodeErr != nil {
			t.Fatalf("write callback: %v", encodeErr)
		}
	}
	writeCallback(620, 1, mail.CallbackBlocked, "generation 1 failed")
	writeCallback(621, 2, mail.CallbackComplete, "generation 2 complete")
	if err := mailFile.Close(); err != nil {
		t.Fatalf("close mail: %v", err)
	}

	receipt := verifier.Receipt{
		Version: 1, TaskRef: "FAC-618", LeaseGeneration: "2",
		CandidateSHA: sha, BaseSHA: base, Command: []string{"go", "test", "./..."},
		ExitCode: 0, Outcome: verifier.OutcomePASS,
	}
	receipt.Digest = receipt.ComputeDigest()
	receiptDir := filepath.Join(dir, ".herd", "verification-receipts")
	if err := os.MkdirAll(receiptDir, 0700); err != nil {
		t.Fatalf("create receipt dir: %v", err)
	}
	receiptData, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(receiptDir, receipt.Digest[len("sha256:"):]+".json"), receiptData, 0600); err != nil {
		t.Fatalf("write receipt: %v", err)
	}

	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	ledgerFile, err := os.Create(ledgerPath)
	if err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	if err := json.NewEncoder(ledgerFile).Encode(reviewledger.LedgerRow{
		Timestamp: "2026-09-02T00:00:00Z", Event: string(reviewledger.EventVerdict),
		SHA: sha, Task: "FAC-618", Verdict: string(reviewledger.VerdictPASS),
	}); err != nil {
		t.Fatalf("write verdict: %v", err)
	}
	if err := ledgerFile.Close(); err != nil {
		t.Fatalf("close ledger: %v", err)
	}

	cands, err := New(IndexOptions{RepoRoot: dir, MailPath: mailPath, LedgerPath: ledgerPath}).BuildIndex(context.Background())
	if err != nil {
		t.Fatalf("BuildIndex failed: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected one candidate, got %d", len(cands))
	}
	c := cands[0]
	if c.CandidateSHA != sha || c.LeaseGeneration != 2 || !c.CompletionValid {
		t.Fatalf("generation 2 evidence was not selected: %+v", c)
	}
	if c.State != StateEligible || len(c.BlockedReasons) != 0 || len(c.BlockedEvidence) != 0 {
		t.Fatalf("generation 1 block contaminated completed generation 2: %+v", c)
	}
}

func writeFAC744CallbackMail(t *testing.T, mailPath string, callbacks []struct {
	sequence, generation int64
	kind                 mail.CallbackKind
	detail               string
}) {
	t.Helper()
	mailFile, err := os.Create(mailPath)
	if err != nil {
		t.Fatalf("create mail: %v", err)
	}
	defer func() {
		if closeErr := mailFile.Close(); closeErr != nil {
			t.Fatalf("close mail: %v", closeErr)
		}
	}()
	for _, cb := range callbacks {
		body, marshalErr := json.Marshal(mail.Callback{
			Ref: "FAC-744", Kind: cb.kind, SHA: "56be267dd2cc0a42acb70141838d0e3f5645605b",
			Detail: cb.detail, LeaseGeneration: cb.generation,
		})
		if marshalErr != nil {
			t.Fatalf("marshal callback: %v", marshalErr)
		}
		if encodeErr := json.NewEncoder(mailFile).Encode(mail.Envelope{
			ID: fmt.Sprintf("fac-744-callback-%d", cb.sequence), Sequence: cb.sequence,
			Sender: "worker", Recipient: mail.CoordinatorInbox,
			Subject: string(cb.kind) + ": FAC-744", Body: string(body),
			Timestamp: time.Unix(cb.sequence, 0).UTC(),
		}); encodeErr != nil {
			t.Fatalf("write callback: %v", encodeErr)
		}
	}
}

func writeFAC744Receipt(t *testing.T, dir, generation string, command []string, profileDigest, profileName, configRevision string) verifier.Receipt {
	t.Helper()
	receipt := verifier.Receipt{
		Version: 1, TaskRef: "FAC-744", LeaseGeneration: generation,
		CandidateSHA: "56be267dd2cc0a42acb70141838d0e3f5645605b",
		BaseSHA:      "b42b69763598436c29d49e7922b27f011ae168a5",
		Command:      command, ExitCode: 0, Outcome: verifier.OutcomePASS,
		ProfileDigest:       profileDigest,
		VerificationProfile: profileName,
		ConfigRevision:      configRevision,
	}
	receipt.Digest = receipt.ComputeDigest()
	receiptDir := filepath.Join(dir, ".herd", "verification-receipts")
	if err := os.MkdirAll(receiptDir, 0700); err != nil {
		t.Fatalf("create receipt dir: %v", err)
	}
	receiptData, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(receiptDir, receipt.Digest[len("sha256:"):]+".json"), receiptData, 0600); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	return receipt
}

func writeFAC744PassVerdict(t *testing.T, dir string) string {
	t.Helper()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	ledgerFile, err := os.Create(ledgerPath)
	if err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	if err := json.NewEncoder(ledgerFile).Encode(reviewledger.LedgerRow{
		Timestamp: "2026-09-02T00:00:00Z", Event: string(reviewledger.EventVerdict),
		SHA: "56be267dd2cc0a42acb70141838d0e3f5645605b", Task: "FAC-744",
		Verdict: string(reviewledger.VerdictPASS),
	}); err != nil {
		t.Fatalf("write verdict: %v", err)
	}
	if err := ledgerFile.Close(); err != nil {
		t.Fatalf("close ledger: %v", err)
	}
	return ledgerPath
}

// FAC-744 finding 3: a PASS receipt only counts as the exact full-suite
// completion proof when its command is the repository's authorized full-suite
// test command and any profile identity it carries matches the live profile.
// Targeted runs, scoped package subsets, and token lookalikes must not mark a
// completion valid.
func TestCandidateIndex_ReceiptAdmissionRequiresExactFullSuiteCommand(t *testing.T) {
	tests := []struct {
		name           string
		command        []string
		profileDigest  string
		profileName    string
		configRevision string
		wantAdmitted   bool
	}{
		{
			name:    "targeted run flag is not the full suite",
			command: []string{"go", "test", "./...", "-run", "TestOne"},
		},
		{
			name:    "extra scoped package arguments are not the full suite",
			command: []string{"go", "test", "./pkg/candidateindex", "./..."},
		},
		{
			name:    "token lookalike non-Go command is refused",
			command: []string{"echo", "go", "test", "./..."},
		},
		{
			name:          "foreign profile digest is refused",
			command:       []string{"go", "test", "./..."},
			profileDigest: "sha256:" + strings.Repeat("1", 64),
		},
		{
			name:        "foreign verification profile name is refused",
			command:     []string{"go", "test", "./..."},
			profileName: "not-the-verification-profile",
		},
		{
			name:           "foreign config revision is refused",
			command:        []string{"go", "test", "./..."},
			configRevision: "sha256:" + strings.Repeat("2", 64),
		},
		{
			name:         "exact authorized full-suite command is admitted",
			command:      []string{"go", "test", "./..."},
			wantAdmitted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			mailPath := filepath.Join(dir, "mail.jsonl")
			writeFAC744CallbackMail(t, mailPath, []struct {
				sequence, generation int64
				kind                 mail.CallbackKind
				detail               string
			}{{sequence: 621, generation: 2, kind: mail.CallbackComplete}})
			writeFAC744Receipt(t, dir, "2", tt.command, tt.profileDigest, tt.profileName, tt.configRevision)
			ledgerPath := writeFAC744PassVerdict(t, dir)

			cands, err := New(IndexOptions{RepoRoot: dir, MailPath: mailPath, LedgerPath: ledgerPath}).BuildIndex(context.Background())
			if err != nil {
				t.Fatalf("BuildIndex failed: %v", err)
			}
			if len(cands) != 1 {
				t.Fatalf("expected one candidate, got %d", len(cands))
			}
			c := cands[0]
			if tt.wantAdmitted {
				if !c.CompletionValid {
					t.Fatalf("exact full-suite receipt was not admitted: %+v", c)
				}
				if c.State == StateBlocked || len(c.BlockedReasons) != 0 {
					t.Fatalf("admitted receipt left candidate blocked: %+v", c)
				}
				return
			}
			if c.CompletionValid {
				t.Fatalf("non-full-suite receipt was admitted as the completion proof: command=%v", tt.command)
			}
		})
	}
}

func containsCandidateSource(sources []CandidateSource, want CandidateSource) bool {
	for _, source := range sources {
		if source == want {
			return true
		}
	}
	return false
}
