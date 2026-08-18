package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/outbox"
	"github.com/Kampe/Herdforge/pkg/residual"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

// FAC-144: production completion → review admission.
//
// CheckCompletion is a diagnostic only. Review enqueue authority is a
// persisted exact-SHA receipt from VerifyAndPersist plus
// ReceiptAdmission.RequireCurrentPassing for the same binding. Stale,
// missing, FAIL, BLOCKED, or mismatched evidence never enters review.

var (
	// ErrVerificationFailed means the candidate produced a FAIL receipt
	// and must return to repair (re-nudge), never review.
	ErrVerificationFailed = errors.New("completion: verification FAIL")
	// ErrVerificationBlocked means the candidate produced a BLOCKED
	// receipt (dirty tree, tool failure, cancel) and must land in an
	// explicit blocked/recovering state, never review.
	ErrVerificationBlocked = errors.New("completion: verification BLOCKED")
	// ErrReceiptRejected means RequireCurrentPassing or binding checks
	// refused the digest for review spawn.
	ErrReceiptRejected = errors.New("completion: receipt rejected for review")
	// ErrBindingMismatch means the live lease/task/policy binding no
	// longer matches the receipt that was persisted.
	ErrBindingMismatch = errors.New("completion: receipt binding mismatch")
)

// CompletionBinding is the live identity a completion callback claims.
// Every field that is set on the receipt must still match when review
// is spawned; changes invalidate prior admission.
type CompletionBinding struct {
	TaskRef             string
	Repo                string
	LeaseOwner          string
	LeaseGeneration     int64
	ProviderRevision    string
	CandidateSHA        string
	BaseSHA             string
	PatchID             string
	ClassifierVersion   string
	VerificationProfile string // stable identity of the verification argv/profile
	ProfileDigest       string // digest of the configured command profile
	ConfigRevision      string // digest of the repository configuration
	Branch              string
	// WorktreeDir is the candidate checkout. It is never copied into
	// operator-facing rejection reasons.
	WorktreeDir string
}

// CompletionDecision is the durable result of handling one completion.
type CompletionDecision struct {
	Outcome  verifier.Outcome
	Digest   string
	Receipt  *verifier.Receipt
	Reason   string // path-safe rejection / status reason
	Replayed bool   // lifecycle/outbox replay; verification was not re-run
	// ReviewReady is true only when Outcome is PASS and the digest was
	// admitted and recorded for review enqueue.
	ReviewReady bool
	// Residuals are the provider-read-back scope-reduction context. A required
	// criterion can never appear here as a successful completion substitute.
	Residuals []residual.Record
}

// HandleScopeReduced is the only nonterminal scope-reduction exit. It creates
// (or deterministically reuses) exactly one provider-linked follow-up per
// record, verifies provider readback, and then refuses required criteria.
// Callers persist/project the returned records into task/review/merge context.
func (g *CompletionGate) HandleScopeReduced(ctx context.Context, p residual.Provider, acceptanceRevision string, records []residual.Record) ([]residual.Record, error) {
	if g == nil {
		return nil, errors.New("completion: nil gate")
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("%w: scope-reduced outcome has no residual record", residual.ErrMissingLinkage)
	}
	linked := make([]residual.Record, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		if _, ok := seen[record.ID]; ok {
			return nil, fmt.Errorf("%w: duplicate residual %s", residual.ErrMissingLinkage, record.ID)
		}
		seen[record.ID] = struct{}{}
		bound, err := residual.EnsureFollowUp(ctx, p, record)
		if err != nil {
			return nil, err
		}
		linked = append(linked, bound)
	}
	if err := residual.ValidateExit(linked, acceptanceRevision); err != nil {
		return nil, err
	}
	return linked, nil
}

