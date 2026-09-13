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
// to be an ancestor of MergeSHA (see the merge-commit OID comparison in
// cmd/herd/integration_native_cleanup.go).
//
// Producer and consumer disagreed about what MergeSHA means. This resolves it
// on the PRODUCER side, and refuses rather than sealing when it cannot.

// integrationCommitFor returns the commit to seal as MergeSHA, or an error.
//
//  1. If the content carrier already contains the reviewed base it IS the
//     integration commit and is returned unchanged. That is every ordinary
//     rebase landing, and this function changes nothing for it.
//  2. Otherwise the carrier sits off the integrated line — a pull request's own
//     commit, reachable only through a merge. The search then walks the PINNED
//     landed range oldest-first for the EARLIEST commit that contains BOTH the
//     reviewed base and the carrier. That is the first point at which the
//     reviewed content actually entered the pinned main line.
//
// The search is deliberately narrow, because "find a later commit that works"
// is how a false proof gets minted. It never leaves base..landed, so it cannot
// wander to an arbitrary later SHA; it requires the carrier as an ancestor, so
// it cannot select a commit that does not actually carry the reviewed content;
// and it takes the EARLIEST such commit, so the answer is deterministic rather
// than "whichever descendant we happened to reach first". A later commit that
// merely contains the base proves nothing and is never selected.
//
// Ancestry rides the package's own ancestorProven probe, never a fresh git
// call: proof.go documents that a fired deadline must come back as a context
// error and never as a false "not an ancestor" evidence answer.
func integrationCommitFor(ctx context.Context, repoDir, base, carrier string, landedCommits []string) (string, error) {
	carrierHasBase, err := ancestorProven(ctx, repoDir, base, carrier)
	if err != nil {
		return "", err
	}
	if carrierHasBase {
		return carrier, nil
	}
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
		if containsBase {
			return sha, nil
		}
	}
	// Refuse rather than seal. A receipt whose MergeSHA does not contain the
	// reviewed base is one the consumer cannot accept, and minting it is how a
	// producer reports success for work that can never be approved.
	return "", fmt.Errorf(
		"no landed commit integrates reviewed base %s with content commit %s: "+
			"the content is present on the landed range but no commit in it contains both, "+
			"so there is no integration commit to bind the receipt to",
		short(base), short(carrier))
}
