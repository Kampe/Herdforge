package reviewledger

import (
	"fmt"
	"sort"
	"strings"
)

// MergeReadiness is the aggregate verdict posture for one candidate SHA.
//
// FAC-625: existing lookups made two mistakes that both produced "safe to merge"
// for candidates reviewers had rejected.
//
//  1. Counting verdict ROWS instead of reading verdict VALUES. An operator
//     (me) checked `grep -c <sha> ledger` and reported eight FAIL/BLOCKED
//     candidates as merge-ready four beats running. That is the same
//     presence-not-polarity error FAC-581 fixed inside review-ingest.
//  2. VerdictFor returns the LAST verdict row. For a candidate with a FAIL from
//     one reviewer and a PASS from another at the same timestamp, last-wins
//     silently discards the dissent and reports PASS.
//
// Readiness is therefore conservative by construction: any unsuperseded
// FAIL or BLOCKED blocks, and disagreement blocks rather than resolving to the
// favourable side.
type MergeReadiness struct {
	SHA   string `json:"sha"`
	Ready bool   `json:"ready"`
	// ProvenanceUnrecorded counts verdicts admitted under
	// GateProvenanceUnrecorded. Those reviews are real and are preserved, but the
	// builder family is explicitly unknown, so they cannot support a cross-family
	// independence claim and must not silently read as a clean pass.
	ProvenanceUnrecorded int      `json:"provenance_unrecorded"`
	Passes               int      `json:"passes"`
	Failures             int      `json:"failures"`
	Blocked              int      `json:"blocked"`
	Reason               string   `json:"reason"`
	Verdicts             []string `json:"verdicts,omitempty"`
}

// shaMatches reports whether a ledger SHA and a caller SHA identify the same
// commit, tolerating the short/long forms both sides legitimately use. Requires
// at least 12 hex chars so a stray prefix cannot collide.
func shaMatches(ledgerSHA, want string) bool {
	a := strings.ToLower(strings.TrimSpace(ledgerSHA))
	b := strings.ToLower(strings.TrimSpace(want))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(b) < 12 || len(a) < 12 {
		return false
	}
	if len(b) < len(a) {
		return strings.HasPrefix(a, b)
	}
	return strings.HasPrefix(b, a)
}

// MergeReadinessFor aggregates every verdict recorded for a candidate SHA.
func (l *Ledger) MergeReadinessFor(sha string) (MergeReadiness, error) {
	sha = strings.TrimSpace(sha)
	out := MergeReadiness{SHA: sha}
	if sha == "" {
		out.Reason = "no candidate sha supplied"
		return out, nil
	}
	rows, err := l.AllRows()
	if err != nil {
		// Fail closed: an unreadable ledger is not an absence of dissent.
		return MergeReadiness{SHA: sha, Reason: "review ledger unreadable"}, err
	}
	reviewers := map[string]string{}
	unrecorded := map[string]bool{}
	for _, row := range rows {
		// Match by PREFIX. The ledger stores 40-char SHAs while callers routinely
		// hold a 12-char short form (PR head refs, packet names, pane names).
		// Exact matching returned "no verdict recorded" for candidates that had
		// several -- an absence that reads as safe and is wrong for the wrong
		// reason, which is the failure mode this whole type exists to prevent.
		if row.Event != string(EventVerdict) || !shaMatches(row.SHA, sha) {
			continue
		}
		verdict := strings.ToUpper(strings.TrimSpace(row.Verdict))
		if verdict == "" {
			continue
		}
		// Later verdicts from the SAME reviewer supersede earlier ones; verdicts
		// from DIFFERENT reviewers never supersede each other.
		name := strings.TrimSpace(row.Reviewer)
		reviewers[name] = verdict
		if row.Gate == GateProvenanceUnrecorded || strings.EqualFold(strings.TrimSpace(row.BuilderFamily), FamilyUnrecorded) {
			unrecorded[name] = true
		}
	}
	if len(reviewers) == 0 {
		out.Reason = "no verdict recorded for this candidate"
		return out, nil
	}
	names := make([]string, 0, len(reviewers))
	for name := range reviewers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := reviewers[name]
		out.Verdicts = append(out.Verdicts, name+"="+v)
		if unrecorded[name] {
			out.ProvenanceUnrecorded++
		}
		switch v {
		case string(VerdictPASS):
			out.Passes++
		case string(VerdictBLOCKED):
			out.Blocked++
		default:
			out.Failures++
		}
	}
	switch {
	case out.Blocked > 0:
		out.Reason = fmt.Sprintf("%d reviewer(s) recorded BLOCKED", out.Blocked)
	case out.Failures > 0 && out.Passes > 0:
		out.Reason = fmt.Sprintf("reviewers disagree (%d PASS, %d FAIL); a split decision is not a pass",
			out.Passes, out.Failures)
	case out.Failures > 0:
		out.Reason = fmt.Sprintf("%d reviewer(s) recorded FAIL", out.Failures)
	case out.ProvenanceUnrecorded > 0 && out.ProvenanceUnrecorded >= out.Passes:
		// Every passing review lacks provable authorship. The review is kept and
		// visible; the independence claim is not granted.
		out.Reason = fmt.Sprintf("%d PASS but provenance was never recorded; review is admitted, "+
			"cross-family independence cannot be claimed", out.Passes)
	default:
		out.Ready = true
		out.Reason = fmt.Sprintf("%d PASS, no dissent", out.Passes)
	}
	return out, nil
}
