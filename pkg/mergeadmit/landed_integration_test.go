package mergeadmit

import (
	"context"
	"strings"
	"testing"
)

// prShapedLanding builds the exact PR836 shape with real git.
//
//	c0 ────────────── base ──────── merge      (main)
//	 └── sideCommit ──────────────────┘        (pull request line)
//
// The pull request branched BEFORE the reviewed base advanced, so the commit
// that carries the content is NOT a descendant of the base. Only the two-parent
// merge contains both. That is why sealing the carrier produced a receipt
// `herd approve` had to refuse.
func prShapedLanding(t *testing.T) (dir, base, sideCommit, mergeCommit string) {
	t.Helper()
	dir = gitRepo(t)
	c0 := commit(t, dir, "a.txt", "zero\n", "c0")

	run(t, dir, "git", "checkout", "-q", "-b", "pr", c0)
	sideCommit = commit(t, dir, "b.txt", "reviewed content\n", "reviewed candidate work")

	run(t, dir, "git", "checkout", "-q", "main")
	base = commit(t, dir, "c.txt", "base advanced\n", "unrelated main work (reviewed base)")

	mergeCommit = githubEmptyMerge(t, dir, base, sideCommit, "Merge pull request #836")

	// Prove the fixture really has the shape the defect needs, or the test
	// would pass against a fixture that cannot exhibit it.
	if ancestorOK(t, dir, base, sideCommit) {
		t.Fatal("fixture invalid: base is an ancestor of the carrier, so the defect cannot occur")
	}
	if !ancestorOK(t, dir, base, mergeCommit) {
		t.Fatal("fixture invalid: base is not an ancestor of the merge commit")
	}
	if !ancestorOK(t, dir, sideCommit, mergeCommit) {
		t.Fatal("fixture invalid: the carrier is not contained in the merge commit")
	}
	return dir, base, sideCommit, mergeCommit
}

func ancestorOK(t *testing.T, dir, sha, ref string) bool {
	t.Helper()
	proven, err := ancestorProven(context.Background(), dir, sha, ref)
	if err != nil {
		t.Fatalf("ancestry probe %s..%s: %v", short(sha), short(ref), err)
	}
	return proven
}

// REGRESSION (FAC-831): the selected commit must be the one that INTEGRATED the
// reviewed content, not the one that merely carries its patch.
func TestIntegrationCommitForPromotesCarrierToTheMergeCommit(t *testing.T) {
	dir, base, sideCommit, mergeCommit := prShapedLanding(t)

	landedCommits, err := rangeCommits(context.Background(), dir, base, mergeCommit)
	if err != nil {
		t.Fatalf("range commits: %v", err)
	}

	got, err := integrationCommitFor(context.Background(), dir, base, sideCommit, landedCommits)
	if err != nil {
		t.Fatalf("integrationCommitFor: %v", err)
	}
	if got == sideCommit {
		t.Fatal("selected the patch carrier; the reviewed base is not an ancestor of it, so the receipt would be unapprovable")
	}
	if got != mergeCommit {
		t.Fatalf("selected %s, want the integration commit %s", short(got), short(mergeCommit))
	}
	if !ancestorOK(t, dir, base, got) {
		t.Fatalf("selected %s does not contain the reviewed base", short(got))
	}
}

// The ordinary rebase landing is untouched: a carrier that already contains the
// base IS the integration commit and is returned unchanged.
func TestIntegrationCommitForLeavesAnOrdinaryLandingAlone(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "candidate work")
	harvested := rewriteOnto(t, dir, "landed", base, []string{candidate})

	landedCommits, err := rangeCommits(context.Background(), dir, base, harvested)
	if err != nil {
		t.Fatalf("range commits: %v", err)
	}
	got, err := integrationCommitFor(context.Background(), dir, base, harvested, landedCommits)
	if err != nil {
		t.Fatalf("integrationCommitFor: %v", err)
	}
	if got != harvested {
		t.Fatalf("promoted an ordinary landing from %s to %s; it was already the integration commit",
			short(harvested), short(got))
	}
}

// NO VALID LANDING: the content exists but nothing integrates it with the
// reviewed base. The producer must refuse, not reach for a later commit.
func TestIntegrationCommitForRefusesWhenNothingIntegratesTheBase(t *testing.T) {
	dir := gitRepo(t)
	c0 := commit(t, dir, "a.txt", "zero\n", "c0")
	run(t, dir, "git", "checkout", "-q", "-b", "pr", c0)
	sideCommit := commit(t, dir, "b.txt", "reviewed content\n", "reviewed candidate work")
	run(t, dir, "git", "checkout", "-q", "main")
	base := commit(t, dir, "c.txt", "base advanced\n", "reviewed base")
	// Main advances further, but NEVER merges the pull request line.
	later := commit(t, dir, "d.txt", "later unrelated\n", "later unrelated main work")

	landedCommits, err := rangeCommits(context.Background(), dir, base, later)
	if err != nil {
		t.Fatalf("range commits: %v", err)
	}
	got, err := integrationCommitFor(context.Background(), dir, base, sideCommit, landedCommits)
	if err == nil {
		t.Fatalf("sealed %s although nothing integrates the reviewed base with the carrier", short(got))
	}
	if got != "" {
		t.Fatalf("a refusal returned a commit: %s", short(got))
	}
	if !strings.Contains(err.Error(), "no landed commit integrates reviewed base") {
		t.Fatalf("refusal reason = %v", err)
	}
	// The later commit contains the base but NOT the content. Selecting it
	// would be a false proof, and it must not be what came back.
	if got == later {
		t.Fatal("selected a later commit that does not carry the reviewed content")
	}
}

