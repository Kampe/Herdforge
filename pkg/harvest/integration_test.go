package harvest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// Hermetic admission fixtures. Task/lease/patch/session must match what
// Admit binds; author family is deliberately different from reviewer family.
const (
	admitTestLease    = "lease-gen-1"
	admitTestPatch    = "https://patch.example/harvest-fac-149"
	admitTestDigest   = "sha256:deadbeefcafebabe000000000000000000000000000000000000000000000001"
	admitTestAuthorFm = "anthropic"
	admitTestAuthorID = "author-session-1"
	admitTestReviewFm = "google"
	admitTestReviewer = "reviewer-1"
	admitTestTier     = "R2"
)

// -- test helpers --

func gitInHarvest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(dir, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFileHarvest(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func addAndCommitHarvest(t *testing.T, dir, msg string, files ...string) string {
	t.Helper()
	args := append([]string{"add"}, files...)
	gitInHarvest(t, dir, args...)
	gitInHarvest(t, dir, "commit", "-q", "-m", msg)
	return gitInHarvest(t, dir, "rev-parse", "HEAD")
}

// setupRepoWithRemote creates a root repo with a bare remote, pushes a base
// commit, and configures local git identity so cherry-picks from the
// Integration (which calls git directly, not through gitInHarvest) work.
func setupRepoWithRemote(t *testing.T) (root, remote string) {
	t.Helper()
	root = t.TempDir()
	remote = t.TempDir()

	gitInHarvest(t, remote, "init", "--bare", "-q", "-b", "main")

	gitInHarvest(t, root, "init", "-q", "-b", "main")
	gitInHarvest(t, root, "config", "user.email", "t@h.local")
	gitInHarvest(t, root, "config", "user.name", "t")
	gitInHarvest(t, root, "config", "commit.gpgSign", "false")
	gitInHarvest(t, root, "commit", "--allow-empty", "-q", "-m", "base")
	gitInHarvest(t, root, "remote", "add", "origin", remote)
	gitInHarvest(t, root, "push", "-q", "origin", "main")

	return root, remote
}

// setupLedger creates a reviewledger.Ledger in the root repo's .herd directory.
func setupLedger(t *testing.T, root string) *reviewledger.Ledger {
	t.Helper()
	ledgerPath := filepath.Join(root, ".herd", "review-ledger.jsonl")
	l, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatalf("NewReviewLedger: %v", err)
	}
	return l
}

// testAdmission returns the caller-asserted Admit context for task.
func testAdmission(task string) AdmissionContext {
	return AdmissionContext{
		Task:           task,
		Lease:          admitTestLease,
		PatchURL:       admitTestPatch,
		AuthorFamily:   admitTestAuthorFm,
		AuthorIdentity: admitTestAuthorID,
	}
}

// withAdmit returns Integration options that bind StaticAdmissionSource for task.
func withAdmit(task string) IntegrationOption {
	return WithAdmissionSource(StaticAdmissionSource{Context: testAdmission(task)})
}

// recordPass writes a launch + independent PASS that satisfies Admit for task.
func recordPass(t *testing.T, l *reviewledger.Ledger, sha, task string) {
	t.Helper()
	if err := l.Record(reviewledger.RecordOpts{
		SHA:             sha,
		Branch:          "main",
		Reviewer:        admitTestReviewer,
		BuilderFamily:   admitTestAuthorFm,
		BuilderIdentity: admitTestAuthorID,
		Gate:            "independent",
		Tier:            admitTestTier,
		Task:            task,
		Lease:           admitTestLease,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA:            sha,
		Reviewer:       admitTestReviewer,
		Verdict:        reviewledger.VerdictPASS,
		ReviewerFamily: admitTestReviewFm,
		Task:           task,
		Lease:          admitTestLease,
		PatchURL:       admitTestPatch,
		VfyDigest:      admitTestDigest,
		CandidateSHA:   sha,
	}); err != nil {
		t.Fatalf("Verdict: %v", err)
	}
}

// createWorktree creates a worktree with a branch and returns its path.
func createWorktree(t *testing.T, root, branch string) string {
	t.Helper()
	wtDir := filepath.Join(filepath.Dir(root), "wt-"+strings.ReplaceAll(branch, "/", "-"))
	absWT, err := filepath.Abs(wtDir)
	if err != nil {
		t.Fatal(err)
	}
	gitInHarvest(t, root, "worktree", "add", "-q", "-b", branch, absWT)
	return absWT
}

// -- test fakes --

type recordingDispatcher struct {
	calls     []dispatchCall
	failOnRef string
}

type dispatchCall struct {
	Ref         string
	EvidenceSHA string
}

func (d *recordingDispatcher) BoardComplete(_ context.Context, ref, evidenceSHA string) error {
	d.calls = append(d.calls, dispatchCall{Ref: ref, EvidenceSHA: evidenceSHA})
	if d.failOnRef != "" && ref == d.failOnRef {
		return fmt.Errorf("board refused for %s", ref)
	}
	return nil
}

type recordingVerifier struct {
	pass  bool
	calls int
}

func (v *recordingVerifier) Execute(_ context.Context, _ string) (*VerifyResult, error) {
	v.calls++
	result := "passed"
	if !v.pass {
		result = "failed"
	}
	return &VerifyResult{Passed: v.pass, Output: result}, nil
}

type recordingSessionManager struct {
	stopped []string
}

func (s *recordingSessionManager) Stop(_ context.Context, branch string) error {
	s.stopped = append(s.stopped, branch)
	return nil
}

// -- Tests --

func TestIntegrationDryRun(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-99-foo")
	writeFileHarvest(t, wt, "feat.go", "package main")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-99 initial implementation", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-99")

	h := NewHarvester(root)
	fd := &recordingDispatcher{}
	in := NewIntegration(h, nil, fd, l, root, WithDryRun(true), withAdmit("FAC-99"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.HarvestResult == nil {
		t.Fatal("expected HarvestResult")
	}
	if len(res.HarvestResult.UnmergedWorktrees) != 1 {
		t.Fatalf("expected 1 unmerged worktree, got %d", len(res.HarvestResult.UnmergedWorktrees))
	}
	if len(res.ReviewGatedSHAs) != 1 {
		t.Fatalf("expected 1 review gate outcome, got %d", len(res.ReviewGatedSHAs))
	}
	if !res.ReviewGatedSHAs[0].Eligible {
		t.Errorf("expected eligible=true, got false (err=%s reason=%s)", res.ReviewGatedSHAs[0].Err, res.ReviewGatedSHAs[0].Reason)
	}
	if len(res.MergedSHAs) != 0 {
		t.Errorf("dry run should not merge, got %d merged", len(res.MergedSHAs))
	}
	if len(fd.calls) != 0 {
		t.Errorf("dry run should not call board-complete, got %d calls", len(fd.calls))
	}
}

func TestIntegrationNoUnmerged(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	l := setupLedger(t, root)
	h := NewHarvester(root)
	in := NewIntegration(h, nil, &recordingDispatcher{}, l, root, withAdmit("FAC-0"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.HarvestResult.UnmergedWorktrees) != 0 {
		t.Fatalf("expected 0 unmerged, got %d", len(res.HarvestResult.UnmergedWorktrees))
	}
	if len(res.ReviewGatedSHAs) != 0 {
		t.Fatalf("expected 0 review outcomes, got %d", len(res.ReviewGatedSHAs))
	}
}

func TestIntegrationCleanMerge(t *testing.T) {
	ctx := context.Background()
	root, remote := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-100-clean")
	writeFileHarvest(t, wt, "feat.go", "package feat")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-100 clean merge", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-100")

	fd := &recordingDispatcher{}
	sm := &recordingSessionManager{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, WithSessionManager(sm), withAdmit("FAC-100"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.ReviewGatedSHAs) != 1 || !res.ReviewGatedSHAs[0].Eligible {
		t.Fatalf("expected 1 eligible SHA, got %+v", res.ReviewGatedSHAs)
	}
	if len(res.MergedSHAs) != 1 {
		t.Fatalf("expected 1 merged SHA, got %d", len(res.MergedSHAs))
	}
	mo := res.MergedSHAs[0]
	if !mo.Pushed {
		t.Error("expected Pushed=true")
	}
	if mo.MergeSHA == "" {
		t.Error("expected non-empty MergeSHA")
	}
	if mo.Conflict {
		t.Error("expected Conflict=false")
	}

	// Board-complete was called with FAC-100 and the merge SHA.
	if len(fd.calls) != 1 {
		t.Fatalf("expected 1 board-complete call, got %d", len(fd.calls))
	}
	if fd.calls[0].Ref != "FAC-100" {
		t.Errorf("expected ref FAC-100, got %s", fd.calls[0].Ref)
	}
	if fd.calls[0].EvidenceSHA != mo.MergeSHA {
		t.Errorf("expected evidence %s, got %s", mo.MergeSHA, fd.calls[0].EvidenceSHA)
	}

	// BoardCompletedRefs accumulates (not overwrites).
	if len(res.BoardCompletedRefs) != 1 || res.BoardCompletedRefs[0] != "FAC-100" {
		t.Errorf("expected BoardCompletedRefs=[FAC-100], got %v", res.BoardCompletedRefs)
	}

	// Session teardown was called.
	if len(sm.stopped) != 1 || sm.stopped[0] != "task/FAC-100-clean" {
		t.Errorf("expected session stop for task/FAC-100-clean, got %v", sm.stopped)
	}

	// Worktree was cleaned up.
	if len(res.CleanedWorktrees) != 1 {
		t.Fatalf("expected 1 cleaned worktree, got %d", len(res.CleanedWorktrees))
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed, still exists at %s", wt)
	}

	// Branch was deleted.
	if _, err := gitOutput(ctx, root, "rev-parse", "--verify", "-q", "task/FAC-100-clean"); err == nil {
		t.Error("branch task/FAC-100-clean should be deleted")
	}

	// Verify the commit is on origin/main in the remote.
	remoteOut, err := gitOutput(ctx, remote, "log", "--oneline", "-1")
	if err != nil {
		t.Fatalf("remote log: %v", err)
	}
	if !strings.Contains(remoteOut, "FAC-100") {
		t.Errorf("expected FAC-100 in remote log, got %s", remoteOut)
	}
}

func TestIntegrationConflict(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	// Create a base file that both sides will modify.
	writeFileHarvest(t, root, "shared.txt", "line1")
	gitInHarvest(t, root, "add", "shared.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "base: add shared.txt")
	gitInHarvest(t, root, "push", "-q", "origin", "main")

	// Worktree modifies shared.txt.
	wt := createWorktree(t, root, "task/FAC-101-conflict")
	writeFileHarvest(t, wt, "shared.txt", "line1\nworktree-change")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-101 worktree change", "shared.txt")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-101")

	// Root modifies shared.txt differently and pushes.
	writeFileHarvest(t, root, "shared.txt", "line1\nmain-change")
	gitInHarvest(t, root, "add", "shared.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "chore: main change")
	gitInHarvest(t, root, "push", "-q", "origin", "main")

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-101"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.MergedSHAs) != 0 {
		t.Fatalf("expected 0 merged SHAs (conflict), got %d", len(res.MergedSHAs))
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected at least one error for the conflict")
	}
	foundConflict := false
	for _, e := range res.Errors {
		if strings.Contains(e, "conflict") {
			foundConflict = true
			break
		}
	}
	if !foundConflict {
		t.Errorf("expected a conflict error, got %v", res.Errors)
	}
	if len(fd.calls) != 0 {
		t.Errorf("expected no board-complete calls on conflict, got %d", len(fd.calls))
	}

	// Worktree should NOT be cleaned up (merge failed).
	if len(res.CleanedWorktrees) != 0 {
		t.Errorf("expected no cleanup on conflict, got %v", res.CleanedWorktrees)
	}
}

func TestIntegrationStaleReview(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-102-stale")
	writeFileHarvest(t, wt, "feat.go", "package stale")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-102 no review", "feat.go")

	// Ledger exists but has NO PASS verdict for this SHA.
	l := setupLedger(t, root)

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-102"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.ReviewGatedSHAs) != 1 {
		t.Fatalf("expected 1 review gate outcome, got %d", len(res.ReviewGatedSHAs))
	}
	if res.ReviewGatedSHAs[0].Eligible {
		t.Error("expected eligible=false (no PASS verdict in ledger)")
	}
	if res.ReviewGatedSHAs[0].Err == "" && res.ReviewGatedSHAs[0].Reason == "" {
		t.Error("expected non-empty error/reason from Admit refusal")
	}
	if len(res.MergedSHAs) != 0 {
		t.Errorf("expected 0 merges (review rejected), got %d", len(res.MergedSHAs))
	}
	if len(fd.calls) != 0 {
		t.Errorf("expected no board-complete calls, got %d", len(fd.calls))
	}

	// Worktree should NOT be cleaned up (no merge).
	if len(res.CleanedWorktrees) != 0 {
		t.Errorf("expected no cleanup, got %v", res.CleanedWorktrees)
	}

	// Verify the SHA is the one we committed.
	if res.ReviewGatedSHAs[0].SHA != sha {
		t.Errorf("expected SHA %s, got %s", sha, res.ReviewGatedSHAs[0].SHA)
	}
}

func TestIntegrationBoardFailure(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-103-board-fail")
	writeFileHarvest(t, wt, "feat.go", "package boardfail")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-103 board failure", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-103")

	fd := &recordingDispatcher{failOnRef: "FAC-103"}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-103"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Merge should succeed.
	if len(res.MergedSHAs) != 1 || !res.MergedSHAs[0].Pushed {
		t.Fatalf("expected 1 pushed merge, got %+v", res.MergedSHAs)
	}

	// Board-complete was called but failed.
	if len(fd.calls) != 1 {
		t.Fatalf("expected 1 board-complete call, got %d", len(fd.calls))
	}
	if fd.calls[0].Ref != "FAC-103" {
		t.Errorf("expected ref FAC-103, got %s", fd.calls[0].Ref)
	}

	// Error recorded for board failure.
	foundBoardErr := false
	for _, e := range res.Errors {
		if strings.Contains(e, "board-complete") {
			foundBoardErr = true
			break
		}
	}
	if !foundBoardErr {
		t.Errorf("expected board-complete error, got %v", res.Errors)
	}

	// BoardCompletedRefs should be empty (board failed).
	if len(res.BoardCompletedRefs) != 0 {
		t.Errorf("expected no board completed refs, got %v", res.BoardCompletedRefs)
	}

	// Worktree should NOT be cleaned up (board-complete failed, so
	// mergedByWorktree didn't increment for this SHA).
	if len(res.CleanedWorktrees) != 0 {
		t.Errorf("expected no cleanup on board failure, got %v", res.CleanedWorktrees)
	}
}

func TestIntegrationCleanupRefusesDirty(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-105-dirty")
	writeFileHarvest(t, wt, "feat.go", "package dirty")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-105 dirty cleanup", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-105")

	// Dirty the worktree with an untracked file BEFORE running.
	writeFileHarvest(t, wt, "uncommitted.txt", "uncommitted changes")

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-105"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Merge should still succeed (cherry-pick is in root, not worktree).
	if len(res.MergedSHAs) != 1 || !res.MergedSHAs[0].Pushed {
		t.Fatalf("expected 1 pushed merge, got %+v", res.MergedSHAs)
	}
	if len(res.BoardCompletedRefs) != 1 {
		t.Fatalf("expected board-complete, got %v", res.BoardCompletedRefs)
	}

	// Cleanup should refuse because the worktree is dirty.
	if len(res.CleanedWorktrees) != 0 {
		t.Fatalf("expected no cleanup (dirty worktree), got %v", res.CleanedWorktrees)
	}
	foundCleanupErr := false
	for _, e := range res.Errors {
		if strings.Contains(e, "dirty") || strings.Contains(e, "cleanup") {
			foundCleanupErr = true
			break
		}
	}
	if !foundCleanupErr {
		t.Errorf("expected a cleanup refusal error, got %v", res.Errors)
	}

	// Worktree should still exist.
	if _, err := os.Stat(wt); os.IsNotExist(err) {
		t.Error("worktree should still exist (cleanup refused)")
	}
}

