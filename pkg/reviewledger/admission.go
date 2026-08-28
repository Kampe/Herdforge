package reviewledger

import (
	"fmt"
	"sort"
	"strings"
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
	AuthorFamily       string
}

type ReducedAdmissionOpts struct{ CandidateSHA string }

// AdmitReduced preserves exact-SHA and independent-review evidence for
// legacy verdicts that predate lease and patch bindings. It never fabricates
// those absent fields and is separate from the full Admit gate.
func (l *Ledger) AdmitReduced(opts ReducedAdmissionOpts) (*AdmissionResult, error) {
	if opts.CandidateSHA == "" {
		return reject("", "candidate sha required")
	}
	sha := l.NormalizeSHA(opts.CandidateSHA)
	rows, err := readRows(l.Path)
	if err != nil {
		return nil, err
	}
	qrows, err := readRows(l.QueuePath)
	if err != nil {
		return nil, err
	}
	for _, row := range qrows {
		if row.Event == string(EventConsumed) && row.SHA == sha {
			return reject(sha, "candidate already consumed (exactly-once admission spent)")
		}
	}
	launch := map[string]LedgerRow{}
	for _, r := range rows {
		if r.Event == string(EventRecord) && r.SHA == sha {
			launch[r.SHA+":"+r.Reviewer] = r
		}
	}
	if len(launch) == 0 {
		return reject(sha, "no launch record for exact candidate sha")
	}
	latest := map[string]LedgerRow{}
	for _, r := range rows {
		if r.Event == string(EventVerdict) && r.SHA == sha {
			latest[r.SHA+":"+r.Reviewer] = r
		}
	}
	// FAC-630: every `continue` below used to land on one generic refusal, so an
	// operator holding a valid PASS and a real digest could not tell WHICH of
	// eight conditions failed. On the live fleet that read as "the digest is
	// missing" when the digest was present and the launch family was the
	// problem, and again as the same string when the risk tier was the problem.
	// Absence of a reason is not a reason: record why each candidate verdict was
	// skipped and report the most specific one.
	var skipped []string
	note := func(reviewer, why string) {
		skipped = append(skipped, fmt.Sprintf("reviewer=%s: %s", reviewer, why))
	}
	for key, verdict := range latest {
		if (verdict.Verdict == string(VerdictFAIL) || verdict.Verdict == string(VerdictBLOCKED)) && !l.isCoordinator(verdict.Reviewer) {
			if launchRow, ok := launch[key]; ok && launchRow.BuilderFamily != "" && FamilyAllowlist[launchRow.BuilderFamily] {
				return reject(sha, fmt.Sprintf("unsuperseded %s veto from reviewer=%s", verdict.Verdict, verdict.Reviewer))
			}
		}
		if verdict.Verdict != string(VerdictPASS) {
			note(verdict.Reviewer, "verdict is "+verdict.Verdict+", not PASS")
			continue
		}
		if l.isCoordinator(verdict.Reviewer) {
			note(verdict.Reviewer, "reviewer is a coordinator; a self-review is not independent")
			continue
		}
		launchRow, ok := launch[key]
		if !ok {
			note(verdict.Reviewer, "no launch record row for this reviewer on this candidate")
			continue
		}
		if launchRow.BuilderFamily == "" {
			note(verdict.Reviewer, "launch record has no builder family")
			continue
		}
		if !FamilyAllowlist[launchRow.BuilderFamily] {
			note(verdict.Reviewer, fmt.Sprintf("launch record builder family %q is not an allowlisted vendor family "+
				"(a candidate whose provenance was recorded as %q cannot establish cross-family independence)",
				launchRow.BuilderFamily, launchRow.BuilderFamily))
			continue
		}
		families := resolveFamily(launchRow.BuilderFamily, launchRow.ReviewerFamily, verdict.BuilderFamily, verdict.ReviewerFamily)
		if families.State != familySet {
			note(verdict.Reviewer, "reviewer family could not be resolved from the launch and verdict rows")
			continue
		}
		if families.Value == launchRow.BuilderFamily {
			note(verdict.Reviewer, fmt.Sprintf("reviewer family %q equals the builder family; not independent", families.Value))
			continue
		}
		if !FamilyAllowlist[families.Value] {
			note(verdict.Reviewer, fmt.Sprintf("reviewer family %q is not an allowlisted vendor family", families.Value))
			continue
		}
		if verdict.Reviewer == "" {
			note("(unnamed)", "verdict has no reviewer identity")
			continue
		}
		if launchRow.BuilderIdentity != "" && verdict.Reviewer == launchRow.BuilderIdentity {
			note(verdict.Reviewer, "reviewer identity equals the builder identity; a self-review is not independent")
			continue
		}
		if verdict.VerificationDigest == "" {
			note(verdict.Reviewer, "verdict carries no verification digest")
			continue
		}
		tier, err := l.Tier(sha)
		if err != nil {
			return nil, err
		}
		if tier == "" {
			note(verdict.Reviewer, "no risk tier is recorded for this candidate on any launch record row")
			continue
		}
		return &AdmissionResult{Admitted: true, Reason: "validated independent verdict for exact candidate (reduced provenance)", SHA: sha, Reviewer: verdict.Reviewer, ReviewerFamily: families.Value, Tier: tier, VerificationDigest: verdict.VerificationDigest, AuthorFamily: launchRow.BuilderFamily}, nil
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		return reject(sha, "no independent PASS verdict with durable verification evidence; "+
			strings.Join(skipped, "; "))
	}
	return reject(sha, "no independent PASS verdict with durable verification evidence: "+
		"no verdict rows exist for this candidate")
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
			AuthorFamily:       launchRow.BuilderFamily,
		}, nil
	}

	return reject(sha, lastReason)
}