// End to end through the sealing funnel: MergeSHA becomes the integration
// commit, ContentSHA records the carrier, and PatchID stays bound to the
// content — recomputing it from the merge commit would hard-fail in
// stablePatchID, because a merge commit has no diff of its own.
func TestEquivalentLandedProofSealsIntegrationCommitAndKeepsContentPatchID(t *testing.T) {
	dir, base, sideCommit, mergeCommit := prShapedLanding(t)

	landedCommits, err := rangeCommits(context.Background(), dir, base, mergeCommit)
	if err != nil {
		t.Fatalf("range commits: %v", err)
	}
	wantPatch, err := commitPatchID(context.Background(), dir, sideCommit)
	if err != nil {
		t.Fatalf("carrier patch id: %v", err)
	}

	proof, err := equivalentLandedProof(context.Background(), dir, base, sideCommit, mergeCommit,
		sideCommit, "ordered-patch-subsequence-on-landed", landedCommits)
	if err != nil {
		t.Fatalf("equivalentLandedProof: %v", err)
	}
	if proof.MergeSHA != mergeCommit {
		t.Fatalf("MergeSHA = %s, want the integration commit %s", short(proof.MergeSHA), short(mergeCommit))
	}
	if proof.ContentSHA != sideCommit {
		t.Fatalf("ContentSHA = %s, want the carrier %s", short(proof.ContentSHA), short(sideCommit))
	}
	if proof.PatchID != wantPatch {
		t.Fatalf("PatchID = %s, want the carrier's patch %s", short(proof.PatchID), short(wantPatch))
	}
	if !strings.HasSuffix(proof.Method, "+integration-commit") {
		t.Fatalf("Method = %q, want the promotion recorded", proof.Method)
	}
	// The consumer's requirement, asserted directly: reviewed base must be an
	// ancestor of the sealed MergeSHA.
	if !ancestorOK(t, dir, base, proof.MergeSHA) {
		t.Fatal("sealed MergeSHA does not contain the reviewed base; herd approve would refuse this receipt")
	}
}

// An ordinary landing seals exactly as before: no ContentSHA, no method suffix.
func TestEquivalentLandedProofIsUnchangedForAnOrdinaryLanding(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "candidate work")
	harvested := rewriteOnto(t, dir, "landed", base, []string{candidate})

	landedCommits, err := rangeCommits(context.Background(), dir, base, harvested)
	if err != nil {
		t.Fatalf("range commits: %v", err)
	}
	proof, err := equivalentLandedProof(context.Background(), dir, base, candidate, harvested,
		harvested, "ordered-patch-subsequence-on-landed", landedCommits)
	if err != nil {
		t.Fatalf("equivalentLandedProof: %v", err)
	}
	if proof.MergeSHA != harvested {
		t.Fatalf("MergeSHA = %s, want %s", short(proof.MergeSHA), short(harvested))
	}
	if proof.ContentSHA != "" {
		t.Fatalf("ContentSHA = %s, want empty when it equals MergeSHA", short(proof.ContentSHA))
	}
	if proof.Method != "ordered-patch-subsequence-on-landed" {
		t.Fatalf("Method = %q, want the unsuffixed method", proof.Method)
	}
}

// The producer must refuse before sealing rather than mint a receipt whose
// MergeSHA cannot satisfy the consumer.
func TestEquivalentLandedProofRefusesRatherThanSealAnUnapprovableReceipt(t *testing.T) {
	dir := gitRepo(t)
	c0 := commit(t, dir, "a.txt", "zero\n", "c0")
	run(t, dir, "git", "checkout", "-q", "-b", "pr", c0)
	sideCommit := commit(t, dir, "b.txt", "reviewed content\n", "reviewed candidate work")
	run(t, dir, "git", "checkout", "-q", "main")
	base := commit(t, dir, "c.txt", "base advanced\n", "reviewed base")
	later := commit(t, dir, "d.txt", "later unrelated\n", "later unrelated main work")

	landedCommits, err := rangeCommits(context.Background(), dir, base, later)
	if err != nil {
		t.Fatalf("range commits: %v", err)
	}
	proof, err := equivalentLandedProof(context.Background(), dir, base, sideCommit, later,
		sideCommit, "ordered-patch-subsequence-on-landed", landedCommits)
	if err == nil {
		t.Fatalf("sealed an unapprovable proof: MergeSHA %s", short(proof.MergeSHA))
	}
	if proof != nil {
		t.Fatal("a refusal returned a proof")
	}
}
