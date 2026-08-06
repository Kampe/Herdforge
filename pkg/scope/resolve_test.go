package scope

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestResolve_HappyPath(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	c1 := repo.commit("a.txt", "one\n", "c1")

	s, err := repo.resolver().Resolve(context.Background(), "org/repo", "main", "", c1)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.MergeBase != base {
		t.Fatalf("merge base = %s, want %s", s.MergeBase, base)
	}
	if s.TargetSHA != base {
		t.Fatalf("target sha = %s, want %s", s.TargetSHA, base)
	}
	if len(s.Commits) != 1 || s.Commits[0] != c1 {
		t.Fatalf("commits = %v, want [%s]", s.Commits, c1)
	}
	if len(s.ChangedPaths) != 1 || s.ChangedPaths[0] != "a.txt" {
		t.Fatalf("changed paths = %v, want [a.txt]", s.ChangedPaths)
	}
	if s.DiffDigest == "" || !strings.HasPrefix(s.DiffDigest, "sha256:") {
		t.Fatalf("diff digest not set: %q", s.DiffDigest)
	}
	if err := s.SelfValidate(); err != nil {
		t.Fatalf("self validate: %v", err)
	}
}

func TestResolve_OrderedMultiCommitChain(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	c1 := repo.commit("a.txt", "1\n", "c1")
	c2 := repo.commit("b.txt", "2\n", "c2")
	c3 := repo.commit("c.txt", "3\n", "c3")

	s, err := repo.resolver().Resolve(context.Background(), "org/repo", "main", "", c3)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{c1, c2, c3}
	if !equalSlices(s.Commits, want) {
		t.Fatalf("commits = %v, want %v (oldest-first order matters for composition proof)", s.Commits, want)
	}
	if !equalSlices(s.ChangedPaths, []string{"a.txt", "b.txt", "c.txt"}) {
		t.Fatalf("changed paths = %v", s.ChangedPaths)
	}
}

func TestResolve_RejectsMalformedCandidateSHA(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)

	if _, err := repo.resolver().Resolve(context.Background(), "org/repo", "main", "", "not-a-sha"); err == nil {
		t.Fatal("expected error for malformed candidate sha")
	}
}

func TestResolve_RejectsMissingCandidate(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)

	fake := strings.Repeat("a", 40)
	_, err := repo.resolver().Resolve(context.Background(), "org/repo", "main", "", fake)
	if !errors.Is(err, ErrCandidateMissing) {
		t.Fatalf("err = %v, want ErrCandidateMissing", err)
	}
}

func TestResolve_CandidateRefMustResolveToCandidateSHA(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	c1 := repo.commit("a.txt", "1\n", "c1")
	c2 := repo.commit("a.txt", "2\n", "c2")

	// Ref currently at c2 but caller claims candidate is c1: force-push shape.
	if _, err := repo.resolver().Resolve(context.Background(), "org/repo", "main", "refs/heads/feature", c1); !errors.Is(err, ErrForcePushed) {
		t.Fatalf("err = %v, want ErrForcePushed", err)
	}
	// Ref at c2 and candidate is c2: consistent, must succeed.
	if _, err := repo.resolver().Resolve(context.Background(), "org/repo", "main", "refs/heads/feature", c2); err != nil {
		t.Fatalf("resolve consistent ref: %v", err)
	}
}

func TestVerifyCurrent_HappyPathNoDrift(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	c1 := repo.commit("a.txt", "1\n", "c1")

	res := repo.resolver()
	s, err := res.Resolve(context.Background(), "org/repo", "main", "", c1)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := res.VerifyCurrent(context.Background(), *s); err != nil {
		t.Fatalf("verify current on unchanged scope: %v", err)
	}
}

func TestVerifyCurrent_TargetBranchAdvanceInvalidatesReceipt(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	c1 := repo.commit("a.txt", "1\n", "c1")

	res := repo.resolver()
	s, err := res.Resolve(context.Background(), "org/repo", "main", "", c1)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// main advances independently of the candidate branch (still a
	// descendant of the same merge base, so the merge base itself does not
	// move — only the target tip does).
	repo.run("checkout", "--quiet", "main")
	newMain := repo.commit("unrelated.txt", "later main work\n", "main advances")
	repo.setOriginMain(newMain)

	err = res.VerifyCurrent(context.Background(), *s)
	if !errors.Is(err, ErrTargetAdvanced) {
		t.Fatalf("err = %v, want ErrTargetAdvanced", err)
	}
}

