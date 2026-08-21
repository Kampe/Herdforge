// Package candidate resolves a task ref to its exact candidate identity.
//
// FAC-568: there was no single command mapping an In Review card to its exact
// candidate SHA, containing branch, worktree, families, verdict, landing status
// and one disposition. Coordinators combined paginated board reads, git ref and
// worktree walks, review artifacts, and merge-tree loops by hand.
//
// The governing rule comes from a live case: an admitted artifact named branch
// standing/platform-ops while the exact SHA was only reachable from
// standing/coverage-integrity. The artifact's branch was WRONG and the SHA was
// still valid.
//
// So THE SHA IS THE IDENTITY. Branch, worktree, and reachability are DERIVED
// from git for that object, never trusted from the artifact that named it. A
// recorded branch is treated as a hint to report and, when it disagrees with
// git, to flag -- never as the thing being harvested.
package candidate

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Git is the read-only git surface this package needs. Injected so resolution
// is testable without a repository.
type Git interface {
	// BranchesContaining lists refs that contain the object.
	BranchesContaining(ctx context.Context, sha string) ([]string, error)
	// WorktreeForBranch returns the checkout path for a branch, if any.
	WorktreeForBranch(ctx context.Context, branch string) (string, error)
	// ObjectExists reports whether the object is present at all.
	ObjectExists(ctx context.Context, sha string) bool
	// ContainedInMain reports whether the object is already an ancestor of
	// origin/main.
	ContainedInMain(ctx context.Context, sha string) bool
	// PatchLandedOnMain reports whether the object's patch is present on
	// origin/main even without ancestry, which is the rebase-merge case.
	PatchLandedOnMain(ctx context.Context, sha string) bool
}

// Review is the admitted review evidence for a ref.
type Review struct {
	CandidateSHA   string
	RecordedBranch string
	Artifact       string
	Reviewer       string
	ReviewerFamily string
	BuilderFamily  string
	Verdict        string
	MergeSHA       string
}

// Reviews resolves admitted review evidence per ref.
type Reviews interface {
	AdmittedForRef(ref string) ([]Review, error)
}

// Landing describes how, if at all, a candidate reached origin/main.
type Landing string

const (
	LandingNone      Landing = "not-landed"
	LandingAncestor  Landing = "ancestor-of-main"
	LandingPatchOnly Landing = "patch-equivalent-on-main"
	LandingUnknown   Landing = "unknown"
)

// Disposition is the single next action for a candidate.
type Disposition string

const (
	DispositionHarvest      Disposition = "harvest"
	DispositionAlreadyOnMain Disposition = "already-on-main"
	DispositionNeedsReview  Disposition = "needs-review"
	DispositionRepair       Disposition = "repair-failed-review"
	DispositionMissingObject Disposition = "missing-object"
	DispositionAmbiguous    Disposition = "ambiguous-candidates"
	DispositionNoEvidence   Disposition = "no-admitted-review"
)

// Identity is everything known about one candidate, assembled from the ledger
// and verified against git.
type Identity struct {
	Ref          string `json:"ref"`
	CandidateSHA string `json:"candidate_sha,omitempty"`

	// Branches are the refs that ACTUALLY contain the object, from git.
	Branches []string `json:"branches,omitempty"`
	// RecordedBranch is what the review artifact claimed.
	RecordedBranch string `json:"recorded_branch,omitempty"`
	// BranchMismatch is true when the artifact named a branch that does not
	// contain the object. The live case that motivated this package.
	BranchMismatch bool `json:"branch_mismatch,omitempty"`

	Worktree string `json:"worktree,omitempty"`

	Artifact       string `json:"artifact,omitempty"`
	Reviewer       string `json:"reviewer,omitempty"`
	ReviewerFamily string `json:"reviewer_family,omitempty"`
	BuilderFamily  string `json:"builder_family,omitempty"`
	Verdict        string `json:"verdict,omitempty"`
	MergeSHA       string `json:"merge_sha,omitempty"`

	Landing     Landing     `json:"landing"`
	Disposition Disposition `json:"disposition"`
	// Detail explains the disposition in one operator-facing sentence.
	Detail string `json:"detail"`
	// Alternatives lists other admitted candidate SHAs when resolution is
	// ambiguous, so an operator names one rather than the tool guessing.
	Alternatives []string `json:"alternative_candidates,omitempty"`
}

