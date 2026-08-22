package harvest

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// FAC-557: a rebase-merge rewrites SHAs, so the reviewed commit is normally NOT
// an ancestor of origin/main even though its patch shipped. Ancestry is
// therefore the wrong question for this merge strategy — wrong by construction,
// not occasionally — and an ancestry-only check reports reviewed-and-shipped work
// as unlanded. A session already deleted baseline rows as "orphaned" for exactly
// this reason.
//
// Landing is attested by PATCH IDENTITY, with ancestry kept as the cheaper
// stronger case when it happens to hold. Every outcome is a NAMED disposition so
// "cannot prove" is never silently rendered as "did not land".

// Disposition names how a candidate's landing was established, or why it could
// not be.
type Disposition string

const (
	// LandedByAncestry: the exact commit is reachable from the target ref. The
	// strongest proof, and what a merge-commit strategy produces.
	LandedByAncestry Disposition = "landed-by-ancestry"
	// LandedByPatchIdentity: the exact commit is not reachable, but its patch
	// is already present on the target. This is a normal rebase-merge.
	LandedByPatchIdentity Disposition = "landed-by-patch-identity"
	// NotLanded: the object exists and its patch is genuinely absent.
	NotLanded Disposition = "not-landed"
	// ObjectPresentBranchCleaned: the reviewed object still exists but no ref
	// contains it. Surfaced BEFORE a merge attempt, because discovering it as a
	// late merge failure tells an operator nothing about why.
	ObjectPresentBranchCleaned Disposition = "object-present-branch-cleaned"
	// Unprovable: landing could not be established either way. Never conflated
	// with NotLanded — fail closed means saying "unprovable", not guessing.
	Unprovable Disposition = "unprovable"
)

// Attestation is the result of asking whether a candidate shipped.
type Attestation struct {
	SHA         string
	TargetRef   string
	Disposition Disposition
	// Landed is true only for a positive proof. Unprovable is never landed.
	Landed bool
	Detail string
}

// AttestLanded reports whether a candidate's content is present on targetRef,
// without needing to be told where it landed.
//
// ProveEquivalentLanded already proves patch equivalence, but it requires the
// caller to supply the landed SHA. After a rebase-merge and a branch cleanup
// nobody knows that SHA, which is precisely when attestation is needed.
func AttestLanded(ctx context.Context, repoDir, sha, targetRef string) (Attestation, error) {
	a := Attestation{SHA: sha, TargetRef: targetRef, Disposition: Unprovable}
	sha = strings.TrimSpace(sha)
	targetRef = strings.TrimSpace(targetRef)
	if sha == "" || targetRef == "" {
		return a, fmt.Errorf("attest landed: candidate sha and target ref are required")
	}

	// The object must exist locally; without it nothing can be compared and the
	// honest answer is unprovable, not unlanded.
	if _, err := gitOutput(ctx, repoDir, "rev-parse", "--verify", sha+"^{commit}"); err != nil {
		a.Detail = "candidate object is not present in this repository, so nothing can be compared"
		return a, nil
	}
	if _, err := gitOutput(ctx, repoDir, "rev-parse", "--verify", targetRef); err != nil {
		a.Detail = fmt.Sprintf("target ref %q cannot be resolved", targetRef)
		return a, nil
	}

	// Cheapest strong proof first.
	if IsAncestor(ctx, repoDir, sha, targetRef) {
		a.Disposition = LandedByAncestry
		a.Landed = true
		a.Detail = "exact commit is reachable from " + targetRef
		return a, nil
	}

	// git cherry is patch-based: it marks a commit "-" when an equivalent patch
	// already exists upstream, and "+" when it does not. That is the question a
	// rebase-merge requires.
	out, err := gitOutput(ctx, repoDir, "cherry", targetRef, sha)
	if err != nil {
		a.Detail = "patch comparison against " + targetRef + " failed: " + err.Error()
		return a, nil
	}
	equivalent, present := patchEquivalenceFor(out, sha)
	if !present {
		// No line for this commit at all: the comparison did not actually cover
		// the candidate, so its own answer is unprovable.
		a.Detail = "patch comparison produced no verdict for this candidate"
		return a, nil
	}
	if equivalent {
		a.Disposition = LandedByPatchIdentity
		a.Landed = true
		a.Detail = "exact commit is not reachable, but its patch is already present on " + targetRef +
			" (normal for a rebase-merge, which rewrites SHAs)"
		return a, nil
	}

	// Genuinely absent. Distinguish the cleaned-branch case, which is the one an
	// operator most often misreads as data loss.
	a.Disposition = NotLanded
	a.Detail = "patch is absent from " + targetRef
	refs, refErr := gitOutput(ctx, repoDir, "for-each-ref", "--contains", sha, "--format=%(refname)")
	if refErr == nil && strings.TrimSpace(refs) == "" {
		a.Disposition = ObjectPresentBranchCleaned
		a.Detail = "the reviewed object still exists but no ref contains it: its branch was cleaned and its patch is not on " + targetRef
	}
	return a, nil
}

// patchEquivalenceFor reads the verdict for one commit out of git cherry output.
//
// The prefix is the whole answer: "-" means an equivalent patch exists upstream,
// "+" means it does not. Matching by SHA rather than taking the first line keeps
// a multi-commit range from being answered by the wrong commit.
func patchEquivalenceFor(cherryOutput, sha string) (equivalent, present bool) {
	sha = strings.TrimSpace(sha)
	for _, line := range strings.Split(cherryOutput, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		if !strings.HasPrefix(fields[1], sha) && !strings.HasPrefix(sha, fields[1]) {
			continue
		}
		switch fields[0] {
		case "-":
			return true, true
		case "+":
			return false, true
		}
	}
	return false, false
}


// IsAncestor reports whether sha is reachable from ref.
//
// One definition on purpose. This exact git invocation is written in eight
// places across the tree, and FAC-561 was caused by two of those copies
// disagreeing about what "on this branch" meant — the selector and the gate
// answered differently. Callers in this package use this; the remaining copies
// elsewhere are part of the FAC-565 consolidation.
//
// A false result covers both "not reachable" and "git could not tell us", which
// is correct for every caller here: an unproven ancestry must never be treated
// as proven. AttestLanded then continues to the patch-identity question rather
// than concluding anything from the negative.
func IsAncestor(ctx context.Context, repoDir, sha, ref string) bool {
	if strings.TrimSpace(sha) == "" || strings.TrimSpace(ref) == "" {
		return false
	}
	return gitSucceeds(ctx, repoDir, "merge-base", "--is-ancestor", sha, ref) == nil
}

// gitSucceeds runs git for its exit status only. gitOutput (integration.go) is
// the package's one place for capturing output; this is the boolean companion,
// used where the exit code IS the answer — merge-base --is-ancestor reports
// reachability entirely through it.
func gitSucceeds(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd.Run()
}
