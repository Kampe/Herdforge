package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/harvestmerge"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// A standing branch may contain deliberately conflicting history after the
// reviewed candidate. Scoped mode must not select that history; the default
// branch-wide mode must continue selecting it so harvestBody can self-abort.
func TestHarvestCommitsCandidateRangeExcludesOutOfScopeHistory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	gitCandidateTest(t, "init", "-q", "-b", "main")
	gitCandidateTest(t, "config", "user.email", "test@example.com")
	gitCandidateTest(t, "config", "user.name", "test")
	writeCandidateFile(t, "shared.txt", "base\n")
	gitCandidateTest(t, "add", "shared.txt")
	gitCandidateTest(t, "commit", "-q", "-m", "base")
	baseSHA := gitCandidateOutput(t, "rev-parse", "HEAD")
	gitCandidateTest(t, "branch", "standing/lane")
	gitCandidateTest(t, "checkout", "-q", "standing/lane")
	writeCandidateFile(t, "reviewed.txt", "reviewed\n")
	gitCandidateTest(t, "add", "reviewed.txt")
	gitCandidateTest(t, "commit", "-q", "-m", "reviewed")
	reviewed := gitCandidateOutput(t, "rev-parse", "HEAD")
	gitCandidateTest(t, "checkout", "-q", "main")
	writeCandidateFile(t, "shared.txt", "main landing change\n")
	gitCandidateTest(t, "add", "shared.txt")
	gitCandidateTest(t, "commit", "-q", "-m", "main landing")
	gitCandidateTest(t, "checkout", "-q", "standing/lane")
	writeCandidateFile(t, "shared.txt", "out-of-scope conflicting rewrite\n")
	gitCandidateTest(t, "add", "shared.txt")
	gitCandidateTest(t, "commit", "-q", "-m", "stale conflict")

	scoped, err := harvestCommits("main", "standing/lane", harvestmerge.CandidateRange{Base: baseSHA, SHA: reviewed})
	if err != nil {
		t.Fatalf("scoped selection: %v", err)
	}
	if len(scoped) != 1 || scoped[0] == "" {
		t.Fatalf("scoped commits = %v, want only reviewed commit", scoped)
	}
	unscoped, err := harvestCommits("main", "standing/lane", harvestmerge.CandidateRange{})
	if err != nil {
		t.Fatalf("unscoped selection: %v", err)
	}
	if len(unscoped) != 2 {
		t.Fatalf("unscoped commits = %v, want reviewed and out-of-scope commits", unscoped)
	}

	scopedDir := filepath.Join(t.TempDir(), "scoped")
	if err := harvestBody(scopedDir, "main", "harvest/scoped", scoped, false); err != nil {
		t.Fatalf("scoped harvest must apply cleanly: %v", err)
	}
	gitCandidateTest(t, "worktree", "remove", "--force", scopedDir)
	gitCandidateTest(t, "branch", "-D", "harvest/scoped")

	unscopedDir := filepath.Join(t.TempDir(), "unscoped")
	err = harvestBody(unscopedDir, "main", "harvest/unscoped", unscoped, false)
	if err == nil || !strings.Contains(err.Error(), "conflicted") {
		t.Fatalf("unscoped harvest must preserve conflict self-abort, got %v", err)
	}
	gitCandidateTest(t, "worktree", "remove", "--force", unscopedDir)
	gitCandidateTest(t, "branch", "-D", "harvest/unscoped")
}

