package reviewledger

import (
	"fmt"
	"sort"
	"strings"
)

// RecordOpts carries optional fields for Record.
type RecordOpts struct {
	SHA             string
	Branch          string
	BuilderFamily   string
	BuilderIdentity string
	ReviewerFamily  string
	Reviewer        string
	Provider        string
	Model           string
	Pane            string
	Pid             string
	Artifact        string
	Gate            string
	Tier            string
	Task            string
	Lease           string
}

// IngestOpts is the authenticated handoff from a reviewer verdict artifact.
// The record and verdict identities must describe the same exact candidate and
// reviewer; callers must not create a PASS row without its admission
// provenance.
type IngestOpts struct {
	Record  RecordOpts
	Verdict VerdictOpts
}

// Record appends a record event. Validates builder family on independent gates.
func (l *Ledger) Record(opts RecordOpts) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.record(opts)
}

func (l *Ledger) record(opts RecordOpts) error {
	if opts.Gate == "mechanical" {
		if opts.BuilderFamily != "" && opts.BuilderFamily != "mechanical" {
			return fmt.Errorf("mechanical record must not carry builder family %q", opts.BuilderFamily)
		}
	} else {
		if opts.BuilderFamily == "" {
			return fmt.Errorf("record needs --builder-family for an independent review")
		}
		if !FamilyAllowlist[opts.BuilderFamily] {
			return fmt.Errorf("unknown builder family %q (refusing unprovable review provenance)", opts.BuilderFamily)
		}
	}
	row := &LedgerRow{
		Event:           string(EventRecord),
		SHA:             opts.SHA,
		Branch:          opts.Branch,
		BuilderFamily:   opts.BuilderFamily,
		BuilderIdentity: opts.BuilderIdentity,
		ReviewerFamily:  opts.ReviewerFamily,
		Reviewer:        opts.Reviewer,
		Provider:        opts.Provider,
		Model:           opts.Model,
		Pane:            opts.Pane,
		Pid:             opts.Pid,
		Artifact:        opts.Artifact,
		Gate:            opts.Gate,
		Tier:            opts.Tier,
		Task:            opts.Task,
		Lease:           opts.Lease,
	}
	return l.appendRow(l.Path, row)
}

// EnsureRecord makes reviewer admission provenance durable and idempotent.
// Some supervisors retain a valid broker verdict after the launch pane has
// already gone away; ingesting that artifact must not leave an otherwise
// admissible PASS permanently unharvestable merely because the launch record
// was not copied into the local ledger first.
func (l *Ledger) EnsureRecord(opts RecordOpts) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ensureRecord(opts)
}

func (l *Ledger) ensureRecord(opts RecordOpts) error {
	rows, err := readRows(l.Path)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Event == string(EventRecord) && row.SHA == opts.SHA && row.Reviewer == opts.Reviewer {
			return nil
		}
	}
	return l.record(opts)
}

