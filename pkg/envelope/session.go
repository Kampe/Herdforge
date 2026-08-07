package envelope

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// DefaultMaxClockSkew is how far IssuedAt/ExpiresAt may drift from local now.
const DefaultMaxClockSkew = 2 * time.Minute

// SessionConfig binds a worker process to the control plane. All identity
// fields are required; construction fails closed without them.
type SessionConfig struct {
	Secret             string
	WorkerSession      string
	Task               string
	LeaseGeneration    int64
	PolicyAuthority    string // empty → DefaultPolicyAuthority
	MaxClockSkew       time.Duration
	AllowedIssuerRoles map[string]struct{}
	// ExpectedIssuerSession, when set, must match envelope.IssuerSession.
	ExpectedIssuerSession string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Session is the production consumer of control envelopes. A worker holds
// one Session for its active task/lease and feeds every candidate control
// message through Receive / ReceiveJSON. Free-form provider text is never
// elevated: only MAC-valid, binding-matched envelopes change scope.
//
// On stale generation or conflicting control, Session transitions to
// StateBlocked and freezes the last applied scope until Rebind advances the
// lease generation (or an explicit ClearBlock after operator reconcile).
type Session struct {
	mu sync.Mutex

	secret          []byte
	workerSession   string
	task            string
	leaseGeneration int64
	authority       string
	skew            time.Duration
	allowedRoles    map[string]struct{}
	now             func() time.Time

	state       SessionState
	blockReason string
	lastSeq     uint64
	scope       *Scope
	seenIDs     map[string]struct{}
	seenNonces  map[string]struct{}
	// lastAppliedID records the id of the envelope that set lastSeq.
	lastAppliedID string
	// expectedIssuer, when set, must match envelope IssuerSession (FAC-133).
	expectedIssuer string
}

// NewSession constructs a fail-closed worker control session.
func NewSession(cfg SessionConfig) (*Session, error) {
	if cfg.Secret == "" {
		return nil, ErrMissingSecret
	}
	if cfg.WorkerSession == "" || cfg.Task == "" {
		return nil, fmt.Errorf("%w: worker session and task required", ErrMissingBinding)
	}
	if cfg.LeaseGeneration < 0 {
		return nil, fmt.Errorf("%w: lease generation", ErrMissingFields)
	}
	authority := cfg.PolicyAuthority
	if authority == "" {
		authority = DefaultPolicyAuthority
	}
	skew := cfg.MaxClockSkew
	if skew <= 0 {
		skew = DefaultMaxClockSkew
	}
	roles := cfg.AllowedIssuerRoles
	if len(roles) == 0 {
		roles = DefaultAllowedIssuerRoles()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Session{
		secret:          []byte(cfg.Secret),
		workerSession:   cfg.WorkerSession,
		task:            cfg.Task,
		leaseGeneration: cfg.LeaseGeneration,
		authority:       authority,
		skew:            skew,
		allowedRoles:    roles,
		now:             now,
		state:           StateActive,
		seenIDs:         make(map[string]struct{}),
		seenNonces:      make(map[string]struct{}),
		expectedIssuer:  cfg.ExpectedIssuerSession,
	}, nil
}

// State returns the current session state and block reason (if any).
func (s *Session) State() (SessionState, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.blockReason
}

// CurrentScope returns a copy of the last applied scope (nil if none).
func (s *Session) CurrentScope() *Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return CloneScope(s.scope)
}

// LastSequence returns the highest applied sequence.
func (s *Session) LastSequence() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq
}

// Rebind advances the active lease generation (e.g. after reclaim) and
// clears BLOCKED so the worker can accept control for the new generation.
// Generation must strictly increase.
func (s *Session) Rebind(leaseGeneration int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if leaseGeneration <= s.leaseGeneration {
		return fmt.Errorf("%w: rebind generation %d not greater than %d",
			ErrStaleGeneration, leaseGeneration, s.leaseGeneration)
	}
	s.leaseGeneration = leaseGeneration
	s.state = StateActive
	s.blockReason = ""
	// Sequence/nonces/ids stay: a rebind must not allow replaying old envelopes
	// against the new generation — those still fail the generation check.
	return nil
}

