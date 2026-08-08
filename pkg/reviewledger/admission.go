package reviewledger

import (
	"fmt"
	"sort"
)

// AdmissionOpts binds the exact-current-candidate context a merge caller
// asserts. Every field is required: Admit fails closed on any that are
// missing, since a missing field means the caller cannot prove what it is
// asking to admit.
type AdmissionOpts struct {
	CandidateSHA   string
	Task           string
	Lease          string
	PatchURL       string
	AuthorFamily   string
	AuthorIdentity string
}

// AdmissionResult is the structured decision Admit emits. Callers must gate
// on Admitted — Reason is diagnostic text only, never a verdict signal.
type AdmissionResult struct {
	Admitted       bool
	Reason         string
	SHA            string
	Reviewer       string
	ReviewerFamily string
	Tier           string
	Lease          string
	PatchURL       string
	// VerificationDigest is the admitted verdict's own test-gate digest.
	// Admit already refuses a verdict that lacks one; surfacing it here lets
	// the completion receipt bind to the digest that was actually admitted
	// rather than one the caller re-reads (and could re-read from a
	// different, later verdict row).
	VerificationDigest string
}

// admissionRejected is the sentinel error Admit returns alongside a non-nil,
// non-admitted AdmissionResult, so callers can distinguish "policy refused
// this candidate" from an I/O failure without parsing prose.
type admissionRejected struct {
	sha    string
	reason string
}

func (e *admissionRejected) Error() string {
	return fmt.Sprintf("herd-merge-admission: refuse sha=%s reason=%q", e.sha, e.reason)
}

func reject(sha, reason string) (*AdmissionResult, error) {
	return &AdmissionResult{Admitted: false, Reason: reason, SHA: sha}, &admissionRejected{sha: sha, reason: reason}
}