// Ingest ensures the matching exact-SHA admission record before persisting a
// PASS verdict. Provenance validation happens before either row is written;
// repeating an accepted handoff is idempotent.
func (l *Ledger) Ingest(opts IngestOpts) (bool, error) {
	if opts.Verdict.Verdict == VerdictPASS {
		if strings.TrimSpace(opts.Record.SHA) == "" || opts.Record.SHA != opts.Verdict.SHA {
			return false, fmt.Errorf("review ingest refused: admission and verdict must bind the exact same sha")
		}
		if strings.TrimSpace(opts.Record.Reviewer) == "" || opts.Record.Reviewer != opts.Verdict.Reviewer {
			return false, fmt.Errorf("review ingest refused: admission and verdict must bind the same reviewer")
		}
		if strings.TrimSpace(opts.Record.Branch) == "" {
			return false, fmt.Errorf("review ingest refused: missing branch provenance")
		}
		if strings.TrimSpace(opts.Record.Artifact) == "" || opts.Record.Artifact != opts.Verdict.Artifact {
			return false, fmt.Errorf("review ingest refused: missing or mismatched authenticated verdict artifact")
		}
		if opts.Record.Gate != "mechanical" && (strings.TrimSpace(opts.Record.BuilderFamily) == "" || strings.TrimSpace(opts.Verdict.ReviewerFamily) == "") {
			return false, fmt.Errorf("review ingest refused: missing independent reviewer provenance")
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureRecord(opts.Record); err != nil {
		return false, err
	}
	return l.verdict(opts.Verdict)
}

// Tier returns the newest recorded tier for a sha, or empty string.
func (l *Ledger) Tier(sha string) (string, error) {
	rows, err := readRows(l.Path)
	if err != nil {
		return "", err
	}
	sha = l.NormalizeSHA(sha)
	var tier string
	for _, r := range rows {
		if r.Event == string(EventRecord) && r.SHA == sha && r.Tier != "" {
			tier = r.Tier
		}
	}
	return tier, nil
}

// TierReport is the durable evidence surfaced by the review-ledger CLI.
// Keeping the candidate SHA alongside its tier prevents an empty tier from
// being mistaken for a successful lookup of a different candidate.
type TierReport struct {
	SHA  string `json:"sha"`
	Tier string `json:"tier"`
}

// VerdictOpts carries fields for Verdict.
type VerdictOpts struct {
	SHA            string
	Reviewer       string
	Verdict        Verdict
	Artifact       string
	ReviewerFamily string
	BuilderFamily  string
	Branch         string
	Lane           string
	Task           string
	Lease          string
	PatchURL       string
	VfyDigest      string
	FindingsRef    string
	CandidateSHA   string
}

// Verdict appends a verdict event and side-writes to the queue.
// Duplicate verdicts (same SHA+Reviewer) are idempotent — second call is a no-op.
func (l *Ledger) Verdict(opts VerdictOpts) (enqueued bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.verdict(opts)
}

func (l *Ledger) verdict(opts VerdictOpts) (enqueued bool, err error) {
	if err := ValidVerdict(opts.Verdict); err != nil {
		return false, err
	}
	if opts.ReviewerFamily != "" && !FamilyAllowlist[opts.ReviewerFamily] {
		return false, fmt.Errorf("unknown reviewer family %q (refusing unprovable review provenance)", opts.ReviewerFamily)
	}

	// Idempotent: skip if a verdict already exists for this SHA+Reviewer.
	rows, err := readRows(l.Path)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Event == string(EventVerdict) && r.SHA == opts.SHA && r.Reviewer == opts.Reviewer {
			return opts.Verdict == VerdictPASS, nil
		}
	}

	row := &LedgerRow{
		Event:              string(EventVerdict),
		SHA:                opts.SHA,
		Reviewer:           opts.Reviewer,
		Verdict:            string(opts.Verdict),
		Artifact:           opts.Artifact,
		Task:               opts.Task,
		Lease:              opts.Lease,
		PatchURL:           opts.PatchURL,
		VerificationDigest: opts.VfyDigest,
		FindingsRef:        opts.FindingsRef,
		CandidateSHA:       opts.CandidateSHA,
	}
	if opts.ReviewerFamily != "" {
		row.ReviewerFamily = opts.ReviewerFamily
	}
	if opts.BuilderFamily != "" {
		row.BuilderFamily = opts.BuilderFamily
	}
	if err := l.appendRow(l.Path, row); err != nil {
		return false, err
	}

	qRow := &LedgerRow{
		Event:    string(EventEnqueue),
		SHA:      opts.SHA,
		Reviewer: opts.Reviewer,
		Branch:   opts.Branch,
		Lane:     opts.Lane,
		Status:   "queued",
	}
	if opts.Verdict == VerdictFAIL || opts.Verdict == VerdictBLOCKED {
		qRow.Event = string(EventRevoked)
		qRow.Verdict = string(opts.Verdict)
		qRow.Status = "revoked"
	}
	if err := l.appendRow(l.QueuePath, qRow); err != nil {
		return false, err
	}
	return opts.Verdict == VerdictPASS, nil
}

// RepairOpts carries fields for Repair.
type RepairOpts struct {
	SHA          string
	RepairAuthor string
	Branch       string
	RepairFamily string
}

// DecisionOpts records an explicit relationship between review decisions.
// These events are deliberately separate from verdicts: a PASS on a later
// candidate does not silently supersede a FAIL on an unrelated candidate.
type DecisionOpts struct {
	SHA          string
	PreviousSHA  string
	Reviewer     string
	Reason       string
	FindingsRef  string
	CandidateSHA string
}

// ReconstructionOpts records an explicit, operator-attested identity
// substitution. SHA is the harvested identity and CandidateSHA is the
// originally reviewed identity; Reason is the content-equality attestation.
type ReconstructionOpts struct {
	SHA          string
	CandidateSHA string
	Branch       string
	Reason       string
	ContentProof string
}

// Reconstruction appends a distinct audit event for a reconstructed harvest.
// It never changes review eligibility or verdict state by itself.
func (l *Ledger) Reconstruction(opts ReconstructionOpts) error {
	if strings.TrimSpace(opts.SHA) == "" || strings.TrimSpace(opts.CandidateSHA) == "" {
		return fmt.Errorf("reconstruction requires harvested and reviewed sha")
	}
	proof := strings.TrimSpace(opts.ContentProof)
	if proof == "" {
		proof = strings.TrimSpace(opts.Reason)
	}
	if proof == "" {
		return fmt.Errorf("reconstruction requires content-equality attestation")
	}
	return l.appendRow(l.Path, &LedgerRow{
		Event: string(EventReconstruction), SHA: opts.SHA,
		CandidateSHA: opts.CandidateSHA, Branch: opts.Branch,
		Reason: opts.Reason, ContentProof: proof, Status: "attested",
	})
}

// Refutation records why an earlier review finding was rejected or disproven.
// It never changes the verdict row; consumers must interpret the relationship
// explicitly and retain the original evidence.
func (l *Ledger) Refutation(opts DecisionOpts) error {
	if strings.TrimSpace(opts.SHA) == "" || strings.TrimSpace(opts.Reason) == "" {
		return fmt.Errorf("refutation requires sha and reason")
	}
	return l.appendRow(l.Path, &LedgerRow{
		Event: string(EventRefutation), SHA: opts.SHA, CandidateSHA: opts.CandidateSHA,
		Reviewer: opts.Reviewer, FindingsRef: opts.FindingsRef, Reason: opts.Reason, Status: "refuted",
		Task: opts.PreviousSHA, // previous decision/candidate reference
	})
}

// Supersession records a deliberate replacement relationship. It is not
// inferred from chronology, so an unrelated PASS cannot erase an older FAIL.
func (l *Ledger) Supersession(opts DecisionOpts) error {
	if strings.TrimSpace(opts.SHA) == "" || strings.TrimSpace(opts.PreviousSHA) == "" || strings.TrimSpace(opts.Reason) == "" {
		return fmt.Errorf("supersession requires sha, previous sha, and reason")
	}
	return l.appendRow(l.Path, &LedgerRow{
		Event: string(EventSupersession), SHA: opts.SHA, CandidateSHA: opts.CandidateSHA,
		Reviewer: opts.Reviewer, FindingsRef: opts.FindingsRef, Reason: opts.Reason, Status: "superseded",
		Task: opts.PreviousSHA,
	})
}

// Repair appends a repair event.
func (l *Ledger) Repair(opts RepairOpts) error {
	row := &LedgerRow{
		Event:        string(EventRepair),
		SHA:          opts.SHA,
		RepairAuthor: opts.RepairAuthor,
		Branch:       opts.Branch,
	}
	if opts.RepairFamily != "" {
		row.RepairFamily = opts.RepairFamily
	}
	return l.appendRow(l.Path, row)
}

// Consumed marks a sha as consumed in both ledger and queue.
func (l *Ledger) Consumed(sha, mergeSHA string) error {
	row := &LedgerRow{
		Event:    string(EventConsumed),
		SHA:      sha,
		MergeSHA: mergeSHA,
	}
	if err := l.appendRow(l.Path, row); err != nil {
		return err
	}
	qRow := &LedgerRow{
		Event:    string(EventConsumed),
		SHA:      sha,
		MergeSHA: mergeSHA,
		Status:   "consumed",
	}
	return l.appendRow(l.QueuePath, qRow)
}

// EnqueueOpts carries fields for manual enqueue.
type EnqueueOpts struct {
	SHA      string
	Reviewer string
	Branch   string
	Lane     string
}

// Enqueue manually appends an enqueue event to the queue.
func (l *Ledger) Enqueue(opts EnqueueOpts) error {
	reviewer := opts.Reviewer
	if reviewer == "" {
		reviewer = "manual"
	}
	row := &LedgerRow{
		Event:    string(EventEnqueue),
		SHA:      opts.SHA,
		Reviewer: reviewer,
		Branch:   opts.Branch,
		Lane:     opts.Lane,
		Status:   "queued",
	}
	return l.appendRow(l.QueuePath, row)
}

// AllRows returns every row from the ledger file.
func (l *Ledger) AllRows() ([]LedgerRow, error) {
	return readRows(l.Path)
}

// QueueRows returns every row from the queue file.
func (l *Ledger) QueueRows() ([]LedgerRow, error) {
	return readRows(l.QueuePath)
}

// resolveFamily determines the effective reviewer family from launch and verdict rows.
func resolveFamily(lbf, lrf, vbf, vrf string) familyState {
	if lrf != "" && vrf != "" && lrf != vrf {
		return familyState{State: familyConflict}
	}
	if lbf != "" && vbf != "" && lbf != vbf {
		return familyState{State: familyConflict}
	}
	if lrf != "" {
		return familyState{Value: lrf, State: familySet}
	}
	return familyState{Value: vrf, State: familyUnset}
}

// Eligible returns true if sha is harvestable for the given builderFamily.
// A SHA-level FAIL/BLOCKED veto blocks eligibility regardless of PASS from other reviewers.
func (l *Ledger) Eligible(sha, builderFamily string) (bool, error) {
	rows, err := readRows(l.Path)
	if err != nil {
		return false, fmt.Errorf("herd-review-ledger: refuse sha=%s reason=ledger read error: %w", sha, err)
	}
	qrows, err := readRows(l.QueuePath)
	if err != nil {
		return false, fmt.Errorf("herd-review-ledger: refuse sha=%s reason=queue read error: %w", sha, err)
	}

	sha = l.NormalizeSHA(sha)
	hasRecord := false
	superseded := false
	for _, r := range rows {
		if r.Event == string(EventRecord) && r.SHA == sha {
			hasRecord = true
		}
		if r.Event == string(EventSupersession) && r.Task == sha {
			superseded = true
		}
	}
	if superseded {
		return false, fmt.Errorf("herd-review-ledger: refuse sha=%s reason=superseded", sha)
	}
	if !hasRecord {
		return false, fmt.Errorf("herd-review-ledger: refuse sha=%s reason=no record", sha)
	}

	launch := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventRecord) {
			k := r.SHA + ":" + r.Reviewer
			launch[k] = r
		}
	}

	latest := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventVerdict) {
			k := r.SHA + ":" + r.Reviewer
			latest[k] = r
		}
	}

	done := make(map[string]bool)
	for _, r := range qrows {
		if r.Event == string(EventConsumed) {
			done[r.SHA] = true
		}
	}

	if done[sha] {
		return false, fmt.Errorf("herd-review-ledger: refuse sha=%s reason=consumed", sha)
	}

	// SHA-level veto: any FAIL/BLOCKED from a valid reviewer blocks eligibility.
	hasVeto := false
	for k, verdict := range latest {
		sparts := strings.SplitN(k, ":", 2)
		if len(sparts) != 2 || sparts[0] != sha {
			continue
		}
		reviewer := sparts[1]
		if l.isCoordinator(reviewer) {
			continue
		}
		if verdict.Verdict != string(VerdictFAIL) && verdict.Verdict != string(VerdictBLOCKED) {
			continue
		}
		// Validate that the veto came from a known family.
		launchRow, hasLaunch := launch[k]
		if hasLaunch {
			lbf := launchRow.BuilderFamily
			if lbf != "" && FamilyAllowlist[lbf] {
				hasVeto = true
				break
			}
		}
	}
	if hasVeto {
		return false, fmt.Errorf("herd-review-ledger: refuse sha=%s reason=review veto (FAIL or BLOCKED)", sha)
	}

	hasPass := false
	familyMismatch := false
	for k, verdict := range latest {
		sparts := strings.SplitN(k, ":", 2)
		if len(sparts) != 2 || sparts[0] != sha {
			continue
		}
		reviewer := sparts[1]
		if l.isCoordinator(reviewer) {
			continue
		}

		launchRow, hasLaunch := launch[k]
		gate := "independent"
		var lbf, lrf, vrf, vbf string
		if hasLaunch {
			if launchRow.Gate != "" {
				gate = launchRow.Gate
			}
			lbf = launchRow.BuilderFamily
			lrf = launchRow.ReviewerFamily
		}
		vrf = verdict.ReviewerFamily
		vbf = verdict.BuilderFamily

		fs := resolveFamily(lbf, lrf, vbf, vrf)
		rf := fs.Value

		if verdict.Verdict != string(VerdictPASS) {
			continue
		}

		if gate == "mechanical" && (reviewer == "mechanical" || rf == "mechanical") {
			hasPass = true
			continue
		}

		if lbf == "" || !FamilyAllowlist[lbf] {
			continue
		}

		// When builderFamily is empty, only cross-family PASS counts.
		if builderFamily == "" {
			if rf != "" && rf != lbf {
				hasPass = true
				continue
			}
			familyMismatch = true
			continue
		}

		if rf != "" && rf != builderFamily {
			hasPass = true
			continue
		}
		familyMismatch = true
	}

	if !hasPass {
		for k, verdict := range latest {
			sparts := strings.SplitN(k, ":", 2)
			if len(sparts) != 2 || sparts[0] != sha {
				continue
			}
			reviewer := sparts[1]
			if l.isCoordinator(reviewer) {
				onlyCoord := true
				for k2 := range latest {
					sp := strings.SplitN(k2, ":", 2)
					if len(sp) == 2 && sp[0] == sha && !l.isCoordinator(sp[1]) {
						onlyCoord = false
						break
					}
				}
				if onlyCoord && verdict.Verdict == string(VerdictPASS) {
					return false, RejectCoordinatorSelfVerdict(sha, reviewer)
				}
			}
		}
		if familyMismatch {
			return false, fmt.Errorf("herd-review-ledger: refuse sha=%s reason=family mismatch", sha)
		}
		return false, fmt.Errorf("herd-review-ledger: refuse sha=%s reason=no qualifying PASS", sha)
	}

	return true, nil
}