func TestIntegrationAlreadyMerged(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-106-already")
	writeFileHarvest(t, wt, "feat.go", "package already")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-106 already merged", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-106")

	// Manually cherry-pick and push so the patch is on origin/main.
	gitInHarvest(t, root, "cherry-pick", sha)
	gitInHarvest(t, root, "push", "-q", "origin", "main")

	// Now harvest: git cherry compares patch content, so the SHA should
	// NOT appear as unmerged (it shows '-', not '+').
	h := NewHarvester(root)
	in := NewIntegration(h, nil, &recordingDispatcher{}, l, root, withAdmit("FAC-106"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.HarvestResult.UnmergedWorktrees) != 0 {
		t.Fatalf("expected 0 unmerged (patch already on origin/main), got %d", len(res.HarvestResult.UnmergedWorktrees))
	}
}

func TestIntegrationVerifierFails(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)
	originalRemote := gitInHarvest(t, root, "rev-parse", "origin/main")

	wt := createWorktree(t, root, "task/FAC-107-verifier")
	writeFileHarvest(t, wt, "feat.go", "package verifier")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-107 verifier fail", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-107")

	v := &recordingVerifier{pass: false}
	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, v, fd, l, root, withAdmit("FAC-107"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.MergedSHAs) != 0 {
		t.Fatalf("expected 0 merged (verifier failed), got %d", len(res.MergedSHAs))
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected error for verifier failure")
	}
	foundVerifierErr := false
	for _, e := range res.Errors {
		if strings.Contains(e, "verifier") {
			foundVerifierErr = true
			break
		}
	}
	if !foundVerifierErr {
		t.Errorf("expected verifier error, got %v", res.Errors)
	}
	if v.calls == 0 {
		t.Error("expected verifier to be called at least once")
	}
	if len(fd.calls) != 0 {
		t.Errorf("expected no board-complete calls, got %d", len(fd.calls))
	}

	// Replay state is preserved for explicit recovery; verifier failure never
	// resets or aborts the destination and never publishes downstream state.
	headMsg, _ := gitOutput(ctx, root, "log", "--oneline", "-1")
	if !strings.Contains(headMsg, "FAC-107") {
		t.Errorf("expected replay head to be preserved for recovery, got %s", headMsg)
	}
	if got := gitInHarvest(t, root, "rev-parse", "origin/main"); got != originalRemote {
		t.Errorf("verifier failure unexpectedly pushed: %s != %s", got, originalRemote)
	}
	if len(res.CleanedWorktrees) != 0 || len(res.BoardCompletedRefs) != 0 {
		t.Fatalf("verifier failure performed downstream mutation: %+v", res)
	}
	rows, rowErr := l.AllRows()
	if rowErr != nil {
		t.Fatal(rowErr)
	}
	for _, row := range rows {
		if row.Event == "consumed" {
			t.Fatalf("verifier failure consumed ledger row: %+v", row)
		}
	}
	paths, globErr := filepath.Glob(filepath.Join(root, ".herd", "replay-blocked-*.jsonl"))
	if globErr != nil || len(paths) != 1 {
		t.Fatalf("expected one durable blocked evidence file: %v %v", paths, globErr)
	}
	evidence, readErr := os.ReadFile(paths[0])
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(evidence), "verifier_failed") || strings.Contains(string(evidence), "passed") {
		t.Fatalf("unsafe or missing verifier evidence: %s", evidence)
	}
}