func TestVerifyCurrent_ForcePushInvalidatesReceipt(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	c1 := repo.commit("a.txt", "1\n", "c1")

	res := repo.resolver()
	s, err := res.Resolve(context.Background(), "org/repo", "main", "refs/heads/feature", c1)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// feature is force-pushed to a different commit; the ref no longer
	// points at the recorded candidate sha.
	repo.commit("a.txt", "2\n", "force-pushed replacement")

	err = res.VerifyCurrent(context.Background(), *s)
	if !errors.Is(err, ErrForcePushed) {
		t.Fatalf("err = %v, want ErrForcePushed", err)
	}
}

func TestVerifyCurrent_TamperedMergeBaseRejected(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	c1 := repo.commit("a.txt", "1\n", "c1")
	c2 := repo.commit("a.txt", "2\n", "c2")

	res := repo.resolver()
	s, err := res.Resolve(context.Background(), "org/repo", "main", "", c2)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Mutant: substitute the candidate's direct parent (c1) for the true
	// merge base (base) — the exact FAC-69 failure shape. The tampered scope
	// still self-validates because its digest is recomputed over the
	// tampered fields, so only a live git recomputation can catch it.
	tampered := *s
	tampered.MergeBase = c1
	tampered.Digest = computeDigest(tampered)
	if err := tampered.SelfValidate(); err != nil {
		t.Fatalf("tampered scope must still self-validate: %v", err)
	}

	err = res.VerifyCurrent(context.Background(), tampered)
	if !errors.Is(err, ErrMergeBaseChanged) {
		t.Fatalf("err = %v, want ErrMergeBaseChanged", err)
	}
}

func TestVerifyCurrent_TamperedCommitListRejected(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	repo.commit("a.txt", "1\n", "c1")
	c2 := repo.commit("a.txt", "2\n", "c2")

	res := repo.resolver()
	s, err := res.Resolve(context.Background(), "org/repo", "main", "", c2)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Mutant: drop an ancestor commit from the recorded chain (squash/omit).
	tampered := *s
	tampered.Commits = []string{c2}
	tampered.Digest = computeDigest(tampered)

	err = res.VerifyCurrent(context.Background(), tampered)
	if !errors.Is(err, ErrCommitSetChanged) {
		t.Fatalf("err = %v, want ErrCommitSetChanged", err)
	}
}

func TestVerifyCurrent_TamperedPathSetRejected(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	c1 := repo.commit("a.txt", "1\n", "c1")

	res := repo.resolver()
	s, err := res.Resolve(context.Background(), "org/repo", "main", "", c1)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	tampered := *s
	tampered.ChangedPaths = []string{"a.txt", "not-really-changed.txt"}
	tampered.Digest = computeDigest(tampered)

	err = res.VerifyCurrent(context.Background(), tampered)
	if !errors.Is(err, ErrPathSetChanged) {
		t.Fatalf("err = %v, want ErrPathSetChanged", err)
	}
}

func TestVerifyCurrent_MissingDigestRejected(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	c1 := repo.commit("a.txt", "1\n", "c1")

	res := repo.resolver()
	s, err := res.Resolve(context.Background(), "org/repo", "main", "", c1)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	noDigest := *s
	noDigest.Digest = ""

	if err := res.VerifyCurrent(context.Background(), noDigest); !errors.Is(err, ErrScopeMissing) {
		t.Fatalf("err = %v, want ErrScopeMissing", err)
	}
}

func TestVerifyCurrent_CorruptedDigestRejected(t *testing.T) {
	repo := newTestRepo(t)
	base := repo.commit("README.md", "base\n", "merge base")
	repo.setOriginMain(base)
	repo.checkoutNewBranch("feature", base)
	c1 := repo.commit("a.txt", "1\n", "c1")

	res := repo.resolver()
	s, err := res.Resolve(context.Background(), "org/repo", "main", "", c1)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	corrupted := *s
	corrupted.Digest = "sha256:" + strings.Repeat("0", 64)

	if err := res.VerifyCurrent(context.Background(), corrupted); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("err = %v, want ErrScopeMismatch", err)
	}
}