// Resolve assembles the identity for one ref.
//
// It never guesses between multiple admitted candidates: ambiguity is reported
// with the alternatives so the operator names the one that shipped. Guessing
// here authorizes work against evidence for a different object.
func Resolve(ctx context.Context, ref string, reviews Reviews, git Git) (*Identity, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("candidate resolve: ref is required")
	}
	if reviews == nil || git == nil {
		return nil, fmt.Errorf("candidate resolve: review and git authorities are required")
	}
	admitted, err := reviews.AdmittedForRef(ref)
	if err != nil {
		return nil, fmt.Errorf("candidate resolve %s: %w", ref, err)
	}

	id := &Identity{Ref: ref, Landing: LandingUnknown}
	if len(admitted) == 0 {
		id.Disposition = DispositionNoEvidence
		id.Detail = "no admitted review evidence names this ref"
		return id, nil
	}
	if len(admitted) > 1 {
		id.Disposition = DispositionAmbiguous
		for _, a := range admitted {
			id.Alternatives = append(id.Alternatives, a.CandidateSHA)
		}
		sort.Strings(id.Alternatives)
		id.Detail = fmt.Sprintf("%d admitted candidates name this ref; name the one that shipped rather than letting the tool guess",
			len(admitted))
		return id, nil
	}

	found := admitted[0]
	id.CandidateSHA = found.CandidateSHA
	id.RecordedBranch = found.RecordedBranch
	id.Artifact = found.Artifact
	id.Reviewer = found.Reviewer
	id.ReviewerFamily = found.ReviewerFamily
	id.BuilderFamily = found.BuilderFamily
	id.Verdict = found.Verdict
	id.MergeSHA = found.MergeSHA

	if !git.ObjectExists(ctx, id.CandidateSHA) {
		id.Disposition = DispositionMissingObject
		id.Detail = "the admitted candidate object is not present in this repository"
		return id, nil
	}

	// Branch comes from GIT, not from the artifact. The artifact's branch was
	// observed to be wrong while the SHA stayed valid.
	branches, berr := git.BranchesContaining(ctx, id.CandidateSHA)
	if berr == nil {
		id.Branches = branches
	}
	if id.RecordedBranch != "" && !containsBranch(id.Branches, id.RecordedBranch) {
		id.BranchMismatch = true
	}
	for _, b := range id.Branches {
		if wt, werr := git.WorktreeForBranch(ctx, b); werr == nil && wt != "" {
			id.Worktree = wt
			break
		}
	}

	switch {
	case git.ContainedInMain(ctx, id.CandidateSHA):
		id.Landing = LandingAncestor
	case git.PatchLandedOnMain(ctx, id.CandidateSHA):
		id.Landing = LandingPatchOnly
	default:
		id.Landing = LandingNone
	}

	id.Disposition, id.Detail = decide(id)
	return id, nil
}

func decide(id *Identity) (Disposition, string) {
	verdict := strings.ToUpper(strings.TrimSpace(id.Verdict))
	if id.Landing == LandingAncestor || id.Landing == LandingPatchOnly {
		detail := "already on origin/main"
		if id.Landing == LandingPatchOnly {
			// The distinction matters: an ancestry check alone reports this as
			// not landed, which is wrong for every rebase-merge.
			detail = "patch is on origin/main without ancestry (rebase-merge); ancestry checks alone will call this unlanded"
		}
		return DispositionAlreadyOnMain, detail
	}
	switch verdict {
	case "PASS":
		if id.BranchMismatch {
			// Harvest by SHA, not by the branch the artifact named.
			return DispositionHarvest, fmt.Sprintf(
				"admitted PASS; harvest the exact SHA — the artifact named %q but the object is reachable from %s",
				id.RecordedBranch, strings.Join(id.Branches, ", "))
		}
		return DispositionHarvest, "admitted PASS and not yet on origin/main"
	case "FAIL":
		return DispositionRepair, "admitted FAIL; return findings to the owning builder"
	default:
		return DispositionNeedsReview, fmt.Sprintf("verdict %q is not a harvestable disposition", id.Verdict)
	}
}

func containsBranch(branches []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, b := range branches {
		if strings.EqualFold(strings.TrimSpace(b), want) {
			return true
		}
	}
	return false
}
