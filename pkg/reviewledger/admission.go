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

// reducedAdmissionEvidence is one immutable ledger/queue observation. Reduced
// admission and merge readiness both evaluate this exact shape rather than
// independently selecting different rows for the same candidate.
type reducedAdmissionEvidence struct {
	RequestedSHA string
	CandidateSHA string
	MatchingSHAs []string
	Rows         []LedgerRow
	QueueRows    []LedgerRow
}

type reducedAdmissionPolicy struct {
	// allowUnrecordedProvenance preserves FAC-668's explicit operator choice.
	// It relaxes only the builder-family proof; all digest, tier, reviewer,
	// identity, veto, and exactly-once gates remain active.
	allowUnrecordedProvenance bool
}

// AdmitReduced preserves exact-SHA and independent-review evidence for
// legacy verdicts that predate lease and patch bindings. It never fabricates
// those absent fields and is separate from the full Admit gate.
func (l *Ledger) AdmitReduced(opts ReducedAdmissionOpts) (*AdmissionResult, error) {
	evidence, err := l.loadReducedAdmissionEvidence(opts.CandidateSHA)
	if err != nil {
		return nil, err
	}
	return l.admitReducedEvidence(evidence, reducedAdmissionPolicy{})
}

func (l *Ledger) loadReducedAdmissionEvidence(candidateSHA string) (reducedAdmissionEvidence, error) {
	requested := strings.TrimSpace(candidateSHA)
	evidence := reducedAdmissionEvidence{RequestedSHA: requested}
	if requested == "" {
		return evidence, nil
	}
	evidence.CandidateSHA = l.NormalizeSHA(requested)

	rows, err := readRows(l.Path)
	if err != nil {
		return evidence, fmt.Errorf("load reduced admission evidence: admission_condition=review_ledger observed ledger_readable=false: %w", err)
	}
	qrows, err := readRows(l.QueuePath)
	if err != nil {
		return evidence, fmt.Errorf("load reduced admission evidence: admission_condition=admission_queue observed queue_readable=false: %w", err)
	}
	evidence.Rows = rows
	evidence.QueueRows = qrows

	matches := make(map[string]struct{})
	for _, row := range rows {
		if row.Event != string(EventRecord) && row.Event != string(EventVerdict) {
			continue
		}
		if shaMatches(row.SHA, evidence.CandidateSHA) {
			matches[row.SHA] = struct{}{}
		}
	}
	for sha := range matches {
		evidence.MatchingSHAs = append(evidence.MatchingSHAs, sha)
	}
	sort.Strings(evidence.MatchingSHAs)
	if len(evidence.MatchingSHAs) == 1 {
		evidence.CandidateSHA = evidence.MatchingSHAs[0]
	}
	return evidence, nil
}

