package reviewledger

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
)

// LaunchProvenance is the authenticated launch-receipt subset host-labelled
// ingest may consume. Host, session, family, and branch must come from this
// proof. A caller-supplied host or family string is not authentication.
type LaunchProvenance struct {
	CandidateSHA  string
	Host          string
	Session       string
	BuilderFamily string
	Branch        string
	CreatedAt     time.Time
	Accepted      bool
	// Member is set only after the proof is an accepted row in the
	// canonical launch log. Caller JSON and package-local structs are not
	// membership; CLI must never set this without a log match.
	Member bool
}

// HostIngestOpts is the coordinator API for append-only authenticated identity
// and builder-provenance reconciliation. It never rewrites historical verdict
// rows, never invents digests or families, and never treats a test-delta
// artifact as a whole-range review of the production parent.
type HostIngestOpts struct {
	SHA            string
	Reviewer       string
	Task           string
	Branch         string
	Artifact       string
	ArtifactDigest string
	Verdict        Verdict
	ReviewerFamily string
	BuilderFamily  string
	VfyDigest      string
	ReadBase       string
	ReadHead       string
	ProductionBase string
	CommitTime     time.Time
	Reaches        func(branch, sha string) bool
	Receipt        LaunchProvenance
}

// HostFromLaunchProof returns the host/session identity authenticated by a
// launch receipt. Filename, git branch, and operator flags are not proof.
func HostFromLaunchProof(processIdentity, herdrSession, paneID, cwd, worktree string) string {
	for _, v := range []string{processIdentity, herdrSession, paneID, cwd, worktree} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func hostKey(host string) string {
	return strings.TrimSpace(host)
}

// ProjectionKey is the shared identity for host-labelled ingest and every
// readiness/eligibility/queue consumer. HostIngest appends distinct
// (SHA, reviewer, authenticated host) rows; collapsing to SHA:reviewer
// lets a later host-B PASS hide a host-A FAIL/BLOCKED.
type ProjectionKey struct {
	SHA, Reviewer, Host string
}

// ProjectionOf is the canonical projection identity. Empty host is a
// distinct legacy projection, not a wildcard across hosts.
func ProjectionOf(sha, reviewer, host string) ProjectionKey {
	return ProjectionKey{SHA: sha, Reviewer: reviewer, Host: hostKey(host)}
}

func rowProjection(r LedgerRow) ProjectionKey {
	return ProjectionOf(r.SHA, r.Reviewer, r.Host)
}

func retrySupersessionFromLatest(latest map[ProjectionKey]LedgerRow, sha string) map[ProjectionKey]bool {
	out := make(map[ProjectionKey]bool)
	for k, verdict := range latest {
		if sha != "" && k.SHA != sha {
			continue
		}
		if verdict.Verdict != string(VerdictPASS) {
			continue
		}
		if retry := strings.TrimSpace(verdict.RetryOf); retry != "" {
			out[ProjectionOf(k.SHA, retry, k.Host)] = true
		}
	}
	return out
}

func retrySupersessionFromRows(rows []LedgerRow, sha string) map[ProjectionKey]bool {
	out := make(map[ProjectionKey]bool)
	for _, r := range rows {
		if r.SHA != sha || r.Verdict != string(VerdictPASS) {
			continue
		}
		if retry := strings.TrimSpace(r.RetryOf); retry != "" {
			out[ProjectionOf(r.SHA, retry, r.Host)] = true
		}
	}
	return out
}

// boundRetryAudit is the native PASS RetryOf EventSupersession: SHA is the
// live candidate, RetryOf names the prior reviewer, Host is the retry host.
// Candidate-identity replacement leaves RetryOf empty and stores PreviousSHA in Task.
func boundRetryAudit(event, retryOf string) bool {
	return event == string(EventSupersession) && strings.TrimSpace(retryOf) != ""
}

// IdentityReplacementSHA is the previous candidate withdrawn by a replacement
// EventSupersession. Empty for bound retry audit rows.
func IdentityReplacementSHA(event, sha, task, retryOf string) string {
	if event != string(EventSupersession) || boundRetryAudit(event, retryOf) {
		return ""
	}
	prev := strings.TrimSpace(task)
	if prev == "" || prev == strings.TrimSpace(sha) {
		return ""
	}
	return prev
}

func indexProjectionEvent(rows []LedgerRow, event string, skipRetired bool) map[ProjectionKey]LedgerRow {
	out := make(map[ProjectionKey]LedgerRow)
	for _, r := range rows {
		if skipRetired && r.Event == string(EventRetired) {
			continue
		}
		if r.Event == event {
			out[rowProjection(r)] = r
		}
	}
	return out
}

func sameHost(a, b string) bool {
	return hostKey(a) == hostKey(b)
}

func unknownBuilderFamilyError(family string) error {
	return fmt.Errorf("unknown builder family %q (refusing unprovable review provenance)", family)
}

func candidUnrecordedFamily(raw string) bool {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" || v == FamilyUnrecorded {
		return true
	}
	for _, sentinel := range []string{"unknown", "unrecorded", "unspecified", "unproven", "none", "n/a"} {
		if v == sentinel {
			return true
		}
	}
	return false
}