// reviewEnqueuePayload is the outbox payload for a review-spawn intent.
type reviewEnqueuePayload struct {
	TaskRef          string `json:"task_ref"`
	CandidateSHA     string `json:"candidate_sha"`
	ReceiptDigest    string `json:"receipt_digest"`
	LeaseGeneration  int64  `json:"lease_generation"`
	ProviderRevision string `json:"provider_revision,omitempty"`
	PatchID          string `json:"patch_id,omitempty"`
	Classifier       string `json:"classifier_version,omitempty"`
	Profile          string `json:"verification_profile,omitempty"`
	ProfileDigest    string `json:"profile_digest,omitempty"`
	ConfigRevision   string `json:"config_revision,omitempty"`
}

// CompletionGate is the production authority between builder completion
// and review spawn. It owns VerifyAndPersist, binding checks, lifecycle
// evidence, and the transactional review-enqueue outbox item.
type CompletionGate struct {
	Verifier  *verifier.Verifier
	Store     verifier.ReceiptStore
	Admission *verifier.ReceiptAdmission
	// Machine, when non-nil, records verification evidence and the review
	// enqueue intent in one lifecycle transaction.
	Machine *lifecycle.Machine
	// Actor is recorded on lifecycle events (default: "completion-gate").
	Actor string
}

// NewCompletionGate builds a gate over an existing verifier and a receipt
// store directory (repo-relative, e.g. .herd/verification-receipts).
func NewCompletionGate(v *verifier.Verifier, receiptDir string, machine *lifecycle.Machine) (*CompletionGate, error) {
	if v == nil {
		return nil, errors.New("completion gate: verifier is required")
	}
	store, err := verifier.NewFileReceiptStore(receiptDir)
	if err != nil {
		return nil, err
	}
	return &CompletionGate{
		Verifier:  v,
		Store:     store,
		Admission: verifier.NewReceiptAdmission(store),
		Machine:   machine,
		Actor:     "completion-gate",
	}, nil
}

// HandleCompletion is the production path: verify the exact candidate,
// persist every terminal receipt, and only on PASS record lifecycle
// evidence + a review-enqueue outbox item. FAIL and BLOCKED never
// enqueue review. Storage/tool/provider failures return a hard error
// (not a soft "unverified" that could be misread as renudge-only).
//
// Idempotency: a second call with the same binding and an already-PASS
// current receipt for the same candidate SHA reuses the digest and
// replays the lifecycle/outbox write without re-running the command.
func (g *CompletionGate) HandleCompletion(ctx context.Context, bind CompletionBinding) (*CompletionDecision, error) {
	if err := g.validate(); err != nil {
		return nil, err
	}
	if err := validateBinding(bind); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Idempotent resume: a prior PASS for this exact binding is still
	// current → re-admit and re-record without re-verifying.
	if d, ok, err := g.resumePassing(ctx, bind); err != nil {
		return nil, err
	} else if ok {
		return d, nil
	}

	req := verificationRequest(bind)
	receipt, err := g.Verifier.VerifyAndPersist(ctx, bind.WorktreeDir, req, g.Store)
	if err != nil {
		// Hard failure: tool/store/provider. Not a review candidate.
		return nil, fmt.Errorf("completion: verify-and-persist: %w", sanitizeErr(err, bind.WorktreeDir))
	}

	decision := &CompletionDecision{
		Outcome: receipt.Outcome,
		Digest:  receipt.Digest,
		Receipt: receipt,
	}

	switch receipt.Outcome {
	case verifier.OutcomePASS:
		if err := g.bindMatchesReceipt(bind, receipt); err != nil {
			decision.Reason = err.Error()
			return decision, fmt.Errorf("%w: %s", ErrBindingMismatch, decision.Reason)
		}
		if _, err := g.Admission.RequireCurrentPassing(ctx, bind.WorktreeDir, receipt.Digest); err != nil {
			decision.Reason = safeReason(err, bind.WorktreeDir)
			return decision, fmt.Errorf("%w: %s", ErrReceiptRejected, decision.Reason)
		}
		replayed, err := g.recordPassing(bind, receipt)
		if err != nil {
			return decision, err
		}
		decision.Replayed = replayed
		decision.ReviewReady = true
		decision.Reason = "PASS"
		return decision, nil

	case verifier.OutcomeFAIL:
		decision.Reason = "verification FAIL — repair required"
		if _, err := g.recordNonPass(bind, receipt, lifecycle.StateBuilding); err != nil {
			// Lifecycle write failure is hard; the FAIL receipt is already durable.
			return decision, err
		}
		return decision, fmt.Errorf("%w: digest %s", ErrVerificationFailed, receipt.Digest)

	case verifier.OutcomeBLOCKED:
		decision.Reason = "verification BLOCKED — explicit blocked/recovering"
		if _, err := g.recordNonPass(bind, receipt, lifecycle.StateBlocked); err != nil {
			return decision, err
		}
		return decision, fmt.Errorf("%w: digest %s", ErrVerificationBlocked, receipt.Digest)

	default:
		decision.Reason = fmt.Sprintf("unknown verification outcome %q", receipt.Outcome)
		return decision, fmt.Errorf("completion: %s", decision.Reason)
	}
}

