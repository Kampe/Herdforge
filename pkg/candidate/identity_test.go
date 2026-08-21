package candidate

import (
	"context"
	"strings"
	"testing"
)

type fakeGit struct {
	branches   map[string][]string
	worktrees  map[string]string
	exists     map[string]bool
	ancestor   map[string]bool
	patchOnMain map[string]bool
}

func (f fakeGit) BranchesContaining(_ context.Context, sha string) ([]string, error) {
	return f.branches[sha], nil
}
func (f fakeGit) WorktreeForBranch(_ context.Context, b string) (string, error) {
	return f.worktrees[b], nil
}
func (f fakeGit) ObjectExists(_ context.Context, sha string) bool      { return f.exists[sha] }
func (f fakeGit) ContainedInMain(_ context.Context, sha string) bool   { return f.ancestor[sha] }
func (f fakeGit) PatchLandedOnMain(_ context.Context, sha string) bool { return f.patchOnMain[sha] }

type fakeReviews struct {
	byRef map[string][]Review
	err   error
}

func (f fakeReviews) AdmittedForRef(ref string) ([]Review, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byRef[ref], nil
}

// TestArtifactBranchIsNotIdentity is the live CHA-2206 case.
//
// The admitted artifact named standing/platform-ops while the exact SHA was
// only reachable from standing/coverage-integrity. The artifact's branch was
// WRONG and the SHA was still valid, so branch must be derived from git and the
// disagreement reported rather than silently followed.
func TestArtifactBranchIsNotIdentity(t *testing.T) {
	sha := "546ef41aa6bc32db32064d80e249f543f05ece6d"
	id, err := Resolve(context.Background(), "CHA-2206",
		fakeReviews{byRef: map[string][]Review{"CHA-2206": {{
			CandidateSHA: sha, RecordedBranch: "standing/platform-ops",
			Verdict: "PASS", ReviewerFamily: "google", BuilderFamily: "openai",
			Artifact: "artifact.md",
		}}}},
		fakeGit{
			exists:    map[string]bool{sha: true},
			branches:  map[string][]string{sha: {"standing/coverage-integrity"}},
			worktrees: map[string]string{"standing/coverage-integrity": "/wt/coverage"},
		})
	if err != nil {
		t.Fatal(err)
	}
	if !id.BranchMismatch {
		t.Fatal("a recorded branch that does not contain the object must be flagged")
	}
	if len(id.Branches) != 1 || id.Branches[0] != "standing/coverage-integrity" {
		t.Fatalf("branches must come from git, got %v", id.Branches)
	}
	if id.Worktree != "/wt/coverage" {
		t.Fatalf("worktree must follow the real branch, got %q", id.Worktree)
	}
	if id.Disposition != DispositionHarvest {
		t.Fatalf("an admitted PASS not on main is harvestable, got %s", id.Disposition)
	}
	// The detail must tell the operator to harvest by SHA and name both branches.
	for _, want := range []string{"exact SHA", "standing/platform-ops", "standing/coverage-integrity"} {
		if !strings.Contains(id.Detail, want) {
			t.Fatalf("detail must mention %q, got %q", want, id.Detail)
		}
	}
}

// A rebase-merged candidate is not an ancestor but its patch is on main.
// Ancestry alone reports it unlanded, which was a real source of false negatives.
func TestPatchEquivalentLandingIsRecognized(t *testing.T) {
	sha := "fc3bd108309667d67493a09efc9725f47b15452f"
	id, _ := Resolve(context.Background(), "CHA-2183",
		fakeReviews{byRef: map[string][]Review{"CHA-2183": {{CandidateSHA: sha, Verdict: "PASS"}}}},
		fakeGit{exists: map[string]bool{sha: true}, patchOnMain: map[string]bool{sha: true}})
	if id.Landing != LandingPatchOnly {
		t.Fatalf("landing = %s, want patch-equivalent", id.Landing)
	}
	if id.Disposition != DispositionAlreadyOnMain {
		t.Fatalf("disposition = %s, want already-on-main", id.Disposition)
	}
	if !strings.Contains(id.Detail, "rebase-merge") {
		t.Fatalf("detail must explain why ancestry checks disagree, got %q", id.Detail)
	}
}

// Ambiguity must refuse and name the alternatives. This is the CHA-2185 /
// CHA-2205 shape where a latest-wins lookup picked the wrong object.
func TestAmbiguityRefusesAndNamesAlternatives(t *testing.T) {
	id, _ := Resolve(context.Background(), "CHA-2185",
		fakeReviews{byRef: map[string][]Review{"CHA-2185": {
			{CandidateSHA: "991ce0757eeb", Verdict: "PASS"},
			{CandidateSHA: "1d5ce367acd4", Verdict: "PASS"},
		}}},
		fakeGit{})
	if id.Disposition != DispositionAmbiguous {
		t.Fatalf("two admitted candidates must refuse, got %s", id.Disposition)
	}
	if len(id.Alternatives) != 2 {
		t.Fatalf("alternatives must be listed, got %v", id.Alternatives)
	}
	if !strings.Contains(id.Detail, "rather than letting the tool guess") {
		t.Fatalf("detail must refuse guessing, got %q", id.Detail)
	}
}

func TestMissingObjectAndNoEvidence(t *testing.T) {
	id, _ := Resolve(context.Background(), "CHA-1",
		fakeReviews{byRef: map[string][]Review{"CHA-1": {{CandidateSHA: "deadbeef", Verdict: "PASS"}}}},
		fakeGit{})
	if id.Disposition != DispositionMissingObject {
		t.Fatalf("an absent object must be reported, got %s", id.Disposition)
	}
	empty, _ := Resolve(context.Background(), "CHA-2", fakeReviews{}, fakeGit{})
	if empty.Disposition != DispositionNoEvidence {
		t.Fatalf("no admitted review must be reported, got %s", empty.Disposition)
	}
}

// A FAIL is a repair disposition, never harvestable.
func TestFailIsRepairNotHarvest(t *testing.T) {
	sha := "abc1234"
	id, _ := Resolve(context.Background(), "CHA-2204",
		fakeReviews{byRef: map[string][]Review{"CHA-2204": {{CandidateSHA: sha, Verdict: "FAIL"}}}},
		fakeGit{exists: map[string]bool{sha: true}})
	if id.Disposition != DispositionRepair {
		t.Fatalf("a FAIL must be a repair disposition, got %s", id.Disposition)
	}
}
