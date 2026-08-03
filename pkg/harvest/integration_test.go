package harvest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/review"
)

// -- test helpers --

func gitInHarvest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-c", "commit.gpgSign=false", "-c", "gpg.x509.program=false", "-c", "gpg.format=openpgp", "-c", "tag.gpgSign=false", "-c", "user.email=test@herdforge.local", "-c", "user.name=Test Runner"}
	gitArgs := append(base, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = dir
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

// setupLedger creates a review.Ledger in the root repo's .herd directory.
func setupLedger(t *testing.T, root string) *review.Ledger {
	t.Helper()
	ledgerPath := filepath.Join(root, ".herd", "review-ledger.jsonl")
	l, err := review.NewReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatalf("NewReviewLedger: %v", err)
	}
	return l
}

// recordPass writes a record + PASS verdict for a SHA in the ledger.
func recordPass(t *testing.T, l *review.Ledger, sha, reviewer, builderFamily string) {
	t.Helper()
	if err := l.Record(review.RecordOpts{
		SHA:           sha,
		Reviewer:      reviewer,
		BuilderFamily: builderFamily,
		Gate:          "independent",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := l.Verdict(review.VerdictOpts{
		SHA:           sha,
		Reviewer:      reviewer,
		Verdict:       review.VerdictPASS,
		BuilderFamily: builderFamily,
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
	recordPass(t, l, sha, "reviewer-1", "anthropic")

	h := NewHarvester(root)
	fd := &recordingDispatcher{}
	in := NewIntegration(h, nil, fd, l, root, WithDryRun(true))
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
		t.Errorf("expected eligible=true, got false (err=%s)", res.ReviewGatedSHAs[0].Err)
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
	in := NewIntegration(h, nil, &recordingDispatcher{}, l, root)
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
	recordPass(t, l, sha, "reviewer-1", "anthropic")

	fd := &recordingDispatcher{}
	sm := &recordingSessionManager{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root, WithSessionManager(sm))
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
	recordPass(t, l, sha, "reviewer-1", "anthropic")

	// Root modifies shared.txt differently and pushes.
	writeFileHarvest(t, root, "shared.txt", "line1\nmain-change")
	gitInHarvest(t, root, "add", "shared.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "chore: main change")
	gitInHarvest(t, root, "push", "-q", "origin", "main")

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root)
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
	in := NewIntegration(h, nil, fd, l, root)
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
	if res.ReviewGatedSHAs[0].Err == "" {
		t.Error("expected non-empty error from ledger refusal")
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
	recordPass(t, l, sha, "reviewer-1", "anthropic")

	fd := &recordingDispatcher{failOnRef: "FAC-103"}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root)
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
	recordPass(t, l, sha, "reviewer-1", "anthropic")

	// Dirty the worktree with an untracked file BEFORE running.
	writeFileHarvest(t, wt, "uncommitted.txt", "uncommitted changes")

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root)
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
	recordPass(t, l, sha, "reviewer-1", "anthropic")

	// Manually cherry-pick and push so the patch is on origin/main.
	gitInHarvest(t, root, "cherry-pick", sha)
	gitInHarvest(t, root, "push", "-q", "origin", "main")

	// Now harvest: git cherry compares patch content, so the SHA should
	// NOT appear as unmerged (it shows '-', not '+').
	h := NewHarvester(root)
	in := NewIntegration(h, nil, &recordingDispatcher{}, l, root)
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

	wt := createWorktree(t, root, "task/FAC-107-verifier")
	writeFileHarvest(t, wt, "feat.go", "package verifier")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-107 verifier fail", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "reviewer-1", "anthropic")

	v := &recordingVerifier{pass: false}
	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, v, fd, l, root)
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

	// Root main should be reset back to origin/main (no cherry-pick residue).
	// The cherry-pick was applied, verifier failed, then reset --hard origin/main.
	headMsg, _ := gitOutput(ctx, root, "log", "--oneline", "-1")
	if strings.Contains(headMsg, "FAC-107") {
		t.Errorf("expected FAC-107 to be reset from main, but HEAD is %s", headMsg)
	}
}

func TestIntegrationFFOnlyAfterRemoteMove(t *testing.T) {
	ctx := context.Background()
	root, remote := setupRepoWithRemote(t)

	wt := createWorktree(t, root, "task/FAC-108-ff")
	writeFileHarvest(t, wt, "feat.go", "package ff")
	sha := addAndCommitHarvest(t, wt, "feat: FAC-108 ff-only test", "feat.go")

	l := setupLedger(t, root)
	recordPass(t, l, sha, "reviewer-1", "anthropic")

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
	in := NewIntegration(h, nil, fd, l, root)
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
	recordPass(t, l, sha, "reviewer-1", "anthropic")

	fd := &recordingDispatcher{}
	h := NewHarvester(root)
	in := NewIntegration(h, nil, fd, l, root)
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
	in2 := NewIntegration(h, nil, fd2, l, root)
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
	in := NewIntegration(h, nil, &recordingDispatcher{}, nil, root)
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