// AdmitReview is the review-spawn gate. It loads the named digest,
// requires a current PASS via ReceiptAdmission, and re-checks the live
// binding. Callers must supply the digest they observed for this
// candidate — "latest for task" is not sufficient.
func (g *CompletionGate) AdmitReview(ctx context.Context, bind CompletionBinding, digest string) (*verifier.Receipt, error) {
	if err := g.validate(); err != nil {
		return nil, err
	}
	if err := validateBinding(bind); err != nil {
		return nil, err
	}
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return nil, fmt.Errorf("%w: missing receipt digest", ErrReceiptRejected)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	receipt, err := g.Admission.RequireCurrentPassing(ctx, bind.WorktreeDir, digest)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrReceiptRejected, safeReason(err, bind.WorktreeDir))
	}
	if err := g.bindMatchesReceipt(bind, receipt); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrBindingMismatch, err.Error())
	}
	return receipt, nil
}

// RequireCurrentPassing is the compile-safe hook reviewsup and forge
// review spawn call. dir is the candidate worktree; digest is the FAC-122
// receipt digest previously persisted for that candidate.
func (g *CompletionGate) RequireCurrentPassing(ctx context.Context, dir, digest string) (*verifier.Receipt, error) {
	if err := g.validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	receipt, err := g.Admission.RequireCurrentPassing(ctx, dir, digest)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrReceiptRejected, safeReason(err, dir))
	}
	return receipt, nil
}

func (g *CompletionGate) validate() error {
	if g == nil || g.Verifier == nil || g.Store == nil || g.Admission == nil {
		return errors.New("completion gate: verifier, store, and admission are required")
	}
	return nil
}

func validateBinding(b CompletionBinding) error {
	if strings.TrimSpace(b.TaskRef) == "" {
		return errors.New("completion: task_ref is required")
	}
	if strings.TrimSpace(b.CandidateSHA) == "" || len(b.CandidateSHA) != 40 {
		return errors.New("completion: candidate_sha must be the exact 40-character commit SHA")
	}
	if strings.TrimSpace(b.WorktreeDir) == "" {
		return errors.New("completion: worktree dir is required")
	}
	if b.LeaseGeneration <= 0 {
		return errors.New("completion: lease_generation must be positive")
	}
	return nil
}

