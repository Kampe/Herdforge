package next

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestEval_NoTasks(t *testing.T) {
	cfg := testConfig()
	tp := newTestProvider([]testTask{})
	p := NewNextPicker(cfg, tp)
	act, err := p.Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if act.Type != ActionClaim {
		t.Errorf("expected ActionClaim for empty board, got %s", act.Type)
	}
}

func TestEval_VerdictArtifacts(t *testing.T) {
	cfg := testConfig()
	tp := newTestProvider([]testTask{})
	tmp, _ := os.MkdirTemp("", "next-test-*")
	defer os.RemoveAll(tmp)
	p := NewNextPicker(cfg, tp)
	p.InboxDir = tmp

	os.WriteFile(tmp+"/test-verdict.md", []byte("verdict"), 0644)

	act, err := p.Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if act.Type != ActionIngest {
		t.Errorf("expected ActionIngest with verdict present, got %s", act.Type)
	}
}

func TestPendingVerdictsExcludesArtifactsAlreadyInLedger(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0755); err != nil {
		t.Fatal(err)
	}
	admittedSHA := "1111111111111111111111111111111111111111"
	freshSHA := "2222222222222222222222222222222222222222"
	if err := os.WriteFile(filepath.Join(inbox, "admitted-verdict.md"), []byte("sha: "+admittedSHA+"\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "fresh-verdict.md"), []byte("sha: "+freshSHA+"\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ledgerPath := filepath.Join(dir, "review-ledger.jsonl")
	ledger, err := reviewledger.NewReviewLedger(dir, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(reviewledger.RecordOpts{
		SHA:           admittedSHA,
		BuilderFamily: "openai",
		Task:          "FAC-463",
	}); err != nil {
		t.Fatal(err)
	}

	p := NewNextPicker(testConfig(), newTestProvider(nil))
	p.InboxDir = inbox
	p.LedgerPath = ledgerPath
	got := p.pendingVerdicts()
	if len(got) != 1 || got[0] != "fresh-verdict.md" {
		t.Fatalf("pending verdicts = %v, want only fresh-verdict.md", got)
	}
}

func TestPreviewClaimQueueSeparatesProvenanceBlocked(t *testing.T) {
	cfg := testConfig()
	tasks := []testTask{
		{ref: "FAC-9", status: "to-do", priority: "urgent", description: "missing fence"},
		{ref: "FAC-10", status: "to-do", priority: "high", description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-10\",\"task_id\":\"t10\",\"edges\":[]}\n```"},
	}
	preview, err := PreviewClaimQueue(context.Background(), newTestProvider(tasks), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Claimable != 1 || preview.ProvenanceBlocked != 1 || len(preview.BlockedRefs) != 1 || preview.BlockedRefs[0] != "FAC-9" {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if !strings.Contains(preview.Description(), "FAC-9") {
		t.Fatalf("description omitted blocked ref: %s", preview.Description())
	}
}

func TestPreviewClaimQueueRoleFilter(t *testing.T) {
	cases := []struct {
		name      string
		role      string
		claimable int
	}{
		{name: "forge smith", role: "forge-smith", claimable: 1},
		{name: "reviewer", role: "reviewer", claimable: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tasks := []testTask{
				{ref: "FAC-9", status: "to-do", priority: "high", labels: []string{"forge-smith"}, description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-9\",\"task_id\":\"t9\",\"edges\":[]}\n```"},
				{ref: "FAC-10", status: "to-do", priority: "high", labels: []string{"reviewer"}, description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-10\",\"task_id\":\"t10\",\"edges\":[]}\n```"},
			}
			preview, err := PreviewClaimQueue(context.Background(), newTestProvider(tasks), cfg, tc.role)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Claimable != tc.claimable || preview.ProvenanceBlocked != 0 {
				t.Fatalf("role filter selected wrong candidates: %+v", preview)
			}
		})
	}
}

func TestEval_ReviewAtCap(t *testing.T) {
	cfg := testConfig()
	// 3 tasks in review hits the cap of 3
	tp := newTestProvider([]testTask{
		{ref: "FAC-1", status: "review", priority: "high"},
		{ref: "FAC-2", status: "review", priority: "medium"},
		{ref: "FAC-3", status: "review", priority: "medium"},
	})
	p := NewNextPicker(cfg, tp)
	act, err := p.Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if act.Type != ActionReview {
		t.Errorf("expected ActionReview with 3 tasks in review (cap: 3), got %s", act.Type)
	}
}

func TestEval_NeedReview(t *testing.T) {
	cfg := testConfig()
	tp := newTestProvider([]testTask{
		{ref: "FAC-1", status: "in-progress", priority: "high"},
	})
	p := NewNextPicker(cfg, tp)
	act, err := p.Eval(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if act.Type != ActionClaim {
		t.Errorf("expected ActionClaim when in-progress task has no candidate SHA, got %s", act.Type)
	}
}

func TestSelftest(t *testing.T) {
	t.Skip("requires herd binary in PATH")
	cfg := testConfig()
	tp := newTestProvider([]testTask{})
	p := NewNextPicker(cfg, tp)
	if err := p.Selftest(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// FAC-664: the blocked refs were named and the claimable ones were not, so the
// line read "2 claimable, 7 blocked by ...: CHA-3193, CHA-3194, ...". An
// operator could see exactly what to REPAIR and nothing about what to DO. A
// count is reportable; only an identity is dispatchable.
func TestClaimPreviewNamesTheTasksAnOperatorCanActOn(t *testing.T) {
	p := ClaimPreview{
		Claimable:         2,
		ClaimableRefs:     []string{"CHA-100", "CHA-101"},
		ProvenanceBlocked: 1,
		BlockedRefs:       []string{"CHA-999"},
	}
	got := p.Description()
	for _, want := range []string{"CHA-100", "CHA-101", "CHA-999"} {
		if !strings.Contains(got, want) {
			t.Errorf("description must name %q so it can be dispatched: %q", want, got)
		}
	}
}

// The no-blockers path must name them too, or the healthy case is the one that
// stays undispatchable.
func TestClaimPreviewNamesClaimableEvenWithNoBlockers(t *testing.T) {
	got := ClaimPreview{Claimable: 1, ClaimableRefs: []string{"CHA-7"}}.Description()
	if !strings.Contains(got, "CHA-7") {
		t.Errorf("a clean queue must still name its claimable task: %q", got)
	}
}

// A long queue is truncated for readability, but a truncated list SAYS so: a
// silently capped list is a count wearing an identity's clothes.
func TestClaimPreviewTruncationAnnouncesItself(t *testing.T) {
	var refs []string
	for i := 0; i < 12; i++ {
		refs = append(refs, "CHA-"+string(rune('a'+i)))
	}
	got := ClaimPreview{Claimable: 12, ClaimableRefs: refs}.Description()
	if !strings.Contains(got, "+4 more") {
		t.Errorf("truncation must be stated, not silent: %q", got)
	}
}

// Zero claimable must not gain a stray suffix, and must keep saying it is a
// filter result rather than an idle queue (FAC-623).
func TestClaimPreviewZeroStillExplainsItself(t *testing.T) {
	got := ClaimPreview{Role: "scout-planner"}.Description()
	if !strings.Contains(got, "filter result") {
		t.Errorf("zero must stay explained as a filter result: %q", got)
	}
	if strings.Contains(got, ": ,") || strings.HasSuffix(strings.TrimSpace(got), ":") {
		t.Errorf("no dangling ref suffix on zero: %q", got)
	}
}
