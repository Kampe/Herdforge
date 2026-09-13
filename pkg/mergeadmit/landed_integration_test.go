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

	got, err := integrationCommitFor(context.Background(), dir, base, sideCommit, sideCommit, landedCommits)
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
	got, err := integrationCommitFor(context.Background(), dir, base, candidate, harvested, landedCommits)
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
	got, err := integrationCommitFor(context.Background(), dir, base, sideCommit, sideCommit, landedCommits)
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

// oursMerge creates a two-parent merge that KEEPS the first parent's tree and
// discards the second parent's content entirely. Ancestry is intact — the
// carrier is a real second parent — but not one reviewed byte survives.
func oursMerge(t *testing.T, dir, firstParent, secondParent, msg string) string {
	t.Helper()
	run(t, dir, "git", "checkout", "-q", "-B", "main", firstParent)
	run(t, dir, "git", "merge", "--no-ff", "--no-edit", "-s", "ours", "-m", msg, secondParent)
	sha := revParse(t, dir, "HEAD")
	if revParse(t, dir, sha+"^{tree}") != revParse(t, dir, firstParent+"^{tree}") {
		t.Fatal("fixture is not an ours merge: its tree differs from the first parent")
	}
	if revParse(t, dir, sha+"^2") != secondParent {
		t.Fatal("fixture ours merge does not carry the candidate as second parent")
	}
	return sha
}

// SAFETY REGRESSION (FAC-831): ancestry is necessary, not sufficient.
//
// An `ours` merge retains the candidate as second-parent ancestry and throws
// every reviewed hunk away. Selecting it would seal a receipt asserting content
// that is not in the tree. Only the replay proof rejects this; the two ancestry
// checks both pass.
func TestIntegrationCommitForRefusesAnOursMergeThatDiscardedTheContent(t *testing.T) {
	dir := gitRepo(t)
	c0 := commit(t, dir, "a.txt", "zero\n", "c0")
	run(t, dir, "git", "checkout", "-q", "-b", "pr", c0)
	sideCommit := commit(t, dir, "b.txt", "reviewed content\n", "reviewed candidate work")
	run(t, dir, "git", "checkout", "-q", "main")
	base := commit(t, dir, "c.txt", "base advanced\n", "reviewed base")
	discarded := oursMerge(t, dir, base, sideCommit, "Merge pull request #836 (ours)")

	// Both ancestry conditions hold. That is the whole point of this fixture.
	if !ancestorOK(t, dir, base, discarded) {
		t.Fatal("fixture invalid: the ours merge does not contain the reviewed base")
	}
	if !ancestorOK(t, dir, sideCommit, discarded) {
		t.Fatal("fixture invalid: the ours merge does not have the carrier as an ancestor")
	}
	// And the reviewed file is simply absent from the merged tree.
	if out := runOut(t, dir, "git", "ls-tree", "--name-only", discarded); strings.Contains(out, "b.txt") {
		t.Fatal("fixture invalid: the ours merge kept the reviewed file")
	}

	landedCommits, err := rangeCommits(context.Background(), dir, base, discarded)
	if err != nil {
		t.Fatalf("range commits: %v", err)
	}
	got, err := integrationCommitFor(context.Background(), dir, base, sideCommit, sideCommit, landedCommits)
	if err == nil {
		t.Fatalf("selected %s: an ours merge that discarded every reviewed hunk was sealed as the integration commit",
			short(got))
	}
	if got != "" {
		t.Fatalf("a refusal returned a commit: %s", short(got))
	}
	if !strings.Contains(err.Error(), "preserves the reviewed result") {
		t.Fatalf("refusal reason = %v", err)
	}
}