func verificationRequest(b CompletionBinding) verifier.VerificationRequest {
	gen := strconv.FormatInt(b.LeaseGeneration, 10)
	artifacts := make([]string, 0, 4)
	if b.PatchID != "" {
		artifacts = append(artifacts, "patch:"+b.PatchID)
	}
	if b.ProviderRevision != "" {
		artifacts = append(artifacts, "provider_revision:"+b.ProviderRevision)
	}
	if b.ClassifierVersion != "" {
		artifacts = append(artifacts, "classifier:"+b.ClassifierVersion)
	}
	if b.VerificationProfile != "" {
		artifacts = append(artifacts, "profile:"+b.VerificationProfile)
	}
	if b.LeaseOwner != "" {
		artifacts = append(artifacts, "lease_owner:"+b.LeaseOwner)
	}
	return verifier.VerificationRequest{
		TaskRef:             b.TaskRef,
		LeaseGeneration:     gen,
		CandidateSHA:        b.CandidateSHA,
		BaseSHA:             b.BaseSHA,
		EnvironmentPolicy:   verifier.EnvironmentPolicyInherited,
		Artifacts:           artifacts,
		VerificationProfile: b.VerificationProfile,
		ProfileDigest:       b.ProfileDigest,
		ConfigRevision:      b.ConfigRevision,
	}
}

func (g *CompletionGate) bindMatchesReceipt(b CompletionBinding, r *verifier.Receipt) error {
	if r == nil {
		return errors.New("nil receipt")
	}
	if r.TaskRef != "" && r.TaskRef != b.TaskRef {
		return fmt.Errorf("task_ref receipt=%s live=%s", r.TaskRef, b.TaskRef)
	}
	if r.CandidateSHA != b.CandidateSHA {
		return fmt.Errorf("candidate_sha receipt=%s live=%s", shortSHA(r.CandidateSHA), shortSHA(b.CandidateSHA))
	}
	if b.BaseSHA != "" && r.BaseSHA != "" && r.BaseSHA != b.BaseSHA {
		return fmt.Errorf("base_sha receipt=%s live=%s", shortSHA(r.BaseSHA), shortSHA(b.BaseSHA))
	}
	wantGen := strconv.FormatInt(b.LeaseGeneration, 10)
	if r.LeaseGeneration != "" && r.LeaseGeneration != wantGen {
		return fmt.Errorf("lease_generation receipt=%s live=%s", r.LeaseGeneration, wantGen)
	}
	if b.VerificationProfile != "" && r.VerificationProfile != b.VerificationProfile {
		return fmt.Errorf("verification profile receipt=%s live=%s", r.VerificationProfile, b.VerificationProfile)
	}
	if b.ProfileDigest != "" && r.ProfileDigest != b.ProfileDigest {
		return errors.New("verification profile digest binding no longer matches receipt")
	}
	if b.ConfigRevision != "" && r.ConfigRevision != b.ConfigRevision {
		return errors.New("verification config revision binding no longer matches receipt")
	}
	// Artifact-bound fields: when the live binding supplies them, the
	// receipt must carry the same values (invalidation on change).
	for _, a := range []struct {
		prefix, live string
	}{
		{"patch:", b.PatchID},
		{"provider_revision:", b.ProviderRevision},
		{"classifier:", b.ClassifierVersion},
		{"profile:", b.VerificationProfile},
		{"lease_owner:", b.LeaseOwner},
	} {
		if a.live == "" {
			continue
		}
		if !artifactHas(r.Artifacts, a.prefix+a.live) {
			return fmt.Errorf("%s binding no longer matches receipt", strings.TrimSuffix(a.prefix, ":"))
		}
	}
	return nil
}

func artifactHas(arts []string, want string) bool {
	for _, a := range arts {
		if a == want {
			return true
		}
	}
	return false
}