func (l *Ledger) isCoordinator(name string) bool {
	_, ok := l.Coordinators[name]
	return ok
}

// isPassVerdictLatest checks whether the latest verdict set for a sha has
// any PASS and no FAIL/BLOCKED, using the family ladder.
func (l *Ledger) isPassVerdictLatest(sha string, latest map[string]LedgerRow, launch map[string]LedgerRow) bool {
	var hasPass bool
	for k, verdict := range latest {
		sparts := strings.SplitN(k, ":", 2)
		if len(sparts) != 2 || sparts[0] != sha {
			continue
		}
		reviewer := sparts[1]
		if l.isCoordinator(reviewer) {
			continue
		}
		launchRow, hasLaunch := launch[k]

		gate := "independent"
		var lbf, lrf, vrf, vbf string
		if hasLaunch {
			if launchRow.Gate != "" {
				gate = launchRow.Gate
			}
			lbf = launchRow.BuilderFamily
			lrf = launchRow.ReviewerFamily
		}
		vrf = verdict.ReviewerFamily
		vbf = verdict.BuilderFamily

		fs := resolveFamily(lbf, lrf, vbf, vrf)
		rf := fs.Value

		if gate == "mechanical" && (reviewer == "mechanical" || rf == "mechanical") {
			if verdict.Verdict == string(VerdictPASS) {
				hasPass = true
			}
			if verdict.Verdict == string(VerdictFAIL) || verdict.Verdict == string(VerdictBLOCKED) {
				return false
			}
			continue
		}

		if lbf == "" || !FamilyAllowlist[lbf] {
			continue
		}

		if verdict.Verdict == string(VerdictPASS) {
			hasPass = true
		}
		if verdict.Verdict == string(VerdictFAIL) || verdict.Verdict == string(VerdictBLOCKED) {
			return false
		}
	}
	return hasPass
}

