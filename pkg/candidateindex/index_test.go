package candidateindex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
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