func TestIntegrationBatchConflictPublishesNoPrefix(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)
	writeFileHarvest(t, root, "shared.txt", "base")
	gitInHarvest(t, root, "add", "shared.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "batch base")
	gitInHarvest(t, root, "push", "-q", "origin", "main")
	wt := createWorktree(t, root, "task/FAC-181-batch-conflict")
	writeFileHarvest(t, wt, "first.txt", "first")
	first := addAndCommitHarvest(t, wt, "batch first", "first.txt")
	writeFileHarvest(t, wt, "shared.txt", "base\nsource")
	second := addAndCommitHarvest(t, wt, "batch second conflict", "shared.txt")
	writeFileHarvest(t, root, "shared.txt", "base\ndestination")
	gitInHarvest(t, root, "add", "shared.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "batch destination conflict")
	gitInHarvest(t, root, "push", "-q", "origin", "main")
	originalRemote := gitInHarvest(t, root, "rev-parse", "origin/main")
	l := setupLedger(t, root)
	recordPass(t, l, first, "FAC-181")
	recordPass(t, l, second, "FAC-181")
	fd := &recordingDispatcher{}
	in := NewIntegration(NewHarvester(root), nil, fd, l, root, withAdmit("FAC-181"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.MergedSHAs) != 0 || len(res.BoardCompletedRefs) != 0 || len(res.CleanedWorktrees) != 0 {
		t.Fatalf("batch conflict published downstream state: %+v", res)
	}
	if got := gitInHarvest(t, root, "rev-parse", "origin/main"); got != originalRemote {
		t.Fatalf("batch conflict pushed a prefix: %s != %s", got, originalRemote)
	}
	rows, rowErr := l.AllRows()
	if rowErr != nil {
		t.Fatal(rowErr)
	}
	for _, row := range rows {
		if row.Event == "consumed" {
			t.Fatalf("batch conflict consumed %s", row.SHA)
		}
	}
	if _, statErr := os.Stat(filepath.Join(root, ".git", "CHERRY_PICK_HEAD")); statErr != nil {
		t.Fatalf("batch conflict did not preserve sequencer evidence: %v", statErr)
	}
	repoID, repoIDErr := canonicalRepoIdentity(ctx, root)
	if repoIDErr != nil {
		t.Fatal(repoIDErr)
	}
	stateRel, evidenceRel, pathErr := replayArtifactPaths(ReplayRequest{
		TaskID: "FAC-181", RepoID: repoID,
		Generation: "integration/FAC-181/" + first, ExpectedHead: originalRemote,
	}, digestSources([]string{first, second}))
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	state, stateErr := loadReplayState(root, stateRel)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if state == nil || len(state.Items) != 2 {
		t.Fatalf("batch conflict checkpoint did not retain complete ordered boundary: %+v", state)
	}
	if state.Items[0].Source != first || state.Items[0].Classification != ReplayAppliedExact || state.Items[0].Matched == "" {
		t.Fatalf("batch conflict lost first exact mapping: %+v", state.Items[0])
	}
	if state.Items[1].Source != second || state.Items[1].Classification != ReplayUnresolved {
		t.Fatalf("batch conflict lost unresolved second mapping: %+v", state.Items[1])
	}
	evidence, evidenceErr := os.ReadFile(filepath.Join(root, evidenceRel))
	if evidenceErr != nil || !strings.Contains(string(evidence), second) || !strings.Contains(string(evidence), "unresolved") {
		t.Fatalf("batch conflict evidence lacks unresolved second source: %q %v", evidence, evidenceErr)
	}
}

func TestIntegrationLaterProofFailureHasNoDownstreamEffects(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)
	wt := createWorktree(t, root, "task/FAC-181-proof-failure")
	writeFileHarvest(t, wt, "one.txt", "one")
	first := addAndCommitHarvest(t, wt, "proof first", "one.txt")
	writeFileHarvest(t, wt, "two.txt", "two")
	second := addAndCommitHarvest(t, wt, "proof second", "two.txt")
	l := setupLedger(t, root)
	recordPass(t, l, first, "FAC-181")
	recordPass(t, l, second, "FAC-181")
	fd := &recordingDispatcher{}
	in := NewIntegration(NewHarvester(root), nil, fd, l, root, withAdmit("FAC-181"))
	proofCalls := 0
	in.readback = func(_ context.Context, _, _ string, sha string) (string, error) {
		proofCalls++
		if proofCalls == 1 && sha != "" {
			return "verified-first", nil
		}
		return "", fmt.Errorf("forced final readback failure for %s", sha)
	}
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if proofCalls != 2 {
		t.Fatalf("expected first proof to pass and second proof to fail, calls=%d", proofCalls)
	}
	if len(fd.calls) != 0 || len(res.BoardCompletedRefs) != 0 || len(res.CleanedWorktrees) != 0 {
		t.Fatalf("later proof failure performed downstream effects: result=%+v board=%+v", res, fd.calls)
	}
	rows, rowErr := l.AllRows()
	if rowErr != nil {
		t.Fatal(rowErr)
	}
	for _, row := range rows {
		if row.Event == "consumed" {
			t.Fatalf("later proof failure consumed ledger row: %+v", row)
		}
	}
}

func TestIntegrationFFOnlyAfterRemoteMove(t *testing.T) {
	ctx := context.Background()
	root, remote := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-108-ff")
	writeFileHarvest(t, wt, "feat.go", "package ff")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-108 ff-only test", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-108")

	// Simulate another agent pushing a commit to remote before we merge.
	other := t.TempDir()
	gitInHarvest(t, other, "clone", "-q", remote, other)
	gitInHarvest(t, other, "config", "user.email", "t@h.local")
	gitInHarvest(t, other, "config", "user.name", "t")
	gitInHarvest(t, other, "config", "commit.gpgSign", "false")
	writeFileHarvest(t, other, "other.txt", "other work")
	gitInHarvest(t, other, "add", "other.txt")
	gitInHarvest(t, other, "commit", "-q", "-m", "chore: other agent work")
	gitInHarvest(t, other, "push", "-q", "origin", "main")

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-108"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Merge should succeed: prepareMain fetches and ff-only merges the other
	// agent's commit, then our cherry-pick applies on top.
	if len(res.MergedSHAs) != 1 || !res.MergedSHAs[0].Pushed {
		t.Fatalf("expected 1 pushed merge, got %+v", res.MergedSHAs)
	}
	if len(res.BoardCompletedRefs) != 1 {
		t.Errorf("expected board-complete, got %v", res.BoardCompletedRefs)
	}

	// Both commits should be on the remote.
	remoteLog, _ := gitOutput(ctx, remote, "log", "--oneline")
	if !strings.Contains(remoteLog, "FAC-108") {
		t.Errorf("expected FAC-108 in remote log, got %s", remoteLog)
	}
	if !strings.Contains(remoteLog, "other agent") {
		t.Errorf("expected 'other agent' in remote log, got %s", remoteLog)
	}
}