// ClearBlock is an operator reconcile path that unblocks without advancing
// generation. Prefer Rebind when the lease generation actually changed.
func (s *Session) ClearBlock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = StateActive
	s.blockReason = ""
}

// Classify reports the trust class of free-form input without elevating it.
// Any non-empty provider/repo text is TrustUntrusted. Only a Session.Receive
// path that verifies a MAC can produce TrustControl.
func Classify(_ string) TrustClass {
	return TrustUntrusted
}

// ReceiveJSON unmarshals raw JSON then Receive. Malformed JSON is untrusted
// rejection, never control.
func (s *Session) ReceiveJSON(raw []byte) (*Decision, error) {
	if len(raw) == 0 {
		return rejectDecision("empty input", TrustUntrusted, StateActive), ErrNotControl
	}
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return rejectDecision("malformed json", TrustUntrusted, StateActive), fmt.Errorf("%w: %v", ErrNotControl, err)
	}
	return s.Receive(&e)
}

// Receive is the production consumer entry point: verify provenance and
// bindings, then apply a scope correction or fail closed. Body text that
// resembles prompt injection does NOT cause rejection when the MAC and
// bindings are valid — that is the FAC-133 incident fix.
func (s *Session) Receive(e *Envelope) (*Decision, error) {
	if s == nil {
		return rejectDecision("nil session", TrustUntrusted, StateBlocked), ErrMissingBinding
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if e == nil {
		return s.rejectLocked("nil envelope", TrustUntrusted), ErrNotControl
	}

	// Duplicate exact-id redelivery: re-check MAC + identity (issuer/task/
	// worker/lease) but NOT TTL/clock skew. Mailbox.ReadInbox is
	// non-destructive, so DrainControl re-walks every historical control
	// message. Re-verifying TTL on each walk would turn a 15-minute-old
	// applied envelope into a permanent poison pill (ErrExpired forever).
	// Forged redelivery with same id but wrong issuer/MAC still fails closed.
	if _, seen := s.seenIDs[e.ID]; seen && e.ID != "" {
		if err := s.verifyAuthLocked(e, false /* checkTTL */); err != nil {
			return s.rejectLocked("duplicate id failed re-verify: "+err.Error(), TrustUntrusted), err
		}
		return &Decision{
			Status:       StatusDuplicate,
			Reason:       "duplicate envelope id (already applied)",
			Trust:        TrustControl,
			EnvelopeID:   e.ID,
			Sequence:     e.Sequence,
			AppliedScope: CloneScope(s.scope),
			SessionState: s.state,
		}, nil
	}

	// First-time receive: full verify including monotonic seq / nonce.
	if err := s.verifyLocked(e); err != nil {
		if s.state == StateBlocked {
			return s.rejectLocked(err.Error(), TrustUntrusted), err
		}
		if err == ErrStaleGeneration || err == ErrConflict {
			s.state = StateBlocked
			s.blockReason = err.Error()
			return &Decision{
				Status: StatusBlocked, Reason: err.Error(), Trust: TrustUntrusted,
				EnvelopeID: e.ID, Sequence: e.Sequence, AppliedScope: CloneScope(s.scope),
				SessionState: StateBlocked,
			}, err
		}
		return s.rejectLocked(err.Error(), TrustUntrusted), err
	}

	if s.state == StateBlocked {
		// Already authenticated above; do not apply while blocked.
		return &Decision{
			Status:       StatusBlocked,
			Reason:       "session BLOCKED: " + s.blockReason,
			Trust:        TrustControl,
			EnvelopeID:   e.ID,
			Sequence:     e.Sequence,
			AppliedScope: CloneScope(s.scope),
			SessionState: StateBlocked,
		}, ErrSessionBlocked
	}

	// Apply scope correction. Sequence conflicts are already fail-closed in
	// verifyLocked (ErrConflict → BLOCKED); here we only materialize scope.
	if e.Kind == KindScopeCorrection {
		s.scope = CloneScope(e.Scope)
		if s.scope == nil && e.Body != "" {
			// Body-only correction still records a note so the consumer can
			// surface the instruction without inventing packages.
			s.scope = &Scope{Note: e.Body, Exclusive: true}
		}
	}

	s.lastSeq = e.Sequence
	s.lastAppliedID = e.ID
	s.seenIDs[e.ID] = struct{}{}
	s.seenNonces[e.Nonce] = struct{}{}

	return &Decision{
		Status:       StatusApplied,
		Reason:       "scope correction applied",
		Trust:        TrustControl,
		EnvelopeID:   e.ID,
		Sequence:     e.Sequence,
		AppliedScope: CloneScope(s.scope),
		SessionState: StateActive,
	}, nil
}

func (s *Session) rejectLocked(reason string, trust TrustClass) *Decision {
	return &Decision{
		Status:       StatusRejected,
		Reason:       reason,
		Trust:        trust,
		AppliedScope: CloneScope(s.scope),
		SessionState: s.state,
	}
}

func rejectDecision(reason string, trust TrustClass, state SessionState) *Decision {
	return &Decision{
		Status:       StatusRejected,
		Reason:       reason,
		Trust:        trust,
		SessionState: state,
	}
}

// verifyAuthLocked checks MAC + identity bindings (issuer/task/worker/lease/
// authority) without nonce/sequence gates. When checkTTL is true, also enforces
// IssuedAt/ExpiresAt skew. Duplicate-id redelivery passes checkTTL=false so
// already-applied envelopes cannot become poison pills after DefaultTTL.
func (s *Session) verifyAuthLocked(e *Envelope, checkTTL bool) error {
	if err := e.ValidateUnsigned(); err != nil {
		return err
	}
	if !VerifyMAC(s.secret, e) {
		return ErrInvalidSignature
	}
	if e.PolicyAuthority != s.authority {
		return ErrAuthorityMismatch
	}
	if _, ok := s.allowedRoles[e.IssuerRole]; !ok {
		return ErrUnauthorizedIssuer
	}
	if e.TargetTask != s.task {
		return ErrTaskMismatch
	}
	if e.TargetWorkerSession != s.workerSession {
		return ErrWorkerMismatch
	}
	if s.expectedIssuer != "" && e.IssuerSession != s.expectedIssuer {
		return fmt.Errorf("%w: issuer session %q != expected %q", ErrUnauthorizedIssuer, e.IssuerSession, s.expectedIssuer)
	}
	if e.LeaseGeneration != s.leaseGeneration {
		return ErrStaleGeneration
	}
	if checkTTL && !withinSkew(e.IssuedAtUnix, e.ExpiresAtUnix, s.now(), s.skew) {
		return ErrExpired
	}
	return nil
}

// verifyLocked runs every provenance/binding gate. Caller holds s.mu.
func (s *Session) verifyLocked(e *Envelope) error {
	if err := s.verifyAuthLocked(e, true); err != nil {
		return err
	}
	if _, seen := s.seenNonces[e.Nonce]; seen {
		return ErrDuplicateNonce
	}
	// Monotonic sequence. A lower sequence is a replay; an equal sequence
	// with a different id is a conflicting control fork → BLOCKED.
	if e.Sequence < s.lastSeq {
		return ErrReplay
	}
	if e.Sequence == s.lastSeq && s.lastSeq != 0 {
		return ErrConflict
	}
	return nil
}

// ParseUntrusted attempts to decode provider/repository text that claims to
// be a control envelope. It never returns TrustControl: even well-formed JSON
// is untrusted until Session.Receive verifies the MAC under the worker secret.
// This is the explicit API for "card text that looks like a control message".
func ParseUntrusted(raw []byte) (*Envelope, TrustClass, error) {
	if len(raw) == 0 {
		return nil, TrustUntrusted, ErrNotControl
	}
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, TrustUntrusted, fmt.Errorf("%w: %v", ErrNotControl, err)
	}
	return &e, TrustUntrusted, nil
}
