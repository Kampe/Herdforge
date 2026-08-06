package scope

import "fmt"

// OutcomePASS is the only AncestorReceipt outcome that extends a
// composition proof.
const OutcomePASS = "PASS"

// AncestorReceipt is one incremental admission receipt covering exactly
// FromSHA (exclusive) through ToSHA (inclusive) — the cheap, common case of
// verifying only the newest commit's delta rather than the whole range.
type AncestorReceipt struct {
	FromSHA string
	ToSHA   string
	Outcome string
	Digest  string
}

// VerifyComposition proves that a sequence of incremental receipts, chained
// end to end, covers the complete scope.MergeBase..scope.CandidateSHA range
// with no gap, no reordering, and no non-PASS link. A single receipt scoped
// to HEAD^..HEAD can never satisfy this alone when the scope spans more than
// one commit: every ancestor commit needs its own PASS receipt in the chain.
// This is the fix for FAC-69's last-commit-only admission — a mutant that
// substitutes an ancestor's own parent for the true merge base, or that
// omits one ancestor's receipt, breaks the chain and is rejected.
func VerifyComposition(s AdmissionScope, receipts []AncestorReceipt) error {
	if err := s.SelfValidate(); err != nil {
		return err
	}
	if len(s.Commits) == 0 {
		return fmt.Errorf("scope: composition: scope has no commits to prove")
	}

	byFrom := make(map[string]AncestorReceipt, len(receipts))
	for _, rc := range receipts {
		if rc.FromSHA == "" || rc.ToSHA == "" {
			return fmt.Errorf("scope: composition: receipt with empty endpoint (from=%q to=%q)", rc.FromSHA, rc.ToSHA)
		}
		if existing, ok := byFrom[rc.FromSHA]; ok && existing.ToSHA != rc.ToSHA {
			return fmt.Errorf("scope: composition: conflicting receipts from %s (%s and %s)", rc.FromSHA, existing.ToSHA, rc.ToSHA)
		}
		byFrom[rc.FromSHA] = rc
	}

	prev := s.MergeBase
	for _, commit := range s.Commits {
		receipt, ok := byFrom[prev]
		if !ok {
			return fmt.Errorf("scope: composition: no receipt covers %s..%s (missing ancestor link)", prev, commit)
		}
		if receipt.ToSHA != commit {
			return fmt.Errorf("scope: composition: receipt from %s covers ..%s, expected ..%s (wrong base — substituted merge base or reordered commit)",
				prev, receipt.ToSHA, commit)
		}
		if receipt.Outcome != OutcomePASS {
			return fmt.Errorf("scope: composition: link %s..%s outcome %q is not PASS", prev, commit, receipt.Outcome)
		}
		if receipt.Digest == "" {
			return fmt.Errorf("scope: composition: link %s..%s has no verification digest", prev, commit)
		}
		prev = commit
	}
	if prev != s.CandidateSHA {
		return fmt.Errorf("scope: composition: chain ends at %s, expected candidate %s", prev, s.CandidateSHA)
	}
	return nil
}