// The same gap with an ALTERED rather than discarded resolution: the merge
// keeps the reviewed path but with different bytes. Ancestry still passes.
func TestIntegrationCommitForRefusesAMergeThatAlteredTheReviewedContent(t *testing.T) {
	dir := gitRepo(t)
	c0 := commit(t, dir, "a.txt", "zero\n", "c0")
	run(t, dir, "git", "checkout", "-q", "-b", "pr", c0)
	sideCommit := commit(t, dir, "b.txt", "reviewed content\n", "reviewed candidate work")
	run(t, dir, "git", "checkout", "-q", "main")
	base := commit(t, dir, "c.txt", "base advanced\n", "reviewed base")

	// Merge normally, then amend the merge so the reviewed file holds other
	// bytes. This is what a hand-resolved conflict looks like from outside.
	merged := githubEmptyMerge(t, dir, base, sideCommit, "Merge pull request #836")
	if err := osWriteFile(t, dir, "b.txt", "SUBSTITUTED content\n"); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", "b.txt")
	run(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	altered := revParse(t, dir, "HEAD")
	if altered == merged {
		t.Fatal("fixture invalid: amend did not produce a new commit")
	}
	if !ancestorOK(t, dir, base, altered) || !ancestorOK(t, dir, sideCommit, altered) {
		t.Fatal("fixture invalid: the altered merge lost an ancestry relation")
	}

	landedCommits, err := rangeCommits(context.Background(), dir, base, altered)
	if err != nil {
		t.Fatalf("range commits: %v", err)
	}
	got, err := integrationCommitFor(context.Background(), dir, base, sideCommit, sideCommit, landedCommits)
	if err == nil {
		t.Fatalf("selected %s: a merge whose tree holds different bytes than the reviewed result was sealed",
			short(got))
	}
	if got != "" {
		t.Fatalf("a refusal returned a commit: %s", short(got))
	}
}

// contentPreservedAt is the predicate the safety rests on, pinned directly:
// true for the honest merge, false for the ours merge.
func TestContentPreservedAtSeparatesAnHonestMergeFromAnOursMerge(t *testing.T) {
	dir, base, sideCommit, mergeCommit := prShapedLanding(t)
	ok, err := contentPreservedAt(context.Background(), dir, base, sideCommit, mergeCommit)
	if err != nil {
		t.Fatalf("honest merge: %v", err)
	}
	if !ok {
		t.Fatal("the honest merge was reported as not preserving the reviewed result")
	}

	other := gitRepo(t)
	c0 := commit(t, other, "a.txt", "zero\n", "c0")
	run(t, other, "git", "checkout", "-q", "-b", "pr", c0)
	side2 := commit(t, other, "b.txt", "reviewed content\n", "reviewed candidate work")
	run(t, other, "git", "checkout", "-q", "main")
	base2 := commit(t, other, "c.txt", "base advanced\n", "reviewed base")
	discarded := oursMerge(t, other, base2, side2, "ours")

	ok, err = contentPreservedAt(context.Background(), other, base2, side2, discarded)
	if err != nil {
		t.Fatalf("ours merge: %v", err)
	}
	if ok {
		t.Fatal("an ours merge was reported as preserving the reviewed result")
	}
}

// A LATER REVERT, recorded honestly.
//
// The reviewed content really did land at the merge, and the merge's own tree
// still replays exactly, so it is selected. This selector answers "which commit
// integrated the reviewed content", and a revert that happens afterwards does
// not change the answer to that question.
//
// The limitation is stated rather than hidden: whether work reverted after
// landing may still be approved is a policy decision about the probed tip, not
// about which commit integrated it, and nothing here claims to make it.
func TestIntegrationCommitForSelectsTheMergeEvenWhenALaterCommitRevertsIt(t *testing.T) {
	dir, base, sideCommit, mergeCommit := prShapedLanding(t)
	run(t, dir, "git", "checkout", "-q", "main")
	run(t, dir, "git", "revert", "--no-edit", "-m", "1", mergeCommit)
	reverted := revParse(t, dir, "HEAD")
	if reverted == mergeCommit {
		t.Fatal("fixture invalid: revert produced no new commit")
	}
	if out := runOut(t, dir, "git", "ls-tree", "--name-only", reverted); strings.Contains(out, "b.txt") {
		t.Fatal("fixture invalid: the revert did not remove the reviewed file")
	}

	landedCommits, err := rangeCommits(context.Background(), dir, base, reverted)
	if err != nil {
		t.Fatalf("range commits: %v", err)
	}
	got, err := integrationCommitFor(context.Background(), dir, base, sideCommit, sideCommit, landedCommits)
	if err != nil {
		t.Fatalf("integrationCommitFor: %v", err)
	}
	if got != mergeCommit {
		t.Fatalf("selected %s, want the merge %s that actually integrated the content", short(got), short(mergeCommit))
	}
	// The revert commit itself must never be selected: it does not replay to
	// the reviewed result.
	if got == reverted {
		t.Fatal("selected the revert commit as the integration commit")
	}
}