func TestIntegrationLedgerConsumed(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-109-consumed")
	writeFileHarvest(t, wt, "feat.go", "package consumed")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-109 ledger consumed", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-109")

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-109"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.MergedSHAs) != 1 || !res.MergedSHAs[0].Pushed {
		t.Fatalf("expected 1 pushed merge, got %+v", res.MergedSHAs)
	}

	// Verify the ledger has a consumed event for this SHA.
	rows, err := l.AllRows()
	if err != nil {
		t.Fatalf("AllRows: %v", err)
	}
	foundConsumed := false
	for _, row := range rows {
		if row.Event == "consumed" && row.SHA == sha {
			foundConsumed = true
			if row.MergeSHA == "" {
				t.Error("consumed event has empty MergeSHA")
			}
			break
		}
	}
	if !foundConsumed {
		t.Error("expected a consumed event in the ledger for the merged SHA")
	}

	// Re-running should not re-merge (SHA is consumed → not eligible).
	fd2 := &recordingDispatcher{}
	in2 := NewIntegration(h, nil, fd2, l, root, withAdmit("FAC-109"))
	res2, err := in2.Run(ctx)
	if err != nil {
		t.Fatalf("Run2: %v", err)
	}
	// The worktree was cleaned up, so harvest should find 0 unmerged.
	// But even if the worktree existed, the consumed event would prevent
	// re-eligibility.
	if len(res2.MergedSHAs) != 0 {
		t.Errorf("expected 0 merges on re-run, got %d", len(res2.MergedSHAs))
	}
}