// admitReducedEvidence is the single reduced-admission predicate. It performs
// no reads and mutates neither the snapshot nor ledger state.
func (l *Ledger) admitReducedEvidence(evidence reducedAdmissionEvidence, policy reducedAdmissionPolicy) (*AdmissionResult, error) {
	if evidence.RequestedSHA == "" {
		return reject("", `admission_condition=candidate_sha observed candidate_sha="" required=true`)
	}
	sha := evidence.CandidateSHA
	if len(evidence.MatchingSHAs) > 1 {
		return reject(sha, fmt.Sprintf("admission_condition=candidate_sha observed candidate_sha=%q ambiguous=true matching_shas=%q",
			evidence.RequestedSHA, evidence.MatchingSHAs))
	}
	for _, row := range evidence.QueueRows {
		if row.Event == string(EventConsumed) && shaMatches(row.SHA, sha) {
			return reject(sha, "admission_condition=consumption observed consumed=true (exactly-once admission spent)")
		}
	}
	launch := make(map[string]LedgerRow)
	for _, row := range evidence.Rows {
		if row.Event == string(EventRecord) && shaMatches(row.SHA, sha) {
			launch[strings.TrimSpace(row.Reviewer)] = row
		}
	}
	if len(launch) == 0 {
		return reject(sha, "admission_condition=launch_record observed launch_rows=0 for exact candidate sha")
	}
	latest := make(map[string]LedgerRow)
	for _, row := range evidence.Rows {
		if row.Event == string(EventVerdict) && shaMatches(row.SHA, sha) {
			latest[strings.TrimSpace(row.Reviewer)] = row
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
	note := func(reviewer, condition, observed string) {
		skipped = append(skipped, fmt.Sprintf("reviewer=%q condition=%s observed %s", reviewer, condition, observed))
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// Vetoes are evaluated before PASS candidates. Returning from one map walk
	// made admission depend on randomized iteration order when a PASS and veto
	// coexisted for the same SHA.
	for _, key := range keys {
		verdict := latest[key]
		if verdict.Verdict != string(VerdictFAIL) && verdict.Verdict != string(VerdictBLOCKED) {
			continue
		}
		if l.isCoordinator(strings.TrimSpace(verdict.Reviewer)) {
			continue
		}
		launchRow, ok := launch[key]
		if ok && strings.TrimSpace(launchRow.BuilderFamily) != "" && FamilyAllowlist[strings.TrimSpace(launchRow.BuilderFamily)] {
			return reject(sha, fmt.Sprintf("admission_condition=veto observed verdict=%q reviewer=%q superseded=false",
				verdict.Verdict, strings.TrimSpace(verdict.Reviewer)))
		}
	}

	tier := ""
	for _, row := range evidence.Rows {
		if row.Event == string(EventRecord) && shaMatches(row.SHA, sha) && strings.TrimSpace(row.Tier) != "" {
			tier = strings.TrimSpace(row.Tier)
		}
	}
	for _, key := range keys {
		verdict := latest[key]
		reviewer := strings.TrimSpace(verdict.Reviewer)
		if verdict.Verdict != string(VerdictPASS) {
			note(reviewer, "verdict", fmt.Sprintf("verdict=%q required=%q", verdict.Verdict, VerdictPASS))
			continue
		}
		if l.isCoordinator(reviewer) {
			note(reviewer, "reviewer_authority", fmt.Sprintf("reviewer=%q coordinator=true independent=false", reviewer))
			continue
		}
		launchRow, ok := launch[key]
		if !ok {
			note(reviewer, "launch_record", fmt.Sprintf("launch_record_present=false reviewer=%q", reviewer))
			continue
		}
		builderFamily := strings.TrimSpace(launchRow.BuilderFamily)
		if builderFamily == "" {
			note(reviewer, "builder_family", `builder_family=""`)
			continue
		}
		allowUnrecorded := policy.allowUnrecordedProvenance && isUnrecordedProvenance(launchRow.Gate, builderFamily)
		if !FamilyAllowlist[builderFamily] && !allowUnrecorded {
			note(reviewer, "builder_family", fmt.Sprintf("builder_family=%q allowlisted=false independent=false", builderFamily))
			continue
		}
		families := resolveFamily(builderFamily, strings.TrimSpace(launchRow.ReviewerFamily),
			strings.TrimSpace(verdict.BuilderFamily), strings.TrimSpace(verdict.ReviewerFamily))
		if allowUnrecorded {
			// The explicit override accepts unknown builder provenance; it must not
			// launder the verdict row's later assertion into a builder identity.
			families = resolveFamily("", strings.TrimSpace(launchRow.ReviewerFamily), "", strings.TrimSpace(verdict.ReviewerFamily))
		}
		if families.State != familySet {
			state := "unset"
			if families.State == familyConflict {
				state = "conflict"
			}
			note(reviewer, "reviewer_family", fmt.Sprintf(
				"reviewer_family_state=%s reviewer_family=%q launch_reviewer_family=%q verdict_reviewer_family=%q launch_builder_family=%q verdict_builder_family=%q",
				state, families.Value, strings.TrimSpace(launchRow.ReviewerFamily), strings.TrimSpace(verdict.ReviewerFamily),
				builderFamily, strings.TrimSpace(verdict.BuilderFamily)))
			continue
		}
		if !allowUnrecorded && families.Value == builderFamily {
			note(reviewer, "family_independence", fmt.Sprintf(
				"reviewer family %q equals the builder family; reviewer_family=%q builder_family=%q independent=false",
				families.Value, families.Value, builderFamily))
			continue
		}
		if !FamilyAllowlist[families.Value] {
			note(reviewer, "reviewer_family", fmt.Sprintf("reviewer_family=%q allowlisted=false", families.Value))
			continue
		}
		if reviewer == "" {
			note("", "reviewer_identity", `reviewer=""`)
			continue
		}
		if strings.TrimSpace(launchRow.BuilderIdentity) != "" && reviewer == strings.TrimSpace(launchRow.BuilderIdentity) {
			note(reviewer, "identity_independence", fmt.Sprintf("reviewer=%q builder_identity_match=true independent=false", reviewer))
			continue
		}
		if strings.TrimSpace(verdict.VerificationDigest) == "" {
			note(reviewer, "verification_digest", `verdict carries no verification digest; verification_digest=""`)
			continue
		}
		if tier == "" {
			note(reviewer, "risk_tier", `no risk tier is recorded for this candidate; risk_tier=""`)
			continue
		}
		reason := "validated independent verdict for exact candidate (reduced provenance)"
		if allowUnrecorded {
			reason = "validated verdict under explicit unrecorded-provenance policy; cross-family independence is not claimed"
		}
		return &AdmissionResult{Admitted: true, Reason: reason, SHA: sha, Reviewer: reviewer, ReviewerFamily: families.Value, Tier: tier, VerificationDigest: strings.TrimSpace(verdict.VerificationDigest), AuthorFamily: builderFamily}, nil
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		return reject(sha, "no independent PASS verdict with durable verification evidence; "+
			strings.Join(skipped, "; "))
	}
	return reject(sha, "no independent PASS verdict with durable verification evidence: "+
		"no verdict rows exist for this candidate; admission_condition=verdict observed verdict_rows=0")
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
