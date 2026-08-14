package mergeadmit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/remoteci"
	"github.com/Kampe/Herdforge/pkg/residual"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// Rejection codes. These are the durable, structured reasons a merge was
// refused — the value FAC-140 routes on. Callers switch on Code; Reason is
// prose for a human and carries no authority.
const (
	CodeMissingField    = "missing_field"
	CodeNotDeclared     = "merge_policy_undeclared"
	CodeProbeFailed     = "probe_failed"
	CodeBaseAdvanced    = "base_advanced"
	CodeHeadMoved       = "candidate_head_moved"
	CodeNotMergeable    = "not_mergeable"
	CodeRequiredCheck   = "required_check_not_green"
	CodeTaskRevision    = "task_revision_changed"
	CodeAcceptance      = "acceptance_digest_mismatch"
	CodeLedgerRefused   = "ledger_refused"
	CodeProofFailed     = "integration_proof_failed"
	CodeReceiptReadback = "receipt_readback_failed"
	CodeResidual        = "residual_linkage_missing"
	CodeRemoteCI        = "remote_ci_not_settled"
)

// ErrRestartAdmission marks the class of refusal where nothing is wrong with
// the candidate — the world moved underneath it. The caller must re-resolve
// current state and run admission again rather than retrying the merge with
// what it already holds. This is the serial integration train's stale-receipt
// signal (merging FAC-94 invalidated three otherwise-clean PASS receipts).
var ErrRestartAdmission = errors.New("admission is stale: live state advanced, re-run admission against current state")

// Request is the caller-asserted merge context. Every field is required.
// Admission does not default, infer, or look anything up from prose: a field
// the caller cannot supply is a claim it cannot make.
type Request struct {
	// Ref is the board ticket ref, e.g. "FAC-156".
	Ref string
	// TaskID is the provider task id the work is bound to. Refs are re-minted
	// across board rollbacks; the id is not.
	TaskID string
	// ProviderRevision is the board task revision the REVIEWER reviewed
	// against. Admission refuses if the live card has moved past it.
	ProviderRevision string
	// AcceptanceDigest binds the merge to that exact card revision's
	// acceptance criteria. It must recompute from the live revision.
	AcceptanceDigest string
	// CandidateSHA is the exact reviewed candidate tip.
	CandidateSHA string
	// BaseSHA is the base the candidate was reviewed against.
	BaseSHA string
	// Lease / LeaseGeneration are the claim generation the work holds.
	Lease           string
	LeaseGeneration int64
	// PatchURL is the patch identity bound into the reviewer's verdict.
	PatchURL string
	// AuthorFamily / AuthorIdentity are the builder's provenance, asserted so
	// the ledger can refuse a self-verdict under a relabelled family.
	AuthorFamily   string
	AuthorIdentity string
	// Mode is how the merge will be published, which selects the proof that
	// Complete will later demand.
	Mode Mode
	// Residuals are exact board-revision context. Admission requires linked
	// provider readback and refuses any still-required acceptance criterion.
	Residuals []residual.Record
	// RemoteCI is the exact candidate, policy, repository, and attempt-bound
	// settlement that policy requires before a local admission may proceed.
	RemoteCI *remoteci.Settlement
}

// Decision is the structured admission outcome. Callers gate SOLELY on
// Admitted. A non-nil Decision with Admitted=false is a policy refusal; a
// non-nil error alongside it carries the same refusal for `if err != nil`
// callers, so neither calling convention can accidentally read a refusal as
// consent.
type Decision struct {
	Admitted     bool              `json:"admitted"`
	Code         string            `json:"code,omitempty"`
	Reason       string            `json:"reason"`
	Ref          string            `json:"ref"`
	CandidateSHA string            `json:"candidate_sha"`
	BaseSHA      string            `json:"base_sha"`
	Reviewer     string            `json:"reviewer,omitempty"`
	ReviewerFam  string            `json:"reviewer_family,omitempty"`
	Tier         string            `json:"risk_tier,omitempty"`
	Mode         Mode              `json:"mode"`
	Checks       map[string]string `json:"checks,omitempty"`
	// VerificationDigest is the admitted verdict's test-gate digest, carried
	// forward so Complete binds the receipt to the digest that was admitted.
	VerificationDigest string `json:"verification_digest,omitempty"`
}

// Gate is the single compiled merge authority. Construct one per repository.
type Gate struct {
	RepoDir string
	// Ledger is the durable review-verdict ledger. Nil refuses everything:
	// with no ledger there is no verdict, and no verdict is not a PASS.
	Ledger *reviewledger.Ledger
	// Live supplies the state re-read immediately before merging.
	Live LiveState
	// Policy is the repository's declared merge contract. Load it with
	// preflight.LoadMergePolicy; the zero value is refused.
	Policy preflight.MergePolicy
	// RemoteCIPolicyRevision is the compiled revision of the admission policy.
	// When non-empty it requires a passed remote settlement bound to this repo.
	RemoteCIRepository     string
	RemoteCIPolicyRevision string
}