func TestResolveHarvestCandidatePinsReviewedSHAAcrossTipDrift(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	gitCandidateTest(t, "init", "-q", "-b", "main")
	gitCandidateTest(t, "config", "user.email", "test@example.com")
	gitCandidateTest(t, "config", "user.name", "test")
	gitCandidateTest(t, "commit", "--allow-empty", "-q", "-m", "base")
	gitCandidateTest(t, "branch", "standing/lane")
	gitCandidateTest(t, "checkout", "-q", "standing/lane")
	writeCandidateFile(t, "reviewed.go", "package reviewed\n")
	gitCandidateTest(t, "add", "reviewed.go")
	gitCandidateTest(t, "commit", "-q", "-m", "reviewed")
	reviewed := gitCandidateOutput(t, "rev-parse", "HEAD")

	writeCandidateFile(t, "advanced.go", "package advanced\n")
	gitCandidateTest(t, "add", "advanced.go")
	gitCandidateTest(t, "commit", "-q", "-m", "advanced")
	advanced := gitCandidateOutput(t, "rev-parse", "HEAD")

	l, err := reviewledger.NewReviewLedger(root, filepath.Join(root, ".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	addCandidatePass(t, l, reviewed, "standing/lane")

	tests := []struct {
		name      string
		requested string
		eligible  bool
		wantSHA   string
	}{
		{name: "moved tip refuses without new pass", eligible: false, wantSHA: ""},
		{name: "exact reviewed pin remains eligible", requested: reviewed, eligible: true, wantSHA: reviewed},
		{name: "advanced tip is not promoted", requested: advanced, eligible: false, wantSHA: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveHarvestCandidateWithReconstructionAt(root, "standing/lane", tt.requested, "", "")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Eligible != tt.eligible || got.Pin.SHA != tt.wantSHA {
				t.Fatalf("report = %+v, want eligible=%t pin=%q", got, tt.eligible, tt.wantSHA)
			}
			if got.Tip != advanced || got.LastPassSHA != reviewed {
				t.Fatalf("provenance = tip %q last pass %q, want %q and %q", got.Tip, got.LastPassSHA, advanced, reviewed)
			}
		})
	}

	selected, err := harvestCommits("main", "standing/lane", harvestmerge.CandidateRange{Base: "main", SHA: reviewed})
	if err != nil {
		t.Fatalf("select pinned candidate: %v", err)
	}
	if len(selected) != 1 {
		t.Fatalf("pinned candidate selected %d commits, want exactly reviewed candidate: %v", len(selected), selected)
	}
	advancedFound, err := harvestCommits("main", "standing/lane", harvestmerge.CandidateRange{Base: "main", SHA: advanced})
	if err != nil {
		t.Fatalf("select advanced candidate: %v", err)
	}
	if len(advancedFound) != 2 {
		t.Fatalf("advanced candidate selected %d commits, want reviewed and advanced: %v", len(advancedFound), advancedFound)
	}
	if _, err := harvestCommits("", "standing/lane", harvestmerge.CandidateRange{SHA: reviewed}); err == nil {
		t.Fatal("pinned candidate with no base must refuse instead of widening selection")
	}

	addCandidatePass(t, l, advanced, "standing/lane")
	got, err := resolveHarvestCandidateWithReconstructionAt(root, "standing/lane", "", "", "")
	if err != nil {
		t.Fatalf("resolve after fresh pass: %v", err)
	}
	if !got.Eligible || got.Pin.SHA != advanced || got.LastPassSHA != advanced {
		t.Fatalf("fresh PASS did not admit exact new tip: %+v", got)
	}
	got, err = resolveHarvestCandidateWithReconstructionAt(root, "standing/lane", reviewed, "", "")
	if err != nil {
		t.Fatalf("resolve retained reviewed pin: %v", err)
	}
	if !got.Eligible || got.Pin.SHA != reviewed {
		t.Fatalf("exact reviewed pin lost eligibility after a later PASS: %+v", got)
	}
}

func TestVerifyHarvestSelectionUsesAssignedRepository(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"commit", "--allow-empty", "-q", "-m", "base"},
		{"branch", "candidate"},
		{"checkout", "-q", "candidate"},
	} {
		if out, err := testgit.Command(root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "candidate.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "candidate.txt"}, {"commit", "-q", "-m", "candidate"}} {
		if out, err := testgit.Command(root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	candidate := strings.TrimSpace(gitCandidateOutputAt(t, root, "rev-parse", "HEAD"))

	unrelated := t.TempDir()
	if out, err := testgit.Command(unrelated, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("initialize unrelated repository: %v\n%s", err, out)
	}
	t.Chdir(unrelated)
	if err := verifyHarvestSelection(root, "main", candidate, []string{candidate}); err != nil {
		t.Fatalf("verification must use assigned repository: %v", err)
	}
}

func TestResolveHarvestCandidateUsesCandidateSHAWhenQueueBranchIsReviewerTask(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	gitCandidateTest(t, "init", "-q", "-b", "main")
	gitCandidateTest(t, "config", "user.email", "test@example.com")
	gitCandidateTest(t, "config", "user.name", "test")
	gitCandidateTest(t, "commit", "--allow-empty", "-q", "-m", "base")
	gitCandidateTest(t, "branch", "standing/nft-data-engineer")
	gitCandidateTest(t, "checkout", "-q", "standing/nft-data-engineer")
	writeCandidateFile(t, "reviewed.go", "package reviewed\n")
	gitCandidateTest(t, "add", "reviewed.go")
	gitCandidateTest(t, "commit", "-q", "-m", "reviewed")
	reviewed := gitCandidateOutput(t, "rev-parse", "HEAD")

	l, err := reviewledger.NewReviewLedger(root, filepath.Join(root, ".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	const reviewerTask = "review-nftdataeng3"
	if err := l.Record(reviewledger.RecordOpts{
		SHA: reviewed, Branch: reviewerTask, Reviewer: "reviewer", BuilderFamily: "anthropic",
		ReviewerFamily: "openai", Gate: "independent", Tier: "R2",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: reviewed, Branch: reviewerTask, Reviewer: "reviewer", Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: "openai", BuilderFamily: "anthropic",
	}); err != nil {
		t.Fatalf("verdict: %v", err)
	}

	got, err := resolveHarvestCandidateWithReconstructionAt(root, "standing/nft-data-engineer", "", "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Eligible || got.Pin.SHA != reviewed || got.LastPassSHA != reviewed {
		t.Fatalf("report = %+v, want eligible reviewed candidate %s despite queue branch %q", got, reviewed, reviewerTask)
	}
}

func TestResolveHarvestCandidateAcceptsAttestedReconstruction(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	gitCandidateTest(t, "init", "-q", "-b", "main")
	gitCandidateTest(t, "config", "user.email", "test@example.com")
	gitCandidateTest(t, "config", "user.name", "test")
	gitCandidateTest(t, "commit", "--allow-empty", "-q", "-m", "base")
	gitCandidateTest(t, "branch", "standing/lane")
	gitCandidateTest(t, "checkout", "-q", "standing/lane")
	writeCandidateFile(t, "reviewed.go", "package reviewed\n")
	gitCandidateTest(t, "add", "reviewed.go")
	gitCandidateTest(t, "commit", "-q", "-m", "reviewed")
	reviewed := gitCandidateOutput(t, "rev-parse", "HEAD")
	gitCandidateTest(t, "checkout", "-q", "main")
	writeCandidateFile(t, "reconstructed.go", "package reconstructed\n")
	gitCandidateTest(t, "add", "reconstructed.go")
	gitCandidateTest(t, "commit", "-q", "-m", "reconstructed")
	reconstructed := gitCandidateOutput(t, "rev-parse", "HEAD")
	gitCandidateTest(t, "branch", "-f", "standing/lane", reconstructed)

	l, err := reviewledger.NewReviewLedger(root, filepath.Join(root, ".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	addCandidatePass(t, l, reviewed, "standing/lane")
	if _, err := resolveHarvestCandidateWithReconstructionAt(root, "standing/lane", reviewed, "", ""); err == nil {
		t.Fatal("default candidate resolution must retain the ancestry refusal")
	}

	got, err := resolveHarvestCandidateWithReconstructionAt(root, "standing/lane", reviewed, reconstructed, "reviewed content was reconstructed exactly")
	if err != nil {
		t.Fatalf("attested reconstruction: %v", err)
	}
	if !got.Eligible || got.Pin.SHA != reviewed || got.ReconstructedSHA != reconstructed {
		t.Fatalf("report = %+v, want reviewed=%s reconstructed=%s eligible", got, reviewed, reconstructed)
	}
	if err := l.Reconstruction(reviewledger.ReconstructionOpts{
		SHA: reconstructed, CandidateSHA: reviewed, Branch: "standing/lane",
		ContentProof: "reviewed content was reconstructed exactly",
	}); err != nil {
		t.Fatalf("record reconstruction: %v", err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if len(rows) != 3 || rows[2].Event != string(reviewledger.EventReconstruction) || rows[2].SHA != reconstructed || rows[2].CandidateSHA != reviewed || rows[2].ContentProof != "reviewed content was reconstructed exactly" {
		t.Fatalf("reconstruction ledger rows = %+v", rows)
	}
}

func addCandidatePass(t *testing.T, l *reviewledger.Ledger, sha, branch string) {
	t.Helper()
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Branch: branch, Reviewer: "reviewer", BuilderFamily: "anthropic",
		ReviewerFamily: "openai", Gate: "independent", Tier: "R2",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Branch: branch, Reviewer: "reviewer", Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: "openai", BuilderFamily: "anthropic",
	}); err != nil {
		t.Fatalf("verdict: %v", err)
	}
}

func writeCandidateFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func gitCandidateTest(t *testing.T, args ...string) {
	t.Helper()
	cmd := testgit.Command(".", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func gitCandidateOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := testgit.Command(".", args...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func gitCandidateOutputAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := testgit.Command(dir, args...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}