func authenticateLaunchProvenance(p LaunchProvenance, sha string, commitTime time.Time, reaches func(branch, sha string) bool) (host, builderFamily string, err error) {
	if !p.Member {
		return "", "", fmt.Errorf("%s", launch.CanonicalMemberRequired)
	}
	if !p.Accepted {
		return "", "", fmt.Errorf("launch receipt is not accepted")
	}
	if cand := strings.TrimSpace(p.CandidateSHA); cand != "" && cand != strings.TrimSpace(sha) {
		return "", "", fmt.Errorf("SHA/reviewer mismatch: launch receipt candidate %q does not match %s", p.CandidateSHA, sha)
	}
	host = hostKey(p.Host)
	if host == "" {
		host = hostKey(p.Session)
	}
	if host == "" {
		return "", "", fmt.Errorf("launch receipt does not authenticate a host or session")
	}
	builderFamily = strings.TrimSpace(p.BuilderFamily)
	if builderFamily == "" || builderFamily == FamilyUnrecorded || !FamilyAllowlist[builderFamily] {
		return "", "", fmt.Errorf("launch receipt builder family %q is not authenticated; refusing invented or unrecorded family", p.BuilderFamily)
	}
	branch := strings.TrimSpace(p.Branch)
	if branch == "" {
		return "", "", fmt.Errorf("launch receipt does not authenticate a reaching branch")
	}
	if p.CreatedAt.IsZero() || commitTime.IsZero() {
		return "", "", fmt.Errorf("launch receipt cannot prove it predates the candidate")
	}
	if p.CreatedAt.After(commitTime) {
		return "", "", fmt.Errorf("launch receipt at %s does not predate candidate %s", p.CreatedAt.UTC().Format(time.RFC3339), commitTime.UTC().Format(time.RFC3339))
	}
	if reaches == nil || !reaches(branch, sha) {
		return "", "", fmt.Errorf("launch receipt branch %q does not reach candidate %s", branch, sha)
	}
	return host, builderFamily, nil
}

func reconcileBuilderFamily(stated, receiptFamily string) (string, error) {
	if candidUnrecordedFamily(stated) {
		return receiptFamily, nil
	}
	if !FamilyAllowlist[strings.TrimSpace(stated)] {
		return "", unknownBuilderFamilyError(stated)
	}
	if !strings.EqualFold(strings.TrimSpace(stated), receiptFamily) {
		return "", fmt.Errorf("builder-family %q contradicts launch provenance %q; refusing to launder the disagreement", stated, receiptFamily)
	}
	return receiptFamily, nil
}

func refuseTestDelta(opts HostIngestOpts, sha string) error {
	if head := strings.TrimSpace(opts.ReadHead); head != "" && head != sha {
		return fmt.Errorf("reviewed-head %s is not candidate %s", head, sha)
	}
	base := strings.TrimSpace(opts.ProductionBase)
	read := strings.TrimSpace(opts.ReadBase)
	if base == "" {
		return fmt.Errorf("production parent base is required; a test-delta range must not be claimed as whole-range review")
	}
	if read == "" || read != base {
		return fmt.Errorf("artifact reviewed-base %q is not the production parent %s; refusing to launder a test-delta as whole-range review", read, base)
	}
	return nil
}

