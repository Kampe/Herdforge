package harvest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func landGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return trimNL(string(out))
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// TestRebaseMergedCandidateIsAttestedWithoutAncestry is the FAC-557 gate.
//
// A rebase-merge rewrites SHAs, so the reviewed commit is not an ancestor of the
// target even though its patch shipped. Ancestry is wrong by construction for
// this strategy, and an ancestry-only check reports reviewed-and-shipped work as
// unlanded.
func TestRebaseMergedCandidateIsAttestedWithoutAncestry(t *testing.T) {
	dir := t.TempDir()
	landGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "base"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	landGit(t, dir, "add", "-A")
	landGit(t, dir, "commit", "-qm", "base")

	// The reviewed candidate on its own branch.
	landGit(t, dir, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	landGit(t, dir, "add", "-A")
	landGit(t, dir, "commit", "-qm", "the reviewed work")
	reviewed := landGit(t, dir, "rev-parse", "HEAD")

	// Advance main, then REBASE the patch onto it, exactly as a rebase-merge
	// does. The landed commit is a different object with the same patch.
	landGit(t, dir, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(dir, "other"), []byte("o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	landGit(t, dir, "add", "-A")
	landGit(t, dir, "commit", "-qm", "unrelated main work")
	landGit(t, dir, "cherry-pick", reviewed)
	landed := landGit(t, dir, "rev-parse", "HEAD")
	if landed == reviewed {
		t.Fatal("fixture must produce a DIFFERENT landed object, or it proves nothing about rebase")
	}

	// Ancestry must genuinely fail, or the test is not exercising the defect.
	if err := exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", reviewed, "main").Run(); err == nil {
		t.Fatal("fixture assumption: the reviewed SHA must NOT be an ancestor")
	}

	a, err := AttestLanded(context.Background(), dir, reviewed, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Landed {
		t.Fatalf("a rebase-merged candidate must attest as landed, got %s: %s", a.Disposition, a.Detail)
	}
	if a.Disposition != LandedByPatchIdentity {
		t.Errorf("disposition = %s, want %s", a.Disposition, LandedByPatchIdentity)
	}
}

// An exact-ancestor candidate takes the stronger, cheaper proof.
func TestAncestorTakesTheAncestryProof(t *testing.T) {
	dir := t.TempDir()
	landGit(t, dir, "init", "-q", "-b", "main")
	landGit(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	sha := landGit(t, dir, "rev-parse", "HEAD")
	landGit(t, dir, "commit", "-q", "--allow-empty", "-m", "later")

	a, err := AttestLanded(context.Background(), dir, sha, "main")
	if err != nil {
		t.Fatal(err)
	}
	if a.Disposition != LandedByAncestry || !a.Landed {
		t.Fatalf("expected ancestry proof, got %s (landed=%v)", a.Disposition, a.Landed)
	}
}

// Work that genuinely did not land must say so, and a cleaned branch must be a
// NAMED disposition rather than a late merge failure.
func TestCleanedBranchIsANamedDisposition(t *testing.T) {
	dir := t.TempDir()
	landGit(t, dir, "init", "-q", "-b", "main")
	landGit(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	landGit(t, dir, "checkout", "-q", "-b", "gone")
	if err := os.WriteFile(filepath.Join(dir, "never-landed.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	landGit(t, dir, "add", "-A")
	landGit(t, dir, "commit", "-qm", "work that never landed")
	orphan := landGit(t, dir, "rev-parse", "HEAD")

	// Still on a branch: plain not-landed.
	a, err := AttestLanded(context.Background(), dir, orphan, "main")
	if err != nil {
		t.Fatal(err)
	}
	if a.Landed {
		t.Fatal("work that never landed must not attest as landed")
	}
	if a.Disposition != NotLanded {
		t.Errorf("disposition = %s, want %s while the branch still exists", a.Disposition, NotLanded)
	}

	// Clean the branch: the object survives, no ref contains it.
	landGit(t, dir, "checkout", "-q", "main")
	landGit(t, dir, "branch", "-D", "gone")
	a, err = AttestLanded(context.Background(), dir, orphan, "main")
	if err != nil {
		t.Fatal(err)
	}
	if a.Landed {
		t.Fatal("a cleaned branch is not evidence of landing")
	}
	if a.Disposition != ObjectPresentBranchCleaned {
		t.Errorf("disposition = %s, want %s", a.Disposition, ObjectPresentBranchCleaned)
	}
}

// An absent object is UNPROVABLE, never "not landed": fail closed means saying
// so, not guessing. Conflating them is how reviewed work gets deleted as
// orphaned.
func TestAbsentObjectIsUnprovableNotUnlanded(t *testing.T) {
	dir := t.TempDir()
	landGit(t, dir, "init", "-q", "-b", "main")
	landGit(t, dir, "commit", "-q", "--allow-empty", "-m", "base")

	a, err := AttestLanded(context.Background(), dir, "0123456789abcdef0123456789abcdef01234567", "main")
	if err != nil {
		t.Fatal(err)
	}
	if a.Landed {
		t.Fatal("an unknown object must not attest as landed")
	}
	if a.Disposition != Unprovable {
		t.Errorf("disposition = %s, want %s — an absent object is not proof of absence of landing", a.Disposition, Unprovable)
	}
	// An unresolvable target ref is equally unprovable.
	a, err = AttestLanded(context.Background(), dir, landGit(t, dir, "rev-parse", "HEAD"), "no/such/ref")
	if err != nil {
		t.Fatal(err)
	}
	if a.Disposition != Unprovable || a.Landed {
		t.Errorf("an unresolvable target must be unprovable, got %s (landed=%v)", a.Disposition, a.Landed)
	}
}

// A multi-commit comparison must answer for the candidate asked about, not
// whichever commit git listed first.
func TestVerdictIsMatchedToTheCandidate(t *testing.T) {
	out := "- aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n+ bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"
	if eq, present := patchEquivalenceFor(out, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); !present || eq {
		t.Errorf("the + commit must read as not-equivalent, got eq=%v present=%v", eq, present)
	}
	if eq, present := patchEquivalenceFor(out, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !present || !eq {
		t.Errorf("the - commit must read as equivalent, got eq=%v present=%v", eq, present)
	}
	if _, present := patchEquivalenceFor(out, "cccccccccccccccccccccccccccccccccccccccc"); present {
		t.Error("a commit with no line must report no verdict, not a default")
	}
}
