package mergeadmit

import (
	"strings"
	"testing"
)

func TestParseModeRejectsAbsentAndUnknown(t *testing.T) {
	for _, s := range []string{"", "   ", "ff", "rebase-merge", "squash-and-merge"} {
		if _, err := ParseMode(s); err == nil {
			t.Fatalf("ParseMode(%q) accepted an unusable mode; absence is not consent to guess ancestry", s)
		}
	}
	for in, want := range map[string]Mode{"merge": ModeMerge, "REBASE": ModeRebase, " squash ": ModeSquash} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// Merge/fast-forward: the candidate object survives, so ancestry proves it.
func TestProveMergeModeExactAncestry(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "work")
	run(t, dir, "git", "checkout", "-q", "main")
	run(t, dir, "git", "merge", "-q", "--no-ff", "-m", "merge work", "work")
	landed := revParse(t, dir, "HEAD")

	p, err := Prove(dir, ProofRequest{Mode: ModeMerge, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
	if err != nil {
		t.Fatalf("merge-mode proof: %v", err)
	}
	if p.Method != "exact-ancestry" {
		t.Fatalf("method = %q, want exact-ancestry", p.Method)
	}
	// For a surviving candidate the receipt binds to the candidate itself; a
	// merge commit has no single-parent diff and could not produce a patch id.
	if p.MergeSHA != candidate {
		t.Fatalf("merge sha = %s, want the surviving candidate %s", p.MergeSHA, candidate)
	}
	if p.PatchID == "" {
		t.Fatal("proof carries no patch id")
	}
}

// THE MUTANT KILL for FAC-178. The coordinator ran
//
//	candidate_ancestor=ok
//
// capturing `git merge-base --is-ancestor`'s result into a variable and
// printing success regardless of its exit status. This test builds a candidate
// that is genuinely NOT on the landed history and asserts the proof refuses.
// If the exit-status check in Prove's ModeMerge branch is dropped or its error
// ignored, this test fails.
func TestProveMergeModeRefusesNonAncestor(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "work")
	// main advances independently; the candidate never lands.
	run(t, dir, "git", "checkout", "-q", "main")
	landed := commit(t, dir, "c.txt", "three\n", "unrelated")

	_, err := Prove(dir, ProofRequest{Mode: ModeMerge, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
	if err == nil {
		t.Fatal("merge-mode proof admitted a candidate that is NOT an ancestor of the landed history")
	}
	if !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("refusal did not name the failed ancestry predicate: %v", err)
	}
}

// The FAC-178 shape end to end: GitHub rebase-merged, so the reviewed sha is
// gone from the landed history entirely. Ancestry is the WRONG question here —
// asking it would refuse a perfectly good merge — and the right one is ordered
// patch identity plus tree identity.
func TestProveRebaseModeRewrittenSHAs(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	c1 := commit(t, dir, "b.txt", "two\n", "work 1")
	candidate := commit(t, dir, "c.txt", "three\n", "work 2")
	landed := rewriteOnto(t, dir, "landed", base, []string{c1, candidate})

	if landed == candidate {
		t.Fatal("fixture did not actually rewrite the sha; the test would prove nothing")
	}
	// Confirm the fixture really is the incident shape: no ancestry at all.
	if err := runGit(dir, "merge-base", "--is-ancestor", candidate, landed); err == nil {
		t.Fatal("fixture candidate is still an ancestor; this is not a rebase rewrite")
	}

	p, err := Prove(dir, ProofRequest{Mode: ModeRebase, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
	if err != nil {
		t.Fatalf("rebase-mode proof refused a valid rewrite: %v", err)
	}
	if p.Method != "ordered-patch-identity+tree-identity" {
		t.Fatalf("method = %q", p.Method)
	}
	if p.MergeSHA != landed {
		t.Fatalf("merge sha = %s, want the landed tip %s", p.MergeSHA, landed)
	}
}

// Using the wrong proof for the mode is itself a failure: a rebase-rewritten
// candidate has no ancestry, and a gate that asks for ancestry anyway would
// have refused the valid FAC-178 merge.
func TestProveWrongModeForRewriteRefuses(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "work")
	landed := rewriteOnto(t, dir, "landed", base, []string{candidate})

	if _, err := Prove(dir, ProofRequest{Mode: ModeMerge, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed}); err == nil {
		t.Fatal("merge-mode ancestry proof passed against a rewritten history")
	}
}

// Patch identity alone is not enough: a range can replay patch-for-patch and
// still leave a different tree (a stray file, a differently-resolved
// conflict). Tree identity is what closes that gap.
func TestProveRebaseModeRefusesDivergentTree(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "work")
	landed := rewriteOnto(t, dir, "landed", base, []string{candidate})
	// Someone slipped an extra commit into the landed history.
	landed = commit(t, dir, "stowaway.txt", "not reviewed\n", "stowaway")

	_, err := Prove(dir, ProofRequest{Mode: ModeRebase, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
	if err == nil {
		t.Fatal("rebase-mode proof admitted a landed history carrying an unreviewed commit")
	}
}

func TestProveRebaseModeRefusesAlteredContent(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "b.txt", "two\n", "work")
	// The landed commit claims to be the same change but is not.
	run(t, dir, "git", "checkout", "-q", "-B", "landed", base)
	landed := commit(t, dir, "b.txt", "two AND SOMETHING ELSE\n", "work")

	if _, err := Prove(dir, ProofRequest{Mode: ModeRebase, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed}); err == nil {
		t.Fatal("rebase-mode proof admitted altered content under the same commit message")
	}
}

// ISOLATES THE TREE CHECK. git patch-id is whitespace-blind by design, so a
// landed commit that reindents or respaces the reviewed change produces the
// IDENTICAL patch id against a DIFFERENT tree. Patch identity alone would
// admit it. Verified against real git: "hello world" and "hello   world"
// share patch id cdb4cccf… and differ in tree.
//
// Delete sameTree's comparison and this test fails; nothing else covers it.
func TestProveRebaseModeRefusesWhitespaceAlteredContent(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "f.txt", "hello world\n", "add f")

	run(t, dir, "git", "checkout", "-q", "-B", "landed", base)
	landed := commit(t, dir, "f.txt", "hello   world\n", "add f")

	// Guard the fixture: if patch-id ever stops colliding here, this test is
	// no longer isolating the tree check and must be rebuilt.
	cp, err := commitPatchID(dir, candidate)
	if err != nil {
		t.Fatalf("candidate patch id: %v", err)
	}
	lp, err := commitPatchID(dir, landed)
	if err != nil {
		t.Fatalf("landed patch id: %v", err)
	}
	if cp != lp {
		t.Skipf("fixture no longer collides on patch id (%s vs %s); tree check is not isolated here", short(cp), short(lp))
	}

	_, err = Prove(dir, ProofRequest{Mode: ModeRebase, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
	if err == nil {
		t.Fatal("rebase-mode proof admitted a whitespace-altered tree that shares the reviewed patch id")
	}
	if !strings.Contains(err.Error(), "tree identity") {
		t.Fatalf("refusal did not come from the tree check: %v", err)
	}
}

// ISOLATES THE PER-COMMIT PATCH COMPARISON. Same commit count, same final
// tree, different ordered patches. Only the positional patch-id comparison can
// tell these apart — the length check and the tree check both pass.
//
// Order is part of the claim: a bisect, a revert, or a partial rollback lands
// on a commit that never existed in the reviewed history.
func TestProveRebaseModeRefusesReorderedCommits(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	commit(t, dir, "x.txt", "ex\n", "add x")
	candidate := commit(t, dir, "y.txt", "why\n", "add y")

	// Same two changes, applied in the opposite order.
	run(t, dir, "git", "checkout", "-q", "-B", "landed", base)
	commit(t, dir, "y.txt", "why\n", "add y")
	landed := commit(t, dir, "x.txt", "ex\n", "add x")

	// Guard the fixture: the discriminator must be order alone.
	if revParse(t, dir, candidate+"^{tree}") != revParse(t, dir, landed+"^{tree}") {
		t.Fatal("fixture trees differ; this test is not isolating the ordered-patch check")
	}

	_, err := Prove(dir, ProofRequest{Mode: ModeRebase, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
	if err == nil {
		t.Fatal("rebase-mode proof admitted a reordered history with the same final tree")
	}
	if !strings.Contains(err.Error(), "of the range differs") {
		t.Fatalf("refusal did not come from the ordered-patch check: %v", err)
	}
}

// The squash proof needs its own tree check for the same whitespace reason.
func TestProveSquashModeRefusesWhitespaceAlteredContent(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	candidate := commit(t, dir, "f.txt", "hello world\n", "add f")

	run(t, dir, "git", "checkout", "-q", "-B", "landed", base)
	landed := commit(t, dir, "f.txt", "hello   world\n", "squashed")

	wantPID, err := rangePatchID(dir, base, candidate)
	if err != nil {
		t.Fatalf("candidate range patch id: %v", err)
	}
	gotPID, err := rangePatchID(dir, base, landed)
	if err != nil {
		t.Fatalf("landed range patch id: %v", err)
	}
	if wantPID != gotPID {
		t.Skipf("fixture no longer collides on range patch id; squash tree check is not isolated here")
	}

	_, err = Prove(dir, ProofRequest{Mode: ModeSquash, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
	if err == nil {
		t.Fatal("squash-mode proof admitted a whitespace-altered tree that shares the reviewed range patch id")
	}
	if !strings.Contains(err.Error(), "tree identity") {
		t.Fatalf("refusal did not come from the tree check: %v", err)
	}
}

func TestProveSquashModeCombinedIdentity(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	commit(t, dir, "b.txt", "two\n", "work 1")
	candidate := commit(t, dir, "c.txt", "three\n", "work 2")

	run(t, dir, "git", "checkout", "-q", "-B", "landed", base)
	run(t, dir, "git", "merge", "-q", "--squash", "work")
	run(t, dir, "git", "commit", "-q", "-m", "squashed work")
	landed := revParse(t, dir, "HEAD")

	p, err := Prove(dir, ProofRequest{Mode: ModeSquash, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
	if err != nil {
		t.Fatalf("squash-mode proof refused a valid squash: %v", err)
	}
	if p.Method != "combined-patch-identity+tree-identity" {
		t.Fatalf("method = %q", p.Method)
	}
}

func TestProveSquashModeRefusesPartialSquash(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	commit(t, dir, "b.txt", "two\n", "work 1")
	candidate := commit(t, dir, "c.txt", "three\n", "work 2")

	// The squash dropped one of the reviewed commits.
	run(t, dir, "git", "checkout", "-q", "-B", "landed", base)
	landed := commit(t, dir, "b.txt", "two\n", "squashed (incomplete)")

	if _, err := Prove(dir, ProofRequest{Mode: ModeSquash, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed}); err == nil {
		t.Fatal("squash-mode proof admitted a squash that dropped a reviewed commit")
	}
}

// The reason FAC-156 was reopened: a branch holding only its worktree anchor
// merged a zero-line diff, and every downstream check passed because there was
// nothing there to be wrong. An empty candidate has no content to prove.
func TestProveRefusesEmptyCandidate(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")

	for _, mode := range []Mode{ModeMerge, ModeRebase, ModeSquash} {
		_, err := Prove(dir, ProofRequest{Mode: mode, BaseSHA: base, CandidateSHA: base, LandedSHA: base})
		if err == nil {
			t.Fatalf("%s-mode proof admitted a candidate with no commits over base", mode)
		}
		if !strings.Contains(err.Error(), "empty candidate") {
			t.Fatalf("%s-mode: refusal did not name the empty candidate: %v", mode, err)
		}
	}
}

func TestProveRefusesUnresolvableRevisions(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "one\n", "base")

	for _, req := range []ProofRequest{
		{Mode: ModeMerge, BaseSHA: "", CandidateSHA: base, LandedSHA: base},
		{Mode: ModeMerge, BaseSHA: base, CandidateSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", LandedSHA: base},
		{Mode: ModeMerge, BaseSHA: base, CandidateSHA: base, LandedSHA: "not-a-rev"},
	} {
		if _, err := Prove(dir, req); err == nil {
			t.Fatalf("proof accepted an unresolvable revision: %+v", req)
		}
	}
}