func TestIntegrationNoLedger(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-110-noledger")
	writeFileHarvest(t, wt, "feat.go", "package noledger")
	addAndCommitHarvest(t, wt, "feat: FAC-110 no ledger", "feat.go")

	h := NewHarvester(root)
	in := NewIntegration(h, nil, &recordingDispatcher{}, nil, root, withAdmit("FAC-110"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.ReviewGatedSHAs) != 1 {
		t.Fatalf("expected 1 review gate outcome, got %d", len(res.ReviewGatedSHAs))
	}
	if res.ReviewGatedSHAs[0].Eligible {
		t.Error("expected eligible=false (no ledger)")
	}
	if res.ReviewGatedSHAs[0].Err != "no review ledger configured" {
		t.Errorf("expected 'no review ledger configured', got %q", res.ReviewGatedSHAs[0].Err)
	}
	if len(res.MergedSHAs) != 0 {
		t.Errorf("expected 0 merges, got %d", len(res.MergedSHAs))
	}
}

func TestIntegrationNoAdmissionSource(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-111-noadmit")
	writeFileHarvest(t, wt, "feat.go", "package noadmit")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-111 no admission source", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-111")

	h := NewHarvester(root)
	// Deliberately omit WithAdmissionSource — fail closed.
	in := NewIntegration(h, nil, &recordingDispatcher{}, l, root)
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ReviewGatedSHAs) != 1 {
		t.Fatalf("expected 1 review gate outcome, got %d", len(res.ReviewGatedSHAs))
	}
	if res.ReviewGatedSHAs[0].Eligible {
		t.Error("expected eligible=false (no admission source)")
	}
	if res.ReviewGatedSHAs[0].Reason != "no admission context source configured" {
		t.Errorf("expected missing-source reason, got %q", res.ReviewGatedSHAs[0].Reason)
	}
	if len(res.MergedSHAs) != 0 {
		t.Errorf("expected 0 merges, got %d", len(res.MergedSHAs))
	}
}