// Queued returns PASS verdicts waiting for harvest (consumed excluded).
func (l *Ledger) Queued() ([]LedgerRow, error) {
	rows, err := l.AllRows()
	if err != nil {
		return nil, err
	}
	qrows, err := l.QueueRows()
	if err != nil {
		return nil, err
	}

	done := make(map[string]bool)
	for _, r := range qrows {
		if r.Event == string(EventConsumed) {
			done[r.SHA] = true
		}
	}

	launch := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventRecord) {
			k := r.SHA + ":" + r.Reviewer
			launch[k] = r
		}
	}

	latest := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventVerdict) {
			k := r.SHA + ":" + r.Reviewer
			latest[k] = r
		}
	}

	type qentry struct {
		row   LedgerRow
		order int
	}
	shaEnqueues := make(map[string]qentry)
	for i, r := range qrows {
		if r.Event == string(EventEnqueue) {
			shaEnqueues[r.SHA] = qentry{row: r, order: i}
		}
	}

	result := make([]LedgerRow, 0, len(shaEnqueues))
	for sha, eq := range shaEnqueues {
		if done[sha] {
			continue
		}
		if l.isPassVerdictLatest(sha, latest, launch) {
			result = append(result, eq.row)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Timestamp < result[j].Timestamp
	})
	return result, nil
}