func (g *CompletionGate) resumePassing(ctx context.Context, bind CompletionBinding) (*CompletionDecision, bool, error) {
	// Look for a lifecycle event that already recorded a PASS digest for
	// this exact candidate + generation. Without a machine we cannot
	// resume from durable evidence — re-verify instead.
	if g.Machine == nil {
		return nil, false, nil
	}
	ts, err := g.Machine.EventStore().CurrentState(bind.TaskRef)
	if err != nil {
		return nil, false, fmt.Errorf("completion: load lifecycle state: %w", err)
	}
	if ts == nil || ts.CandidateSHA != bind.CandidateSHA {
		return nil, false, nil
	}
	if ts.LeaseGeneration > 0 && ts.LeaseGeneration != bind.LeaseGeneration {
		return nil, false, nil
	}
	// Evidence digest is on the latest event, not TaskState. Load history.
	digest, err := g.latestEvidenceDigest(bind.TaskRef, bind.CandidateSHA)
	if err != nil || digest == "" {
		return nil, false, err
	}
	receipt, err := g.Admission.RequireCurrentPassing(ctx, bind.WorktreeDir, digest)
	if err != nil {
		// Stale or non-PASS — fall through to re-verify.
		return nil, false, nil
	}
	if err := g.bindMatchesReceipt(bind, receipt); err != nil {
		return nil, false, nil
	}
	replayed, err := g.recordPassing(bind, receipt)
	if err != nil {
		return nil, false, err
	}
	return &CompletionDecision{
		Outcome:     verifier.OutcomePASS,
		Digest:      receipt.Digest,
		Receipt:     receipt,
		Reason:      "PASS (resumed)",
		Replayed:    replayed,
		ReviewReady: true,
	}, true, nil
}

func (g *CompletionGate) latestEvidenceDigest(taskRef, candidateSHA string) (string, error) {
	if g.Machine == nil {
		return "", nil
	}
	events, err := g.Machine.EventStore().Events(taskRef)
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.CandidateSHA == candidateSHA && strings.HasPrefix(ev.EvidenceDigest, "sha256:") {
			return ev.EvidenceDigest, nil
		}
	}
	return "", nil
}