func TestBranchToRef(t *testing.T) {
	tests := []struct {
		branch string
		want   string
	}{
		{"task/FAC-99-foo", "FAC-99"},
		{"task/KAN-123-implement", "KAN-123"},
		{"main", "main"},
		{"lane", "lane"},
		{"feature/abc", "feature/abc"},
	}
	for _, tt := range tests {
		t.Run(tt.branch, func(t *testing.T) {
			got := branchToRef(tt.branch)
			if got != tt.want {
				t.Errorf("branchToRef(%q) = %q, want %q", tt.branch, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// FAC-149: FAC-126 incident shape reproduced against Integration.Run.
// Author-family / prose / stale-SHA must never invoke merge even when the
// candidate sits on the live integration tree with session provenance bound.
// ---------------------------------------------------------------------------

// TestIntegrationFAC126_AuthorFamilyNeverMerges proves a same-family PASS
// (the FAC-126 self-verdict shape) does not make Integration.Run merge.
func TestIntegrationFAC126_AuthorFamilyNeverMerges(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-126-author-family")
	writeFileHarvest(t, wt, "feat.go", "package authorfamily")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-126 author-family self-verdict", "feat.go")

	l := setupLedger(t, root)
	// Launch + PASS where reviewer family == author family (self-verdict).
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Branch: "task/FAC-126-author-family",
		BuilderFamily: admitTestAuthorFm, BuilderIdentity: admitTestAuthorID,
		Reviewer: admitTestReviewer, Tier: admitTestTier, Task: "FAC-126",
		Lease: admitTestLease, Gate: "independent",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: admitTestReviewer, Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: admitTestAuthorFm, // same family as author — the incident
		Task:           "FAC-126", Lease: admitTestLease,
		PatchURL: admitTestPatch, VfyDigest: admitTestDigest, CandidateSHA: sha,
	}); err != nil {
		t.Fatalf("Verdict: %v", err)
	}

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-126"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertNeverMerged(t, res, fd, "self-verdict")
}

// TestIntegrationFAC126_ProseNeverMerges proves a prose-shaped "PASS" row
// and a still-working reviewer (launch only) never invoke merge.
func TestIntegrationFAC126_ProseNeverMerges(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	// Case A: reviewer still working — launch record, no structured verdict.
	wtA := createWorktree(t, root, "task/FAC-126-still-working")
	writeFileHarvest(t, wtA, "a.go", "package stillworking")
	shaA := addAndCommitHarvest(t, wtA, "feat: FAC-126 still working", "a.go")

	l := setupLedger(t, root)
	if err := l.Record(reviewledger.RecordOpts{
		SHA: shaA, Branch: "task/FAC-126-still-working",
		BuilderFamily: admitTestAuthorFm, BuilderIdentity: admitTestAuthorID,
		Reviewer: admitTestReviewer, Tier: admitTestTier, Task: "FAC-126",
		Lease: admitTestLease, Gate: "independent",
	}); err != nil {
		t.Fatalf("Record A: %v", err)
	}

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-126"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run A: %v", err)
	}
	assertNeverMerged(t, res, fd, "still working")
	if len(res.ReviewGatedSHAs) == 0 {
		t.Fatal("expected at least one review gate outcome for still-working case")
	}
	for _, rg := range res.ReviewGatedSHAs {
		if rg.Eligible {
			t.Fatalf("still-working must not be eligible: %+v", rg)
		}
		if !strings.Contains(rg.Reason, "still working") && !strings.Contains(rg.Reason, "no verdict") {
			// Accept either the "still working" reason or a broader refuse.
			if rg.Reason == "" {
				t.Errorf("expected structured refuse reason, got empty for %s", rg.SHA)
			}
		}
	}

	// Case B: prose-shaped verdict smuggled past ValidVerdict (PR-comment shape).
	// Append raw JSONL — mirrors FAC-146's prose incident fixture.
	wtB := createWorktree(t, root, "task/FAC-126-prose")
	writeFileHarvest(t, wtB, "b.go", "package prose")
	shaB := addAndCommitHarvest(t, wtB, "feat: FAC-126 prose PASS", "b.go")

	if err := l.Record(reviewledger.RecordOpts{
		SHA: shaB, Branch: "task/FAC-126-prose",
		BuilderFamily: admitTestAuthorFm, BuilderIdentity: admitTestAuthorID,
		Reviewer: admitTestReviewer, Tier: admitTestTier, Task: "FAC-126",
		Lease: admitTestLease, Gate: "independent",
	}); err != nil {
		t.Fatalf("Record B: %v", err)
	}
	// Direct file append of a prose "verdict" — not via Verdict() API.
	proseLine := fmt.Sprintf(
		`{"ts":"2026-08-02T00:00:00Z","event":"verdict","sha":%q,"reviewer":%q,"verdict":"PASS bound to %s","reviewer_family":%q,"task":"FAC-126","lease":%q,"patch_url":%q,"verification_digest":%q}`+"\n",
		shaB, admitTestReviewer, shaB, admitTestReviewFm, admitTestLease, admitTestPatch, admitTestDigest,
	)
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	if _, err := f.WriteString(proseLine); err != nil {
		t.Fatalf("write prose: %v", err)
	}
	f.Close()

	fd2 := &recordingDispatcher{}
	in2 := NewIntegration(h, nil, fd2, l, root, withAdmit("FAC-126"))
	res2, err := in2.Run(ctx)
	if err != nil {
		t.Fatalf("Run B: %v", err)
	}
	// Only care about shaB eligibility among outcomes.
	foundB := false
	for _, rg := range res2.ReviewGatedSHAs {
		if rg.SHA == shaB {
			foundB = true
			if rg.Eligible {
				t.Fatalf("prose verdict must not admit: %+v", rg)
			}
		}
	}
	if !foundB {
		t.Fatal("expected review gate outcome for prose SHA")
	}
	if len(res2.MergedSHAs) != 0 {
		t.Fatalf("prose must never merge, got %d", len(res2.MergedSHAs))
	}
	if len(fd2.calls) != 0 {
		t.Fatalf("prose must never board-complete, got %d", len(fd2.calls))
	}
}

// TestIntegrationFAC126_StaleSHANeverMerges: PASS bound to an old SHA does
// not admit a rebased tip (exact-candidate / current integration tree).
func TestIntegrationFAC126_StaleSHANeverMerges(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-126-stale")
	writeFileHarvest(t, wt, "feat.go", "package stale")
	oldSHA := addAndCommitHarvest(t, wt, "feat: FAC-126 old tip", "feat.go")

	l := setupLedger(t, root)
	// Valid Admit-satisfying PASS for the OLD tip only.
	recordPass(t, l, oldSHA, "FAC-126")

	// Rebase-shaped advance: new commit becomes the current tip. The old
	// PASS is still on disk, but Admit must refuse the new candidate SHA.
	writeFileHarvest(t, wt, "feat.go", "package stale\n// rebased")
	newSHA := addAndCommitHarvest(t, wt, "feat: FAC-126 rebased tip", "feat.go")
	if newSHA == oldSHA {
		t.Fatal("expected distinct new tip after rewrite")
	}

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-126"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Both SHAs may appear as unmerged; neither may be admitted for merge
	// under a stale-only ledger (old SHA has PASS but if still on branch
	// it could admit — wait: old SHA still has valid PASS and is still an
	// ancestor of HEAD, so Admit WOULD admit oldSHA. The incident shape is
	// that the *new* candidate (current tip) must not be admitted via the
	// old tip's PASS. Harvest lists every unmerged commit, so oldSHA might
	// still be mergeable if its PASS is valid.
	//
	// FAC-126's merge-of-wrong-thing was: treating prose/author PASS on a
	// request as authority for the live tip. Assert specifically that
	// newSHA is not eligible.
	newEligible := false
	for _, rg := range res.ReviewGatedSHAs {
		if rg.SHA == newSHA && rg.Eligible {
			newEligible = true
		}
	}
	if newEligible {
		t.Fatalf("stale-SHA PASS must not admit rebased tip %s; outcomes=%+v", newSHA, res.ReviewGatedSHAs)
	}
	// And the rebased tip must never appear in MergedSHAs.
	for _, mo := range res.MergedSHAs {
		if mo.SHA == newSHA {
			t.Fatalf("rebased tip must not merge: %+v", mo)
		}
	}
}

// TestIntegrationFAC126_OldFailPlusProseRequestNeverMerges is the full
// incident composite: independent FAIL on record, a new author prose/sentinel
// PASS request, and the independent reviewer still working. Integration.Run
// must never invoke merge.
func TestIntegrationFAC126_OldFailPlusProseRequestNeverMerges(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-126-composite")
	writeFileHarvest(t, wt, "feat.go", "package composite")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-126 composite incident", "feat.go")

	l := setupLedger(t, root)

	// 1. Old independent FAIL (veto) from reviewer-veto.
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Branch: "task/FAC-126-composite",
		BuilderFamily: admitTestAuthorFm, BuilderIdentity: admitTestAuthorID,
		Reviewer: "reviewer-veto", Tier: admitTestTier, Task: "FAC-126",
		Lease: admitTestLease, Gate: "independent",
	}); err != nil {
		t.Fatalf("Record veto: %v", err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: "reviewer-veto", Verdict: reviewledger.VerdictFAIL,
		ReviewerFamily: "xai", Task: "FAC-126", Lease: admitTestLease,
		PatchURL: admitTestPatch, VfyDigest: admitTestDigest, CandidateSHA: sha,
	}); err != nil {
		t.Fatalf("Verdict FAIL: %v", err)
	}

	// 2. New author-family "sentinel PASS" request (self-verdict launch + PASS).
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Branch: "task/FAC-126-composite",
		BuilderFamily: admitTestAuthorFm, BuilderIdentity: admitTestAuthorID,
		Reviewer: "author-sentinel", Tier: admitTestTier, Task: "FAC-126",
		Lease: admitTestLease, Gate: "independent",
	}); err != nil {
		t.Fatalf("Record sentinel: %v", err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: "author-sentinel", Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: admitTestAuthorFm, // author family — must not admit
		Task:           "FAC-126", Lease: admitTestLease,
		PatchURL: admitTestPatch, VfyDigest: admitTestDigest, CandidateSHA: sha,
	}); err != nil {
		t.Fatalf("Verdict sentinel: %v", err)
	}

	// 3. Independent reviewer still working — launch only, no verdict.
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Branch: "task/FAC-126-composite",
		BuilderFamily: admitTestAuthorFm, BuilderIdentity: admitTestAuthorID,
		Reviewer: admitTestReviewer, Tier: admitTestTier, Task: "FAC-126",
		Lease: admitTestLease, Gate: "independent",
	}); err != nil {
		t.Fatalf("Record still-working: %v", err)
	}

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-126"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertNeverMerged(t, res, fd, "FAC-126 composite")
	if len(res.ReviewGatedSHAs) != 1 {
		t.Fatalf("expected 1 review outcome, got %+v", res.ReviewGatedSHAs)
	}
	if res.ReviewGatedSHAs[0].Eligible {
		t.Fatalf("composite incident must refuse admission: %+v", res.ReviewGatedSHAs[0])
	}
}