// Pending returns launched records without a matching verdict.
func (l *Ledger) Pending() ([]LedgerRow, error) {
	rows, err := l.AllRows()
	if err != nil {
		return nil, err
	}

	verdictIdx := make(map[string]int)
	for i, r := range rows {
		if r.Event == string(EventVerdict) {
			k := r.SHA + ":" + r.Reviewer
			verdictIdx[k] = i
		}
	}

	type recEntry struct {
		row   LedgerRow
		index int
	}
	newestRec := make(map[string]recEntry)
	for i, r := range rows {
		if r.Event == string(EventRecord) {
			k := r.SHA + ":" + r.Reviewer
			newestRec[k] = recEntry{row: r, index: i}
		}
	}

	var pending []LedgerRow
	for k, rec := range newestRec {
		vi, hasVerdict := verdictIdx[k]
		if !hasVerdict || vi < rec.index {
			pending = append(pending, rec.row)
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		return pending[i].Timestamp < pending[j].Timestamp
	})
	return pending, nil
}

// PassSHAs returns distinct SHAs with PASS and no FAIL/BLOCKED.
func (l *Ledger) PassSHAs() ([]string, error) {
	rows, err := l.AllRows()
	if err != nil {
		return nil, err
	}

	launch := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventRecord) {
			k := r.SHA + ":" + r.Reviewer
			launch[k] = r
		}
	}

	latest := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventVerdict) {
			k := r.SHA + ":" + r.Reviewer
			latest[k] = r
		}
	}

	shaVerdicts := make(map[string][]LedgerRow)
	for k, v := range latest {
		sparts := strings.SplitN(k, ":", 2)
		if len(sparts) == 2 {
			shaVerdicts[sparts[0]] = append(shaVerdicts[sparts[0]], v)
		}
	}

	var shas []string
	for sha, vset := range shaVerdicts {
		hasPass := false
		hasVeto := false
		hasIndependent := false
		for _, verdict := range vset {
			reviewer := verdict.Reviewer
			if l.isCoordinator(reviewer) {
				continue
			}
			lk := sha + ":" + reviewer
			lr, hasLaunch := launch[lk]
			if !hasLaunch {
				continue
			}
			lbf := lr.BuilderFamily
			if lbf == "" || !FamilyAllowlist[lbf] {
				continue
			}

			gate := lr.Gate
			if gate == "" {
				gate = "independent"
			}
			lrf := lr.ReviewerFamily

			hasIndependent = true

			if gate == "mechanical" && (reviewer == "mechanical" || lrf == "mechanical") {
				if verdict.Verdict == string(VerdictPASS) {
					hasPass = true
				}
				continue
			}

			if verdict.Verdict == string(VerdictPASS) {
				hasPass = true
			}
			if verdict.Verdict == string(VerdictFAIL) || verdict.Verdict == string(VerdictBLOCKED) {
				hasVeto = true
			}
		}
		if hasIndependent && hasPass && !hasVeto {
			shas = append(shas, sha)
		}
	}

	sort.Slice(shas, func(i, j int) bool {
		return shas[i] > shas[j]
	})
	return shas, nil
}