// Admit is the merge-admission gate. It consumes one validated,
// exact-current-candidate review-ledger verdict record whose reviewer
// identity, reviewer family, task, lease generation, patch ID, and
// verification digest satisfy policy, and whose risk tier is on record —
// or it fails closed with a structured, non-prose rejection reason.
//
// Admit only ever reasons over structured LedgerRow.Verdict == PASS records
// durably appended through (*Ledger).Verdict, freshly re-read from disk on
// every call, and only for the exact SHA opts.CandidateSHA names. It never
// treats free text (a PR comment, a chat message, an "in progress" review
// request) as a verdict — there is no code path that parses prose here.
func (l *Ledger) Admit(opts AdmissionOpts) (*AdmissionResult, error) {
	if opts.CandidateSHA == "" {
		return reject("", "candidate sha required")
	}
	sha := l.NormalizeSHA(opts.CandidateSHA)
	if opts.Task == "" {
		return reject(sha, "active task required")
	}
	if opts.Lease == "" {
		return reject(sha, "active lease generation required")
	}
	if opts.PatchURL == "" {
		return reject(sha, "patch id required")
	}

	rows, err := readRows(l.Path)
	if err != nil {
		return nil, err
	}
	qrows, err := readRows(l.QueuePath)
	if err != nil {
		return nil, err
	}

	done := make(map[string]bool)
	for _, r := range qrows {
		if r.Event == string(EventConsumed) {
			done[r.SHA] = true
		}
	}
	if done[sha] {
		return reject(sha, "candidate already consumed (exactly-once admission spent)")
	}

	// Exact-SHA gate: only a launch record for this precise candidate SHA
	// counts. A stale or unknown SHA has nothing to admit against.
	launch := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventRecord) && r.SHA == sha {
			launch[r.SHA+":"+r.Reviewer] = r
		}
	}
	if len(launch) == 0 {
		return reject(sha, "no launch record for exact candidate sha (stale or unknown sha)")
	}

	latest := make(map[string]LedgerRow)
	var order []string
	for _, r := range rows {
		if r.Event == string(EventVerdict) && r.SHA == sha {
			k := r.SHA + ":" + r.Reviewer
			if _, seen := latest[k]; !seen {
				order = append(order, k)
			}
			latest[k] = r
		}
	}
	if len(latest) == 0 {
		return reject(sha, "no verdict for exact candidate sha (reviewer still working or absent)")
	}

	// SHA-level veto gate: any unsuperseded FAIL/BLOCKED from a
	// non-coordinator reviewer with a provable launch family blocks
	// admission outright, regardless of any PASS elsewhere for this SHA.
	for k, verdict := range latest {
		if verdict.Verdict != string(VerdictFAIL) && verdict.Verdict != string(VerdictBLOCKED) {
			continue
		}
		if l.isCoordinator(verdict.Reviewer) {
			continue
		}
		launchRow, hasLaunch := launch[k]
		if hasLaunch && launchRow.BuilderFamily != "" && FamilyAllowlist[launchRow.BuilderFamily] {
			return reject(sha, fmt.Sprintf("unsuperseded %s veto from reviewer=%s", verdict.Verdict, verdict.Reviewer))
		}
	}

	sort.Strings(order)
	lastReason := "no verdict satisfied merge-admission policy"
	for _, k := range order {
		verdict := latest[k]
		if verdict.Verdict != string(VerdictPASS) {
			continue
		}
		reviewer := verdict.Reviewer
		if l.isCoordinator(reviewer) {
			lastReason = "coordinator verdicts never qualify as merge authority"
			continue
		}
		launchRow, hasLaunch := launch[k]
		if !hasLaunch {
			lastReason = "verdict has no matching launch record"
			continue
		}
		if launchRow.BuilderFamily == "" || !FamilyAllowlist[launchRow.BuilderFamily] {
			lastReason = "author family missing or unknown"
			continue
		}

		fs := resolveFamily(launchRow.BuilderFamily, launchRow.ReviewerFamily, verdict.BuilderFamily, verdict.ReviewerFamily)
		if fs.State == familyConflict {
			lastReason = "reviewer family conflicts between launch and verdict records"
			continue
		}
		reviewerFamily := fs.Value
		if reviewerFamily == "" || !FamilyAllowlist[reviewerFamily] {
			lastReason = "reviewer family missing or unknown"
			continue
		}
		// Provenance gate (family): an author-family reviewer never admits.
		if reviewerFamily == launchRow.BuilderFamily {
			lastReason = "reviewer family matches author family (self-verdict)"
			continue
		}
		if opts.AuthorFamily != "" && reviewerFamily == opts.AuthorFamily {
			lastReason = "reviewer family matches asserted author family (self-verdict)"
			continue
		}

		// Provenance gate (identity): a shared account/session cannot
		// supply reviewer provenance even under a different family label.
		if reviewer == "" {
			lastReason = "reviewer identity missing"
			continue
		}
		if launchRow.BuilderIdentity != "" && reviewer == launchRow.BuilderIdentity {
			lastReason = "reviewer identity matches author identity (shared account)"
			continue
		}
		if opts.AuthorIdentity != "" && reviewer == opts.AuthorIdentity {
			lastReason = "reviewer identity matches asserted author identity (shared account)"
			continue
		}

		if verdict.Task != opts.Task {
			lastReason = "verdict task does not match active task"
			continue
		}
		if verdict.Lease == "" || verdict.Lease != opts.Lease {
			lastReason = "verdict lease does not match active lease generation (stale lease)"
			continue
		}
		if verdict.PatchURL == "" || verdict.PatchURL != opts.PatchURL {
			lastReason = "verdict patch id does not match candidate patch id"
			continue
		}
		if verdict.VerificationDigest == "" {
			lastReason = "verdict missing verification digest"
			continue
		}

		tier, err := l.Tier(sha)
		if err != nil {
			return nil, err
		}
		if tier == "" {
			lastReason = "no risk tier recorded for candidate sha"
			continue
		}

		return &AdmissionResult{
			Admitted:           true,
			Reason:             "validated independent verdict for exact candidate",
			SHA:                sha,
			Reviewer:           reviewer,
			ReviewerFamily:     reviewerFamily,
			Tier:               tier,
			Lease:              verdict.Lease,
			PatchURL:           verdict.PatchURL,
			VerificationDigest: verdict.VerificationDigest,
		}, nil
	}

	return reject(sha, lastReason)
}