// ComputeAcceptanceDigest binds a merge to one exact board card revision. A
// verdict minted against an older revision of the card produces a digest that
// no longer recomputes, so a package-local PASS cannot survive an acceptance
// criteria edit — which is the FAC-150 failure (a provider-only PASS at
// 3786c0f that did not satisfy the card's production-wiring criterion).
func ComputeAcceptanceDigest(ref, taskID, providerRevision string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		strings.ToUpper(strings.TrimSpace(ref)),
		strings.TrimSpace(taskID),
		strings.TrimSpace(providerRevision),
	}, "\x00")))
	return hex.EncodeToString(h[:])
}

// TaskContentRevision derives a revision token from the board card content a
// reviewer actually reads. The provider exposes no revision counter, so the
// content IS the revision: edit the acceptance criteria and the token changes.
//
// That is what makes the acceptance binding bite. In the FAC-150 incident a
// provider-only PASS was internally valid but did not satisfy the card's
// production-wiring criterion. With this token, editing or tightening a card's
// criteria invalidates every verdict minted against the old text, and the work
// has to be re-reviewed against what the card says now.
func TaskContentRevision(ref, title, description string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		strings.ToUpper(strings.TrimSpace(ref)),
		strings.TrimSpace(title),
		strings.TrimSpace(description),
	}, "\x00")))
	return hex.EncodeToString(h[:])
}

func (g *Gate) refuse(req Request, code, format string, a ...any) (*Decision, error) {
	reason := fmt.Sprintf(format, a...)
	d := &Decision{
		Admitted: false, Code: code, Reason: reason, Ref: req.Ref,
		CandidateSHA: req.CandidateSHA, BaseSHA: req.BaseSHA, Mode: req.Mode,
	}
	err := fmt.Errorf("herd-merge-admission: refuse ref=%s sha=%s code=%s reason=%q",
		req.Ref, short(req.CandidateSHA), code, reason)
	if code == CodeBaseAdvanced || code == CodeHeadMoved || code == CodeTaskRevision {
		err = fmt.Errorf("%w: %v", ErrRestartAdmission, err)
	}
	return d, err
}