// VetoSHAs returns distinct SHAs with any FAIL/BLOCKED in latest verdicts.
func (l *Ledger) VetoSHAs() ([]string, error) {
	rows, err := l.AllRows()
	if err != nil {
		return nil, err
	}

	launch := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventRecord) {
			k := r.SHA + ":" + r.Reviewer
			launch[k] = r
		}
	}

	latest := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventVerdict) {
			k := r.SHA + ":" + r.Reviewer
			latest[k] = r
		}
	}

	shaVerdicts := make(map[string][]LedgerRow)
	for k, v := range latest {
		sparts := strings.SplitN(k, ":", 2)
		if len(sparts) == 2 {
			shaVerdicts[sparts[0]] = append(shaVerdicts[sparts[0]], v)
		}
	}

	var shas []string
	for sha, vset := range shaVerdicts {
		hasVeto := false
		for _, verdict := range vset {
			reviewer := verdict.Reviewer
			if l.isCoordinator(reviewer) {
				continue
			}
			lk := sha + ":" + reviewer
			lr, hasLaunch := launch[lk]
			if !hasLaunch {
				continue
			}
			lbf := lr.BuilderFamily
			if lbf == "" || !FamilyAllowlist[lbf] {
				continue
			}
			if verdict.Verdict == string(VerdictFAIL) || verdict.Verdict == string(VerdictBLOCKED) {
				hasVeto = true
			}
		}
		if hasVeto {
			shas = append(shas, sha)
		}
	}

	sort.Slice(shas, func(i, j int) bool {
		return shas[i] > shas[j]
	})
	return shas, nil
}