// HostIngest appends provenance and queue events for an existing SHA/reviewer
// using an authenticated launch receipt. Historical verdicts are preserved.
func (l *Ledger) HostIngest(opts HostIngestOpts) (enqueued bool, err error) {
	sha := strings.TrimSpace(opts.SHA)
	reviewer := strings.TrimSpace(opts.Reviewer)
	if sha == "" || reviewer == "" {
		return false, fmt.Errorf("host ingest requires an exact candidate SHA and existing reviewer")
	}
	artifactIngest := strings.TrimSpace(opts.ArtifactDigest) != "" || opts.Verdict != ""
	host, receiptFamily, err := authenticateLaunchProvenance(opts.Receipt, sha, opts.CommitTime, opts.Reaches)
	if err != nil {
		return false, err
	}
	if artifactIngest {
		if err := RequireCloseableCardRef(opts.Task, "host ingest task"); err != nil {
			return false, err
		}
		if err := refuseTestDelta(opts, sha); err != nil {
			return false, err
		}
	}
	builderFamily := receiptFamily
	if artifactIngest {
		if err := ValidVerdict(opts.Verdict); err != nil {
			return false, err
		}
		if strings.TrimSpace(opts.ArtifactDigest) == "" {
			return false, fmt.Errorf("host ingest requires the artifact digest; refusing invented evidence")
		}
		builderFamily, err = reconcileBuilderFamily(opts.BuilderFamily, receiptFamily)
		if err != nil {
			return false, err
		}
		if !FamilyAllowlist[strings.TrimSpace(opts.ReviewerFamily)] {
			return false, fmt.Errorf("host ingest refuses unknown reviewer family %q", opts.ReviewerFamily)
		}
		if strings.EqualFold(strings.TrimSpace(opts.ReviewerFamily), builderFamily) {
			return false, fmt.Errorf("host ingest requires independent authenticated families")
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	release, lockErr := lockVerdictMutation(l.Path)
	if lockErr != nil {
		return false, lockErr
	}
	defer func() { err = errors.Join(err, release()) }()

	before, readErr := os.ReadFile(l.Path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, readErr
	}
	rows, err := readRows(l.Path)
	if err != nil {
		return false, err
	}

	var prior *LedgerRow
	var priorSameHost *LedgerRow
	for i := range rows {
		r := rows[i]
		if r.Event != string(EventVerdict) || r.SHA != sha || r.Reviewer != reviewer {
			continue
		}
		copy := r
		prior = &copy
		if sameHost(r.Host, host) {
			priorSameHost = &copy
		}
	}
	if prior == nil {
		return false, fmt.Errorf("SHA/reviewer mismatch: existing reviewer identity %q not found for %s", reviewer, sha)
	}
	if strings.TrimSpace(opts.Task) == "" {
		opts.Task = prior.Task
	}
	if err := RequireCloseableCardRef(opts.Task, "host ingest task"); err != nil {
		return false, err
	}

	if !artifactIngest {
		if err := l.appendAuthenticatedProvenance(opts, host, receiptFamily, *prior); err != nil {
			return false, err
		}
		return l.preservePrefix(before)
	}

	if priorSameHost != nil {
		if priorSameHost.ArtifactDigest == opts.ArtifactDigest &&
			priorSameHost.ReviewerFamily == opts.ReviewerFamily &&
			priorSameHost.BuilderFamily == builderFamily &&
			priorSameHost.Verdict == string(opts.Verdict) {
			return false, nil
		}
		// Dedup remains SHA+reviewer+authenticated-host, never digest or family
		// alone. Same host with a different artifact is FAC-493 reassessment.
		return false, fmt.Errorf("duplicate same-reviewer reassessment requires FAC-493 reassesses binding; host-labelled ingest is for a distinct authenticated host")
	}

	recordOpts := RecordOpts{
		SHA: sha, Branch: firstNonEmpty(opts.Receipt.Branch, opts.Branch), Reviewer: reviewer, Task: opts.Task,
		Host: host, BuilderFamily: builderFamily, ReviewerFamily: opts.ReviewerFamily,
		Artifact: opts.Artifact, Gate: "independent", Pane: opts.Receipt.Session,
	}
	if err := l.ensureRecord(recordOpts); err != nil {
		return false, err
	}
	enqueued, err = l.verdict(VerdictOpts{
		SHA: sha, Reviewer: reviewer, Task: opts.Task, Branch: firstNonEmpty(opts.Branch, opts.Receipt.Branch),
		Host: host, Verdict: opts.Verdict, Artifact: opts.Artifact,
		ArtifactDigest: opts.ArtifactDigest, ReviewerFamily: opts.ReviewerFamily,
		BuilderFamily: builderFamily, VfyDigest: opts.VfyDigest, CandidateSHA: sha,
	})
	if err != nil {
		return false, err
	}
	_, err = l.preservePrefix(before)
	return enqueued, err
}

func (l *Ledger) appendAuthenticatedProvenance(opts HostIngestOpts, host, builderFamily string, prior LedgerRow) error {
	recordOpts := RecordOpts{
		SHA: opts.SHA, Branch: firstNonEmpty(opts.Receipt.Branch, opts.Branch, prior.Branch),
		Reviewer: opts.Reviewer, Task: firstNonEmpty(opts.Task, prior.Task),
		Host: host, BuilderFamily: builderFamily, ReviewerFamily: prior.ReviewerFamily,
		Artifact: prior.Artifact, Gate: "independent", Pane: opts.Receipt.Session,
	}
	if err := l.ensureRecord(recordOpts); err != nil {
		return err
	}
	if prior.Verdict != string(VerdictPASS) {
		return nil
	}
	return l.appendRow(l.QueuePath, &LedgerRow{
		Event: string(EventEnqueue), SHA: opts.SHA, Reviewer: opts.Reviewer,
		Branch: recordOpts.Branch, Host: host, Status: "queued",
	})
}

func (l *Ledger) preservePrefix(before []byte) (bool, error) {
	if len(before) == 0 {
		return false, nil
	}
	after, err := os.ReadFile(l.Path)
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(string(after), string(before)) {
		return false, fmt.Errorf("host ingest rewrote historical ledger bytes")
	}
	return false, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