// TestIntegrationAdmitValidDifferentFamilyMergesOnce is the positive
// control: a different-family exact task/lease/patch/digest/session receipt
// admits and merges exactly once; a second Run cannot re-admit.
func TestIntegrationAdmitValidDifferentFamilyMergesOnce(t *testing.T) {
	ctx := context.Background()
	root, remote := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-149-once")
	writeFileHarvest(t, wt, "feat.go", "package once")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-149 exact admit once", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "FAC-149")

	fd := &recordingDispatcher{}
	sm := &recordingSessionManager{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, WithSessionManager(sm), withAdmit("FAC-149"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ReviewGatedSHAs) != 1 || !res.ReviewGatedSHAs[0].Eligible {
		t.Fatalf("expected eligible, got %+v", res.ReviewGatedSHAs)
	}
	if len(res.MergedSHAs) != 1 || !res.MergedSHAs[0].Pushed {
		t.Fatalf("expected 1 pushed merge, got %+v", res.MergedSHAs)
	}
	if len(fd.calls) != 1 {
		t.Fatalf("expected 1 board-complete, got %d", len(fd.calls))
	}

	// Exactly-once: recreate a worktree at the same SHA-shaped work and
	// re-run — Consumed must refuse re-admission. (Worktree was cleaned;
	// re-seed a branch with an equivalent new commit that has no verdict.)
	// Stronger check: call Admit directly for the spent SHA.
	result, admitErr := l.Admit(reviewledger.AdmissionOpts{
		CandidateSHA:   sha,
		Task:           "FAC-149",
		Lease:          admitTestLease,
		PatchURL:       admitTestPatch,
		AuthorFamily:   admitTestAuthorFm,
		AuthorIdentity: admitTestAuthorID,
	})
	if result != nil && result.Admitted {
		t.Fatalf("second Admit after Consumed must refuse, got %+v err=%v", result, admitErr)
	}
	if result == nil || !strings.Contains(result.Reason, "consumed") {
		t.Fatalf("expected consumed refuse reason, got %+v err=%v", result, admitErr)
	}

	// Origin/main advanced with our merge.
	remoteLog, _ := gitOutput(ctx, remote, "log", "--oneline")
	if !strings.Contains(remoteLog, "FAC-149") {
		t.Errorf("expected FAC-149 on remote, got %s", remoteLog)
	}
}

// TestIntegrationAdmitStaleAfterOriginMainAdvance: when origin/main moves
// and the candidate is rebased onto a new tip without a fresh verdict, the
// new tip never merges. A verified current receipt (recordPass on new tip)
// is required.
func TestIntegrationAdmitStaleAfterOriginMainAdvance(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-149-rebase")
	writeFileHarvest(t, wt, "feat.go", "package rebase")
	oldSHA := addAndCommitHarvest(t, wt, "feat: FAC-149 pre-rebase", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, oldSHA, "FAC-149")

	// Advance origin/main with unrelated work.
	writeFileHarvest(t, root, "main-move.txt", "moved")
	gitInHarvest(t, root, "add", "main-move.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "chore: origin/main advanced")
	gitInHarvest(t, root, "push", "-q", "origin", "main")

	// Rebase worktree onto new main → new candidate SHA, old verdict stale.
	gitInHarvest(t, wt, "rebase", "origin/main")
	newSHA := gitInHarvest(t, wt, "rev-parse", "HEAD")
	if newSHA == oldSHA {
		t.Fatal("expected rebase to produce a new tip SHA")
	}

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-149"))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, rg := range res.ReviewGatedSHAs {
		if rg.SHA == newSHA && rg.Eligible {
			t.Fatalf("rebased tip without fresh verdict must not admit: %+v", rg)
		}
	}
	for _, mo := range res.MergedSHAs {
		if mo.SHA == newSHA {
			t.Fatalf("rebased tip must not merge without fresh receipt: %+v", mo)
		}
	}

	// Fresh different-family receipt for the current tip → merge proceeds.
	recordPass(t, l, newSHA, "FAC-149")
	fd2 := &recordingDispatcher{}
	in2 := NewIntegration(h, nil, fd2, l, root, withAdmit("FAC-149"))
	res2, err := in2.Run(ctx)
	if err != nil {
		t.Fatalf("Run2: %v", err)
	}
	found := false
	for _, mo := range res2.MergedSHAs {
		if mo.SHA == newSHA && mo.Pushed {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected merge of current tip with fresh receipt; gated=%+v merged=%+v",
			res2.ReviewGatedSHAs, res2.MergedSHAs)
	}
}

func assertNeverMerged(t *testing.T, res *IntegrationResult, fd *recordingDispatcher, label string) {
	t.Helper()
	if res == nil {
		t.Fatalf("%s: nil IntegrationResult", label)
	}
	if len(res.MergedSHAs) != 0 {
		t.Fatalf("%s: expected 0 merges, got %+v", label, res.MergedSHAs)
	}
	if len(fd.calls) != 0 {
		t.Fatalf("%s: expected 0 board-complete calls, got %d", label, len(fd.calls))
	}
	if len(res.CleanedWorktrees) != 0 {
		t.Fatalf("%s: expected no cleanup, got %v", label, res.CleanedWorktrees)
	}
	for _, rg := range res.ReviewGatedSHAs {
		if rg.Eligible {
			t.Fatalf("%s: SHA %s unexpectedly eligible (reason=%q)", label, rg.SHA, rg.Reason)
		}
	}
}

// TestRunReviewGate_CandidateNotOnCurrentIntegrationTreeRefuses is the
// mutation-proof gate for ensureCandidateOnBranch (FAC-149 acceptance:
// current-integration-tree guard must not be vacuous).
//
// Setup: a fully valid Admit-satisfying PASS exists for candidateSHA, and
// the caller asserts correct task/lease/session provenance — so Admit
// alone WOULD admit. The worktree HEAD has then advanced past that
// commit (reset to origin/main), so candidateSHA is no longer on the
// current integration tree.
//
// Deleting the ensureCandidateOnBranch call in runReviewGate turns this
// test red: the gate would fall through to Admit and report Eligible=true.
func TestRunReviewGate_CandidateNotOnCurrentIntegrationTreeRefuses(t *testing.T) {
	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-149-off-tree")
	writeFileHarvest(t, wt, "feat.go", "package offtree")
	candidateSHA := addAndCommitHarvest(t, wt, "feat: FAC-149 off-tree candidate", "feat.go")

	l := setupLedger(t, root)
	// Full valid receipt — Admit would pass if the tree guard were absent.
	// Shape matches recordPass/withAdmit: different-family reviewer, task,
	// lease, patch URL, verification digest, risk tier (AGENTS.md #2 probe).
	recordPass(t, l, candidateSHA, "FAC-149")

	// Keep an explicit ref so X stays reachable after the branch rewrite
	// (stale-but-once-valid verdict: object + ledger PASS still exist).
	gitInHarvest(t, root, "branch", "keep/FAC-149-off-tree-candidate", candidateSHA)

	// Rewrite the worktree branch so X is no longer an ancestor of HEAD
	// while remaining a resolvable commit via the keep/ ref.
	gitInHarvest(t, wt, "reset", "--hard", "origin/main")
	head := gitInHarvest(t, wt, "rev-parse", "HEAD")
	if head == candidateSHA {
		t.Fatal("reset left HEAD on candidate; cannot exercise off-tree guard")
	}
	if _, err := gitOutput(ctx, wt, "rev-parse", "--verify", "-q", candidateSHA+"^{commit}"); err != nil {
		t.Fatalf("candidate object must still resolve (keep/ ref) so Admit's exact-SHA match would still succeed without the tree guard: %v", err)
	}
	if err := runGit(ctx, wt, "merge-base", "--is-ancestor", candidateSHA, "HEAD"); err == nil {
		t.Fatal("candidate must not be ancestor of HEAD after reset")
	}

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, withAdmit("FAC-149"))
	uw := UnmergedWork{
		WorktreePath: wt,
		Branch:       "task/FAC-149-off-tree",
		// Stale harvest snapshot still offers X — the shape a live author
		// session can produce after rebase/amend out from under a verdict.
		Unmerged: []string{candidateSHA},
	}
	outcome := in.runReviewGate(ctx, candidateSHA, uw)

	if outcome.Eligible {
		t.Fatalf("off-tree candidate with valid Admit receipt must not be eligible; got %+v\n"+
			"(deleting ensureCandidateOnBranch makes Eligible=true here — that is the vacuity probe)",
			outcome)
	}
	if outcome.Reason != "candidate not current on integration tree" {
		t.Fatalf("reason = %q, want %q (err=%q)", outcome.Reason, "candidate not current on integration tree", outcome.Err)
	}
	if outcome.SHA != candidateSHA {
		t.Fatalf("outcome SHA = %s, want %s", outcome.SHA, candidateSHA)
	}

	// Full Integration.Run path: harvest will not list X (not on branch),
	// but even if a stale Unmerged list offered it, Run must never merge X.
	// Drive Run and assert X never appears in MergedSHAs.
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, mo := range res.MergedSHAs {
		if mo.SHA == candidateSHA {
			t.Fatalf("off-tree candidate must never appear in MergedSHAs: %+v", mo)
		}
	}
	if len(fd.calls) != 0 {
		t.Fatalf("off-tree candidate must never board-complete, got %d calls", len(fd.calls))
	}

	// Control: the SAME receipt admits when the candidate IS on HEAD again.
	// Proves refusal is from ensureCandidateOnBranch, not a broken Admit setup.
	gitInHarvest(t, wt, "reset", "--hard", candidateSHA)
	onTree := in.runReviewGate(ctx, candidateSHA, uw)
	if !onTree.Eligible {
		t.Fatalf("control: candidate on HEAD with valid receipt must admit, got %+v", onTree)
	}
}
