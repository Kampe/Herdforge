package scope

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// buildFAC69Scope reproduces the FAC-69 incident shape against a real git
// repo: a green merge base, six commits on top of it, and a final checkpoint
// that is itself green — the exact chain a last-commit-only ("compare only
// against direct parent") admission policy would have waved through.
func buildFAC69Scope(t *testing.T) (repo *testRepo, scope AdmissionScope, commits []string) {
	t.Helper()
	repo = newTestRepo(t)
	base := repo.commit("pkg/deps/deps.go", "package deps\n// green\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)

	commits = []string{
		repo.commit("pkg/deps/deps.go", "package deps\n// regression introduced\n", "c1: introduces regression"),
		repo.commit("pkg/dispatch/dispatch.go", "package dispatch\n", "c2"),
		repo.commit("pkg/lifecycle/lifecycle.go", "package lifecycle\n", "c3"),
		repo.commit("pkg/worktree/worktree.go", "package worktree\n", "c4"),
		repo.commit("README.md", "notes\n", "c5: direct parent of final checkpoint"),
		repo.commit("pkg/deps/deps.go", "package deps\n// checkpoint touches deps but does not re-verify it broke earlier\n", "c6: final checkpoint"),
	}

	res := repo.resolver()
	s, err := res.Resolve(context.Background(), "org/herdforge", "main", "", commits[5])
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !equalSlices(s.Commits, commits) {
		t.Fatalf("commits = %v, want %v", s.Commits, commits)
	}
	return repo, *s, commits
}

func TestVerifyComposition_FAC69_LastCommitOnlyReceiptIsRejected(t *testing.T) {
	_, s, commits := buildFAC69Scope(t)

	// Only the final link (c5..c6, "direct parent") has a receipt — the
	// exact FAC-69 shape: the checkpoint compared only against its direct
	// parent looked green, but nothing proves the merge-base..c5 delta
	// (where the regression actually lives) ever passed.
	lastLinkOnly := []AncestorReceipt{
		{FromSHA: commits[4], ToSHA: commits[5], Outcome: OutcomePASS, Digest: "sha256:" + "final"},
	}

	err := VerifyComposition(s, lastLinkOnly)
	if err == nil {
		t.Fatal("last-commit-only admission must be rejected before PR/reviewer actions")
	}
}

func TestVerifyComposition_FAC69_FullChainWithExactCompositionProofIsAccepted(t *testing.T) {
	_, s, commits := buildFAC69Scope(t)

	receipts := []AncestorReceipt{
		{FromSHA: s.MergeBase, ToSHA: commits[0], Outcome: OutcomePASS, Digest: "sha256:r1"},
		{FromSHA: commits[0], ToSHA: commits[1], Outcome: OutcomePASS, Digest: "sha256:r2"},
		{FromSHA: commits[1], ToSHA: commits[2], Outcome: OutcomePASS, Digest: "sha256:r3"},
		{FromSHA: commits[2], ToSHA: commits[3], Outcome: OutcomePASS, Digest: "sha256:r4"},
		{FromSHA: commits[3], ToSHA: commits[4], Outcome: OutcomePASS, Digest: "sha256:r5"},
		{FromSHA: commits[4], ToSHA: commits[5], Outcome: OutcomePASS, Digest: "sha256:r6"},
	}

	if err := VerifyComposition(s, receipts); err != nil {
		t.Fatalf("fully admitted six-commit chain must be accepted: %v", err)
	}
}

func TestVerifyComposition_MutantSubstitutesAncestorForMergeBase(t *testing.T) {
	_, s, commits := buildFAC69Scope(t)

	// Mutant: the first link claims to start from c1 (an ancestor commit)
	// instead of the true merge base, with every other link otherwise
	// complete and passing. This is the "HEAD^ for merge-base" substitution
	// the spec calls out by name.
	receipts := []AncestorReceipt{
		{FromSHA: commits[0], ToSHA: commits[1], Outcome: OutcomePASS, Digest: "sha256:r2"},
		{FromSHA: commits[1], ToSHA: commits[2], Outcome: OutcomePASS, Digest: "sha256:r3"},
		{FromSHA: commits[2], ToSHA: commits[3], Outcome: OutcomePASS, Digest: "sha256:r4"},
		{FromSHA: commits[3], ToSHA: commits[4], Outcome: OutcomePASS, Digest: "sha256:r5"},
		{FromSHA: commits[4], ToSHA: commits[5], Outcome: OutcomePASS, Digest: "sha256:r6"},
	}

	if err := VerifyComposition(s, receipts); err == nil {
		t.Fatal("substituting an ancestor for the true merge base must be rejected")
	}
}

func TestVerifyComposition_MutantOmitsOneAncestor(t *testing.T) {
	_, s, commits := buildFAC69Scope(t)

	// Mutant: every link present except c2..c3 — a gap in the middle of the
	// chain.
	receipts := []AncestorReceipt{
		{FromSHA: s.MergeBase, ToSHA: commits[0], Outcome: OutcomePASS, Digest: "sha256:r1"},
		{FromSHA: commits[0], ToSHA: commits[1], Outcome: OutcomePASS, Digest: "sha256:r2"},
		// commits[1]..commits[2] intentionally missing
		{FromSHA: commits[2], ToSHA: commits[3], Outcome: OutcomePASS, Digest: "sha256:r4"},
		{FromSHA: commits[3], ToSHA: commits[4], Outcome: OutcomePASS, Digest: "sha256:r5"},
		{FromSHA: commits[4], ToSHA: commits[5], Outcome: OutcomePASS, Digest: "sha256:r6"},
	}

	if err := VerifyComposition(s, receipts); err == nil {
		t.Fatal("omitting one ancestor link must be rejected")
	}
}

func TestVerifyComposition_NonPassLinkRejected(t *testing.T) {
	_, s, commits := buildFAC69Scope(t)

	receipts := []AncestorReceipt{
		{FromSHA: s.MergeBase, ToSHA: commits[0], Outcome: "FAIL", Digest: "sha256:r1"},
		{FromSHA: commits[0], ToSHA: commits[1], Outcome: OutcomePASS, Digest: "sha256:r2"},
		{FromSHA: commits[1], ToSHA: commits[2], Outcome: OutcomePASS, Digest: "sha256:r3"},
		{FromSHA: commits[2], ToSHA: commits[3], Outcome: OutcomePASS, Digest: "sha256:r4"},
		{FromSHA: commits[3], ToSHA: commits[4], Outcome: OutcomePASS, Digest: "sha256:r5"},
		{FromSHA: commits[4], ToSHA: commits[5], Outcome: OutcomePASS, Digest: "sha256:r6"},
	}

	if err := VerifyComposition(s, receipts); err == nil {
		t.Fatal("a non-PASS link anywhere in the chain must be rejected")
	}
}

func TestVerifyComposition_TamperedScopeRejected(t *testing.T) {
	_, s, _ := buildFAC69Scope(t)
	s.Digest = "sha256:" + strings.Repeat("0", 64)

	if err := VerifyComposition(s, nil); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("err = %v, want ErrScopeMismatch", err)
	}
}
