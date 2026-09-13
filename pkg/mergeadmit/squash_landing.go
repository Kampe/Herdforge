package mergeadmit

import (
	"context"
	"fmt"
	"strings"
)

// ProveLanded establishes content containment only. It does not admit review
// evidence or grant completion authority; ReconcileLanded still checks those.
// Both the CLI observation and receipt path use this identical predicate.
func (g *Gate) ProveLanded(req Request, landed string) (*Proof, error) {
	if g == nil {
		return nil, fmt.Errorf("landed proof requires a gate")
	}
	if req.Reconstruction != nil && g.Ledger == nil {
		return nil, fmt.Errorf("reconstructed landing proof requires a ledger")
	}
	base, candidate, err := g.reconstructionContent(req)
	if err != nil {
		return nil, err
	}
	return ProveEquivalentLandedContext(context.Background(), g.RepoDir, ProofRequest{BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed})
}

// A squash has the aggregate reviewed patch, not any intermediate patch.
// Require both its complete patch identity and the exact result of replaying
// the reviewed delta onto its actual parent. Patch-ID whitespace normalization
// alone cannot authorize different bytes. More than one match is ambiguous.
//
// The ancestry gate rides the package's own owned-process probe
// (ancestorProven), not harvest.IsAncestor: a caller deadline must kill the
// probe's whole process group and come back as the context error, never as a
// "not an ancestor" evidence answer or an orphaned child.
func matchSquashRangeReplay(ctx context.Context, dir, base, candidate string, landed []string) (string, bool, error) {
	proven, err := ancestorProven(ctx, dir, base, candidate)
	if err != nil {
		return "", false, err
	}
	if !proven {
		return "", false, fmt.Errorf("reviewed base ancestry is unproven")
	}
	want, err := rangePatchID(ctx, dir, base, candidate)
	if err != nil {
		return "", false, err
	}
	var matches []string
	for _, sha := range landed {
		parents, err := gitOut(ctx, dir, "rev-list", "--parents", "-n", "1", sha)
		if err != nil {
			if c := ctxFailure(ctx, err); c != nil {
				return "", false, c
			}
			return "", false, err
		}
		fields := strings.Fields(parents)
		if len(fields) != 2 {
			continue
		} // A squash endpoint is a single-parent commit.
		parent := fields[1]
		parentProven, parentErr := ancestorProven(ctx, dir, base, parent)
		if parentErr != nil {
			// A fired deadline stops the whole loop: falling through to the
			// next candidate would keep spawning probes after the budget
			// died and read the cancellation as "no match found".
			return "", false, parentErr
		}
		if !parentProven {
			continue
		}
		got, err := rangePatchID(ctx, dir, parent, sha)
		if err != nil {
			if c := ctxFailure(ctx, err); c != nil {
				return "", false, c
			}
			return "", false, err
		}
		if got != want {
			continue
		}
		replayed, err := replayReviewedTree(ctx, dir, base, parent, candidate)
		if err != nil {
			if c := ctxFailure(ctx, err); c != nil {
				return "", false, c
			}
			continue
		} // A conflicting replay proves no equivalence.
		tree, err := gitOut(ctx, dir, "rev-parse", "--verify", sha+"^{tree}")
		if err != nil {
			if c := ctxFailure(ctx, err); c != nil {
				return "", false, c
			}
			return "", false, err
		}
		if replayed == tree {
			matches = append(matches, sha)
		}
	}
	if len(matches) > 1 {
		return "", false, fmt.Errorf("ambiguous squash landing: %d exact matches", len(matches))
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return "", false, nil
}