func (g *CompletionGate) recordPassing(bind CompletionBinding, receipt *verifier.Receipt) (replayed bool, err error) {
	if g.Machine == nil {
		// Receipt is durable; lifecycle/outbox composition is optional for
		// unit seams that only exercise admission. Production wiring always
		// supplies a Machine.
		return false, nil
	}
	actor := g.Actor
	if actor == "" {
		actor = "completion-gate"
	}
	payload, _ := json.Marshal(reviewEnqueuePayload{
		TaskRef:          bind.TaskRef,
		CandidateSHA:     bind.CandidateSHA,
		ReceiptDigest:    receipt.Digest,
		LeaseGeneration:  bind.LeaseGeneration,
		ProviderRevision: bind.ProviderRevision,
		PatchID:          bind.PatchID,
		Classifier:       bind.ClassifierVersion,
		Profile:          bind.VerificationProfile,
		ProfileDigest:    bind.ProfileDigest,
		ConfigRevision:   bind.ConfigRevision,
	})
	// Idempotency is keyed on task + candidate + digest so a restart after
	// verify-before-commit converges without double consumption.
	key := fmt.Sprintf("verify-pass:%s:%s:%s", bind.TaskRef, bind.CandidateSHA, receipt.Digest)

	// Identical key already recorded → pure replay (no ValidTransition check).
	if existing, err := g.Machine.EventStore().EventByIdempotencyKey(key); err != nil {
		return false, err
	} else if existing != nil {
		// Ensure the review-enqueue outbox intent is also present (same key).
		items := []outbox.Item{{
			IdempotencyKey: key + ":review-enqueue",
			TaskRef:        bind.TaskRef,
			Kind:           "review_enqueue",
			Payload:        string(payload),
		}}
		// Re-run Transition so outbox items enroll on replay path too
		// (Machine re-enqueues identical items; outbox dedups).
		res, err := g.Machine.Transition(lifecycle.TransitionRequest{
			TaskRef:          bind.TaskRef,
			Repo:             bind.Repo,
			To:               existing.ToState,
			Actor:            actor,
			IdempotencyKey:   key,
			LeaseGeneration:  bind.LeaseGeneration,
			ProviderRevision: bind.ProviderRevision,
			Branch:           bind.Branch,
			CandidateSHA:     bind.CandidateSHA,
			EvidenceDigest:   receipt.Digest,
			Payload:          string(payload),
			OutboxItems:      items,
		})
		if err != nil {
			return false, fmt.Errorf("completion: lifecycle pass replay: %w", sanitizeErr(err, bind.WorktreeDir))
		}
		return res.Replayed, nil
	}

	cur, err := g.Machine.EventStore().CurrentState(bind.TaskRef)
	if err != nil {
		return false, err
	}
	if cur == nil {
		return false, fmt.Errorf("completion: no lifecycle state for %s (dispatch must advance to building before verify)", bind.TaskRef)
	}
	// Already reviewing with matching candidate: evidence is live; do not
	// invent an illegal self-transition.
	if cur.State == lifecycle.StateReviewing && cur.CandidateSHA == bind.CandidateSHA {
		return true, nil
	}
	if cur.State != lifecycle.StateBuilding && cur.State != lifecycle.StateVerifying && cur.State != lifecycle.StateRecovering {
		return false, fmt.Errorf("completion: lifecycle state %s cannot accept verification for %s", cur.State, bind.TaskRef)
	}

	items := []outbox.Item{{
		IdempotencyKey: key + ":review-enqueue",
		TaskRef:        bind.TaskRef,
		Kind:           "review_enqueue",
		Payload:        string(payload),
	}}
	// Intermediate Verifying step when coming from Building so the event
	// log shows verify-then-review rather than a skip.
	if cur.State == lifecycle.StateBuilding {
		vKey := key + ":verifying"
		if _, err := g.Machine.Transition(lifecycle.TransitionRequest{
			TaskRef:          bind.TaskRef,
			Repo:             bind.Repo,
			To:               lifecycle.StateVerifying,
			Actor:            actor,
			IdempotencyKey:   vKey,
			LeaseGeneration:  bind.LeaseGeneration,
			ProviderRevision: bind.ProviderRevision,
			Branch:           bind.Branch,
			CandidateSHA:     bind.CandidateSHA,
			EvidenceDigest:   receipt.Digest,
			Payload:          string(payload),
		}); err != nil {
			return false, fmt.Errorf("completion: lifecycle verifying: %w", sanitizeErr(err, bind.WorktreeDir))
		}
	}
	res, err := g.Machine.Transition(lifecycle.TransitionRequest{
		TaskRef:          bind.TaskRef,
		Repo:             bind.Repo,
		To:               lifecycle.StateReviewing,
		Actor:            actor,
		IdempotencyKey:   key,
		LeaseGeneration:  bind.LeaseGeneration,
		ProviderRevision: bind.ProviderRevision,
		Branch:           bind.Branch,
		CandidateSHA:     bind.CandidateSHA,
		EvidenceDigest:   receipt.Digest,
		Payload:          string(payload),
		OutboxItems:      items,
	})
	if err != nil {
		return false, fmt.Errorf("completion: lifecycle pass: %w", sanitizeErr(err, bind.WorktreeDir))
	}
	return res.Replayed, nil
}

