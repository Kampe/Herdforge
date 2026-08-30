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
	// OperatorDecidable marks a candidate that is blocked ONLY because builder
	// provenance was never recorded -- no FAIL, no BLOCKED, no missing verdict.
	//
	// FAC-668: this class had no path forward. On a fleet where commits are
	// authored under one shared human identity with no trailers, the builder
	// family is genuinely unknowable after the fact, so "provenance was never
	// recorded" was a permanent verdict on work that had a real PASS. Seven
	// candidates sat there, and a reviewer reporting "cannot merge, provenance
	// not set" was correct and had nowhere to go.
	//
	// A gate nobody can satisfy is not a safety property, it is an outage. This
	// does not weaken the gate: it names the class so an operator can decide,
	// and so the decision is visible instead of being taken by bypassing the
	// gate entirely.
	OperatorDecidable bool `json:"operator_decidable,omitempty"`
	// AdmittedWithoutProvenance is set when the caller explicitly accepted an
	// unrecorded-provenance candidate. It is recorded on the result so the
	// choice is auditable rather than implicit.
	AdmittedWithoutProvenance bool `json:"admitted_without_provenance,omitempty"`
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
// MergeReadinessFor reports readiness with the default fail-closed posture.
func (l *Ledger) MergeReadinessFor(sha string) (MergeReadiness, error) {
	return l.mergeReadinessFor(sha, false)
}

// MergeReadinessAllowingUnrecordedProvenance is the same decision with the
// unrecorded-provenance class explicitly accepted. The caller is asserting that
// a review it cannot prove was cross-family is good enough for THIS merge; the
// result records that so the choice is auditable (FAC-668).
func (l *Ledger) MergeReadinessAllowingUnrecordedProvenance(sha string) (MergeReadiness, error) {
	return l.mergeReadinessFor(sha, true)
}

func (l *Ledger) mergeReadinessFor(sha string, allowUnrecorded bool) (MergeReadiness, error) {
	sha = strings.TrimSpace(sha)
	out := MergeReadiness{SHA: sha}
	if sha == "" {
		out.Reason = "no candidate sha supplied"
		return out, nil
	}
	evidence, err := l.loadReducedAdmissionEvidence(sha)
	if err != nil {
		// Fail closed and retain the exact evidence source named by the shared
		// loader. An unreadable queue is not an unreadable ledger, and neither is
		// an absence of dissent.
		return MergeReadiness{SHA: sha, Reason: err.Error()}, err
	}
	// FAC-641: an EMPTY ledger is not "nothing has been reviewed".
	//
	// The coordinator runs from its own worktree, whose .herd/review-ledger.jsonl
	// is a 0-byte file while the authoritative shared ledger holds 1968 rows. Read
	// against the empty one, this reported all 71 open heads as no-verdict --
	// which would have dispatched 71 reviews for work that was already reviewed,
	// and reported 4 genuinely-ready candidates as unreviewed.
	//
	// That is the exact defect this type exists to prevent, committed inside it: an
	// absence treated as a definitive negative. A ledger with no rows at all means
	// the caller is pointed at the wrong file, not that the fleet has never
	// reviewed anything, so it fails closed and says so.
	if len(evidence.Rows) == 0 {
		return MergeReadiness{SHA: sha, Reason: "review ledger is EMPTY; refusing to report no-verdict from a ledger with zero rows (wrong repo root?)"},
			fmt.Errorf("review ledger has no rows: refusing to infer review state from an empty ledger")
	}
	if len(evidence.MatchingSHAs) > 1 {
		out.Reason = fmt.Sprintf("candidate sha prefix is ambiguous: matched %q", evidence.MatchingSHAs)
		return out, fmt.Errorf("review readiness candidate sha %q matches multiple ledger shas", sha)
	}
	reviewers := map[string]string{}
	latestVerdict := map[string]LedgerRow{}
	launch := map[string]LedgerRow{}
	for _, row := range evidence.Rows {
		if row.Event != string(EventRecord) || !shaMatches(row.SHA, evidence.CandidateSHA) {
			continue
		}
		launch[strings.TrimSpace(row.Reviewer)] = row
	}
	for _, row := range evidence.Rows {
		// Match by PREFIX. The ledger stores 40-char SHAs while callers routinely
		// hold a 12-char short form (PR head refs, packet names, pane names).
		// Exact matching returned "no verdict recorded" for candidates that had
		// several -- an absence that reads as safe and is wrong for the wrong
		// reason, which is the failure mode this whole type exists to prevent.
		if row.Event != string(EventVerdict) || !shaMatches(row.SHA, evidence.CandidateSHA) {
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
		latestVerdict[name] = row
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
		verdictRow := latestVerdict[name]
		launchRow, hasLaunch := launch[name]
		provenanceUnrecorded := false
		if hasLaunch {
			provenanceUnrecorded = isUnrecordedProvenance(launchRow.Gate, launchRow.BuilderFamily)
		} else {
			// Keep a verdict's historical marker visible, but never let that
			// marker substitute for the launch row the admission predicate needs.
			provenanceUnrecorded = isUnrecordedProvenance(verdictRow.Gate, verdictRow.BuilderFamily)
		}
		if provenanceUnrecorded {
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
	default:
		strict, strictErr := l.admitReducedEvidence(evidence, reducedAdmissionPolicy{})
		if strictErr == nil && strict != nil && strict.Admitted {
			out.Ready = true
			out.Reason = fmt.Sprintf("%d PASS, no dissent; reduced admission agrees", out.Passes)
			break
		}

		// FAC-668 remains a one-condition explicit choice. Evaluating the SAME
		// predicate with only that policy bit changed proves no missing digest,
		// tier, identity, reviewer family, veto, or exactly-once condition is
		// accidentally rescued by the flag.
		override, overrideErr := l.admitReducedEvidence(evidence, reducedAdmissionPolicy{allowUnrecordedProvenance: true})
		if overrideErr == nil && override != nil && override.Admitted && out.ProvenanceUnrecorded >= out.Passes {
			out.OperatorDecidable = true
			if allowUnrecorded {
				out.Ready = true
				out.AdmittedWithoutProvenance = true
				out.Reason = fmt.Sprintf("%d PASS, admitted WITHOUT provable builder provenance by explicit operator choice; "+
					"cross-family independence is NOT claimed for this merge", out.Passes)
				break
			}
		}

		decision, decisionErr := strict, strictErr
		if allowUnrecorded {
			// The operator already chose to waive only unknown provenance. Name
			// the next condition from that exact policy evaluation, rather than
			// repeating the strict provenance refusal the operator just resolved.
			decision, decisionErr = override, overrideErr
		}
		admissionReason := "admission refused the candidate"
		if decision != nil && decision.Reason != "" {
			admissionReason = decision.Reason
		} else if decisionErr != nil {
			admissionReason = decisionErr.Error()
		}
		out.Reason = "reduced admission would refuse: " + admissionReason
		if out.OperatorDecidable {
			out.Reason += "; re-run with --allow-unrecorded-provenance to accept only the unknown-provenance condition, " +
				"or use `herd review-ingest <artifact> --reresolve-provenance` when a reaching historical launch receipt now exists; " +
				"record the builder family at dispatch with `herd launch-record` for future candidates"
		}
	}
	return out, nil
}