// Admit is the ONLY function in this repository permitted to authorize a
// merge. It fails closed on the first thing it cannot prove.
//
// Order matters: cheap structural checks first, then live re-reads, then the
// ledger. The live re-reads come BEFORE the ledger deliberately — a stale
// candidate must be reported as stale rather than as "no verdict", because
// those two refusals call for different recovery.
func (g *Gate) Admit(req Request) (*Decision, error) {
	if g == nil {
		return nil, fmt.Errorf("herd-merge-admission: no gate configured")
	}
	if err := residual.ValidateExit(req.Residuals, req.ProviderRevision); err != nil {
		return g.refuse(req, CodeResidual, "residual gate refused merge: %v", err)
	}
	for _, f := range []struct{ name, val string }{
		{"ref", req.Ref},
		{"task_id", req.TaskID},
		{"provider_revision", req.ProviderRevision},
		{"acceptance_digest", req.AcceptanceDigest},
		{"candidate_sha", req.CandidateSHA},
		{"base_sha", req.BaseSHA},
		{"lease", req.Lease},
		{"patch_url", req.PatchURL},
		{"author_family", req.AuthorFamily},
		{"author_identity", req.AuthorIdentity},
	} {
		if strings.TrimSpace(f.val) == "" {
			return g.refuse(req, CodeMissingField, "%s is required; a field the caller cannot supply is a claim it cannot make", f.name)
		}
	}
	if req.LeaseGeneration <= 0 {
		return g.refuse(req, CodeMissingField, "lease_generation is required and must be positive")
	}
	mode, err := ParseMode(string(req.Mode))
	if err != nil {
		return g.refuse(req, CodeMissingField, "%v", err)
	}
	req.Mode = mode

	if g.Ledger == nil {
		return g.refuse(req, CodeMissingField, "no review ledger configured; with no ledger there is no verdict, and no verdict is not a PASS")
	}

	// The repository must DECLARE its merge contract before anything may be
	// merged autonomously under it (FAC-135).
	if rep := preflight.CheckMergePolicy(g.Policy); !rep.OK {
		return g.refuse(req, CodeNotDeclared, "autonomous merge refused: %s", strings.Join(rep.Reasons, "; "))
	}
	if g.RemoteCIPolicyRevision != "" {
		if req.RemoteCI == nil {
			return g.refuse(req, CodeRemoteCI, "remote CI settlement is required by merge policy")
		}
		if err := req.RemoteCI.Validate(); err != nil {
			return g.refuse(req, CodeRemoteCI, "remote CI settlement is invalid: %v", err)
		}
		if req.RemoteCI.State != remoteci.StatePassed {
			return g.refuse(req, CodeRemoteCI, "remote CI state %q is not a passing terminal settlement", req.RemoteCI.State)
		}
		b := req.RemoteCI.Binding
		if b.Repository != g.RemoteCIRepository || b.PolicyRevision != g.RemoteCIPolicyRevision || b.CandidateSHA != req.CandidateSHA {
			return g.refuse(req, CodeRemoteCI, "remote CI settlement is not bound to this repository, policy, and exact candidate")
		}
	}

	// --- live re-read, immediately before merge ---

	base, err := g.Live.OriginMain.Read("origin_main")
	if err != nil {
		return g.refuse(req, CodeProbeFailed, "%v", err)
	}
	if !sameSHA(base, req.BaseSHA) {
		return g.refuse(req, CodeBaseAdvanced,
			"integration base advanced from %s to %s since this candidate was admitted; every queued receipt cut against the old base is stale",
			short(req.BaseSHA), short(base))
	}

	head, err := g.Live.CandidateHead.Read("candidate_head")
	if err != nil {
		return g.refuse(req, CodeProbeFailed, "%v", err)
	}
	if !sameSHA(head, req.CandidateSHA) {
		return g.refuse(req, CodeHeadMoved,
			"candidate head moved from %s to %s; a verdict is bound to the exact sha it reviewed and does not transfer",
			short(req.CandidateSHA), short(head))
	}

	mergeable, err := g.Live.Mergeable.Read("mergeable")
	if err != nil {
		return g.refuse(req, CodeProbeFailed, "%v", err)
	}
	if !strings.EqualFold(mergeable, "clean") && !strings.EqualFold(mergeable, "mergeable") {
		return g.refuse(req, CodeNotMergeable, "provider reports mergeability %q, which is not clean", mergeable)
	}

	checks, err := readChecks(g.Live.Checks, g.Policy.RequiredChecks)
	if err != nil {
		return g.refuse(req, CodeRequiredCheck, "%v", err)
	}

	liveRevision, err := g.Live.TaskRevision.Read("task_revision")
	if err != nil {
		return g.refuse(req, CodeProbeFailed, "%v", err)
	}
	if liveRevision != strings.TrimSpace(req.ProviderRevision) {
		return g.refuse(req, CodeTaskRevision,
			"board card revision moved from %s to %s since review; the acceptance criteria that were reviewed are not the ones on the card now",
			short(req.ProviderRevision), short(liveRevision))
	}

	// Recompute from the LIVE revision. The equality check above has already
	// forced liveRevision == req.ProviderRevision, so the two sources agree by
	// this point — the live value is used anyway so that reordering or
	// removing that check cannot silently leave this one reading the caller's
	// own assertion back to itself.
	//
	// What this check earns ON TOP of the revision check is the rest of the
	// binding: a digest computed over a different ref or a different task id,
	// or one that was simply typed in, does not recompute. Both checks are
	// mutation-verified as individually load-bearing.
	want := ComputeAcceptanceDigest(req.Ref, req.TaskID, liveRevision)
	if req.AcceptanceDigest != want {
		return g.refuse(req, CodeAcceptance,
			"acceptance digest %s does not bind this card at revision %s (expected %s); a package-local PASS that omits the card's acceptance criteria is not merge-admissible",
			short(req.AcceptanceDigest), short(liveRevision), short(want))
	}

	// --- durable verdict: exact SHA, supersession, provenance ---

	result, ledgerErr := g.Ledger.Admit(reviewledger.AdmissionOpts{
		CandidateSHA:   req.CandidateSHA,
		Task:           req.Ref,
		Lease:          req.Lease,
		PatchURL:       req.PatchURL,
		AuthorFamily:   req.AuthorFamily,
		AuthorIdentity: req.AuthorIdentity,
	})
	// Gate on Admitted only. A nil result with an error is an I/O failure and
	// is just as much a refusal as a policy rejection.
	if result == nil || !result.Admitted {
		reason := "review ledger refused this candidate"
		if result != nil && result.Reason != "" {
			reason = result.Reason
		} else if ledgerErr != nil {
			reason = ledgerErr.Error()
		}
		return g.refuse(req, CodeLedgerRefused, "%s", reason)
	}

	return &Decision{
		Admitted:     true,
		Reason:       fmt.Sprintf("exact-sha verdict admitted: %s", result.Reason),
		Ref:          req.Ref,
		CandidateSHA: req.CandidateSHA,
		BaseSHA:      req.BaseSHA,
		Reviewer:     result.Reviewer,
		ReviewerFam:  result.ReviewerFamily,
		Tier:         result.Tier,
		Mode:         req.Mode,
		Checks:       checks,

		VerificationDigest: result.VerificationDigest,
	}, nil
}

// sameSHA compares two object ids allowing either side to be abbreviated, but
// never treating an empty value as a match.
func sameSHA(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	// A prefix shorter than git's minimum unique length is not an identity
	// claim; refuse rather than match loosely.
	if len(a) < 7 {
		return false
	}
	return strings.HasPrefix(b, a)
}