func (g *CompletionGate) recordNonPass(bind CompletionBinding, receipt *verifier.Receipt, to lifecycle.State) (bool, error) {
	if g.Machine == nil {
		return false, nil
	}
	actor := g.Actor
	if actor == "" {
		actor = "completion-gate"
	}
	key := fmt.Sprintf("verify-%s:%s:%s:%s", strings.ToLower(string(receipt.Outcome)), bind.TaskRef, bind.CandidateSHA, receipt.Digest)
	cur, err := g.Machine.EventStore().CurrentState(bind.TaskRef)
	if err != nil {
		return false, err
	}
	if cur == nil {
		// No lifecycle yet — receipt is still durable; skip event.
		return false, nil
	}
	// Only transition when legal from current state.
	if !lifecycle.ValidTransition(cur.State, to) {
		// Prefer Recovering/Blocked fold when direct edge missing.
		if lifecycle.ValidTransition(cur.State, lifecycle.StateBlocked) && to == lifecycle.StateBlocked {
			// ok
		} else if lifecycle.ValidTransition(cur.State, lifecycle.StateBuilding) && to == lifecycle.StateBuilding {
			// ok
		} else {
			return false, nil
		}
	}
	res, err := g.Machine.Transition(lifecycle.TransitionRequest{
		TaskRef:          bind.TaskRef,
		Repo:             bind.Repo,
		To:               to,
		Actor:            actor,
		IdempotencyKey:   key,
		LeaseGeneration:  bind.LeaseGeneration,
		ProviderRevision: bind.ProviderRevision,
		Branch:           bind.Branch,
		CandidateSHA:     bind.CandidateSHA,
		EvidenceDigest:   receipt.Digest,
		Payload:          fmt.Sprintf(`{"outcome":%q,"digest":%q}`, receipt.Outcome, receipt.Digest),
	})
	if err != nil {
		// Non-pass lifecycle is best-effort relative to the durable FAIL/
		// BLOCKED receipt; surface but do not invent a pass path.
		return false, fmt.Errorf("completion: lifecycle %s: %w", to, sanitizeErr(err, bind.WorktreeDir))
	}
	return res.Replayed, nil
}

// safeReason strips absolute host paths from error text so rejection
// reasons can be logged and mailed without leaking the operator host.
func safeReason(err error, worktreeDir string) string {
	if err == nil {
		return ""
	}
	return sanitizeErr(err, worktreeDir).Error()
}

func sanitizeErr(err error, worktreeDir string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if worktreeDir != "" {
		msg = strings.ReplaceAll(msg, worktreeDir, ".")
		msg = strings.ReplaceAll(msg, filepath.Clean(worktreeDir), ".")
	}
	// Drop other absolute path segments that may appear in exec errors.
	parts := strings.Fields(msg)
	for i, p := range parts {
		if strings.HasPrefix(p, "/") || hasWindowsDrivePrefix(p) {
			parts[i] = filepath.Base(p)
		}
	}
	msg = strings.Join(parts, " ")
	return errors.New(msg)
}

// SeedLifecycleToBuilding advances a fresh task Draft→…→Building so the
// completion gate can legally record verification. Production dispatch
// owns this path; tests use it as a fixture helper.
func SeedLifecycleToBuilding(m *lifecycle.Machine, taskRef, repo string, leaseGen int64) error {
	if m == nil {
		return errors.New("nil lifecycle machine")
	}
	steps := []lifecycle.State{
		lifecycle.StateEligible,
		lifecycle.StateClaimed,
		lifecycle.StateDispatched,
		lifecycle.StateBuilding,
	}
	for i, to := range steps {
		if _, err := m.Transition(lifecycle.TransitionRequest{
			TaskRef:         taskRef,
			Repo:            repo,
			To:              to,
			Actor:           "seed",
			IdempotencyKey:  fmt.Sprintf("seed:%s:%d:%s", taskRef, leaseGen, to),
			LeaseGeneration: leaseGen,
		}); err != nil {
			return fmt.Errorf("seed step %d (%s): %w", i, to, err)
		}
	}
	return nil
}

// hasWindowsDrivePrefix reports whether p starts with a Windows drive
// designator: a letter, a colon, then a separator.
//
// This replaces a hardcoded drive-letter string comparison. That literal made
// this file trip the repository's own absolute-path preflight, which scans
// every .go file for host path prefixes and cannot distinguish a path being
// EMBEDDED from a path being DETECTED. CI failed on main for a file whose
// entire purpose is stripping absolute paths out of error text.
//
// Matching structurally is also simply more correct: the old literal only
// matched one drive letter, so every other volume walked straight through.
func hasWindowsDrivePrefix(p string) bool {
	if len(p) < 3 {
		return false
	}
	c := p[0]
	if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') {
		return false
	}
	return p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}
