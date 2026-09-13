package mergeadmit

import (
	"context"
	"fmt"
)

// FAC-831: MergeSHA must name the commit that INTEGRATED the reviewed content,
// not merely a commit that carries its patch.
//
// PR836 is the exact failure this exists to prevent.
// matchOrderedPatchSubsequence returned e1e19ed1, a single-parent commit on the
// pull request's own line whose patch matched the reviewed tail. The real
// integration commit fb351fa8 has two parents and is the only commit in the
// landed range containing the reviewed base 903457b7. nonEmptyCommits had
// already removed it from the selection pool, because a merge commit has no
// diff against its first parent — correct for patch IDENTITY (FAC-736), wrong
// as the pool for SHA SELECTION. The producer sealed a receipt on the carrier
// and `herd approve` then refused, because consumers require the reviewed base
// to be an ancestor of MergeSHA.
//
// ANCESTRY IS NOT CONTENT. Reaching a candidate through second-parent ancestry
// says only that the object is in the graph. A merge resolved with `ours`, or a
// conflict resolution that keeps main's side, retains exactly that ancestry
// while discarding every reviewed hunk — and the caller does not close the gap
// either: ProveEquivalentLandedContext proves the reviewed patches appear
// SOMEWHERE in base..landed as content commits, never that they survive into
// the tree of the commit finally selected. An earlier revision of this file
// claimed the ancestry check meant the selected commit "actually carries the
// reviewed content". It did not. Selection now requires a replay proof.

// maxIntegrationReplays bounds the git work this search may do. Only commits
// that already passed BOTH ancestry checks are replayed, which is a handful in
// any real range; the cap exists so a pathological range cannot turn selection
// into an unbounded sequence of merge-tree invocations.
const maxIntegrationReplays = 64

// contentPreservedAt proves the reviewed delta is present in sha's own TREE.
//
// The predicate is the package's existing replay: apply base..candidate onto
// sha's first parent with the exact reviewed merge base, and require the result
// to equal sha's tree. That is the same primitive the rebased-stack and squash
// proofs use, so there is no second definition of "the reviewed result landed
// here" to drift from the first.
//
// An `ours` merge fails it: its tree equals the first parent's, while the
// replay produces the tree WITH the reviewed changes. A conflict resolution
// that altered the reviewed hunks fails it for the same reason. A replay that
// cannot be computed at all (conflict, root commit with no first parent) is a
// refusal, never an assumption.
func contentPreservedAt(ctx context.Context, repoDir, base, candidate, sha string) (bool, error) {
	parent, err := gitOut(ctx, repoDir, "rev-parse", "--verify", "-q", sha+"^1")
	if err != nil {
		// Parent RESOLUTION is on the same footing: a cancelled run or an
		// exhausted allowance here is not "this commit has no first parent".
		if proofRunAborted(ctx, err) {
			if c := ctxFailure(ctx, err); c != nil {
				return false, c
			}
			return false, err
		}
		// No first parent: a root commit cannot have integrated anything.
		return false, nil
	}
	replayed, err := replayReviewedTree(ctx, repoDir, base, parent, candidate)
	if err != nil {
		// A cancelled run or an exhausted allowance must ABORT, never answer.
		// Returning (false, nil) for those turned "I could not look" into "the
		// content is not preserved", which silently skips a candidate commit and
		// can end in a wrong refusal or a wrong selection.
		if proofRunAborted(ctx, err) {
			if c := ctxFailure(ctx, err); c != nil {
				return false, c
			}
			return false, err
		}
		// merge-tree refused (conflict). Unprovable is not proven.
		return false, nil
	}
	landedTree, err := gitOut(ctx, repoDir, "rev-parse", "--verify", "-q", sha+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("resolve tree of %s: %w", short(sha), err)
	}
	return replayed == landedTree, nil
}

// integrationCommitFor returns the commit to seal as MergeSHA, or an error.
//
//  1. If the content carrier already contains the reviewed base it IS the
//     integration commit and is returned unchanged. That is every ordinary
//     rebase landing, already covered by the caller's ordered-patch and tree
//     predicates, and this function changes nothing for it.
//  2. Otherwise the carrier sits off the integrated line — a pull request's own
//     commit, reachable only through a merge. The search then walks the PINNED
//     landed range oldest-first for the EARLIEST commit that contains the
//     reviewed base, contains the carrier, AND whose own tree is the exact
//     result of replaying the reviewed delta onto its first parent.
//
// All three conditions are required. Ancestry alone selects a merge that threw
// the reviewed content away; the replay is what makes the selection a content
// claim rather than a graph claim. The search never leaves base..landed, so it
// cannot reach an arbitrary later SHA, and it takes the EARLIEST qualifying
// commit so the answer is deterministic.
//
// Ancestry rides the package's own ancestorProven probe, never a fresh git
// call: proof.go documents that a fired deadline must come back as a context
// error and never as a false "not an ancestor" evidence answer.
func integrationCommitFor(ctx context.Context, repoDir, base, candidate, carrier string, landedCommits []string) (string, error) {
	carrierHasBase, err := ancestorProven(ctx, repoDir, base, carrier)
	if err != nil {
		return "", err
	}
	if carrierHasBase {
		return carrier, nil
	}
	replays := 0
	for _, sha := range landedCommits {
		if sha == carrier {
			continue
		}
		containsCarrier, err := ancestorProven(ctx, repoDir, carrier, sha)
		if err != nil {
			return "", err
		}
		if !containsCarrier {
			continue
		}
		containsBase, err := ancestorProven(ctx, repoDir, base, sha)
		if err != nil {
			return "", err
		}
		if !containsBase {
			continue
		}
		if replays >= maxIntegrationReplays {
			return "", fmt.Errorf(
				"integration search exceeded %d content replays without a proof; refusing to seal on an unbounded search",
				maxIntegrationReplays)
		}
		replays++
		preserved, err := contentPreservedAt(ctx, repoDir, base, candidate, sha)
		if err != nil {
			return "", err
		}
		if preserved {
			return sha, nil
		}
	}
	// Refuse rather than seal. A receipt whose MergeSHA does not contain the
	// reviewed base is one the consumer cannot accept, and one whose tree does
	// not hold the reviewed result is a false content claim. Minting either is
	// how a producer reports success for work that is not there.
	return "", fmt.Errorf(
		"no landed commit both integrates reviewed base %s with content commit %s and preserves the reviewed result: "+
			"the content appears on the landed range, but no commit in it contains both and replays to its own tree "+
			"(a merge that discarded or altered the reviewed hunks looks exactly like this)",
		short(base), short(carrier))
}
