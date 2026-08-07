// Package envelope provides authenticated trusted control envelopes so a
// worker can accept a legitimate coordinator control message (for example a
// scope correction) without treating it as prompt injection.
//
// Provider/repository text is untrusted free-form data and can never forge
// an envelope. Only a MAC-valid envelope bound to the active task, lease
// generation, and worker session is trusted. Receivers fail closed: spoofed,
// replayed, cross-task, or stale-generation envelopes are rejected or force
// an observable BLOCKED state rather than silently continuing the old scope.
//
// FAC-133 / live incident: a FAC-146 worker rejected a valid orchestrator
// scope correction as "prompt injection" because content heuristics ran
// before provenance. Trust is the signature and binding fields, never body
// text that happens to mention injection, secrets, or shell commands.
package envelope

import (
	"errors"
	"fmt"
	"time"
)

// DefaultPolicyAuthority is the control-plane policy that may issue trusted
// instructions. Receivers reject any other authority.
const DefaultPolicyAuthority = "herd.control.v1"

// Kind identifies the control instruction family.
type Kind string

const (
	// KindScopeCorrection narrows or reasserts the worker's exclusive package
	// allowlist / scope note without elevating merge or credential authority.
	KindScopeCorrection Kind = "scope.correction"
)

// Known issuer roles that may mint control envelopes under policy.
const (
	RoleOrchestrator = "orchestrator"
	RoleCoordinator  = "coordinator"
	RoleAuditor      = "auditor"
)

// TrustClass is how a receiver classifies input before (or after) verification.
type TrustClass string

const (
	// TrustUntrusted is provider/repo/free-form text. It is never control.
	TrustUntrusted TrustClass = "untrusted"
	// TrustControl is a MAC-valid envelope bound to the active session.
	TrustControl TrustClass = "control"
)

// Status is the structured outcome of Session.Receive.
type Status string

const (
	// StatusApplied: envelope verified and applied (scope updated).
	StatusApplied Status = "applied"
	// StatusRejected: fail-closed refuse; session stays Active (or remains Blocked).
	StatusRejected Status = "rejected"
	// StatusBlocked: stale/conflicting control forces observable BLOCKED.
	StatusBlocked Status = "blocked"
	// StatusDuplicate: exact-id redelivery of an already-applied envelope.
	StatusDuplicate Status = "duplicate"
)

// SessionState is the durable control state a worker holds.
type SessionState string

const (
	// StateActive: worker may apply new verified control.
	StateActive SessionState = "active"
	// StateBlocked: control plane failed closed; scope is frozen until rebind.
	StateBlocked SessionState = "blocked"
)

// Scope is the structured payload of a scope correction. Exclusive means the
// worker must not expand beyond PackageAllowlist (when non-empty).
type Scope struct {
	PackageAllowlist []string `json:"package_allowlist,omitempty"`
	Exclusive        bool     `json:"exclusive"`
	Note             string   `json:"note,omitempty"`
}

// Envelope is the authenticated control message. Signature is computed over
// Canonical() and is never part of the signed material itself.
type Envelope struct {
	Version             string `json:"v"`
	ID                  string `json:"id"`
	Kind                Kind   `json:"kind"`
	Sequence            uint64 `json:"seq"`
	Nonce               string `json:"nonce"`
	IssuerRole          string `json:"issuer_role"`
	IssuerSession       string `json:"issuer_session"`
	PolicyAuthority     string `json:"policy_authority"`
	TargetTask          string `json:"target_task"`
	LeaseGeneration     int64  `json:"lease_generation"`
	TargetWorkerSession string `json:"target_worker_session"`
	IssuedAtUnix        int64  `json:"issued_at"`
	ExpiresAtUnix       int64  `json:"expires_at"`
	// Body may contain text that resembles prompt injection. Authenticity is
	// the MAC + binding fields, never a content heuristic over Body.
	Body      string `json:"body"`
	Scope     *Scope `json:"scope,omitempty"`
	Signature string `json:"sig"`
}

// Decision is the structured result of receiving a candidate control message.
// Callers gate on Status; Reason is diagnostic only.
type Decision struct {
	Status       Status
	Reason       string
	Trust        TrustClass
	EnvelopeID   string
	Sequence     uint64
	AppliedScope *Scope
	SessionState SessionState
}

// Sentinel errors for typed fail-closed paths.
var (
	ErrMissingSecret       = errors.New("envelope: secret is required (fail-closed)")
	ErrMissingBinding      = errors.New("envelope: session binding incomplete (fail-closed)")
	ErrInvalidSignature    = errors.New("envelope: invalid signature")
	ErrUnknownKind         = errors.New("envelope: unknown control kind")
	ErrUnauthorizedIssuer  = errors.New("envelope: issuer role not authorized")
	ErrAuthorityMismatch   = errors.New("envelope: policy authority mismatch")
	ErrTaskMismatch        = errors.New("envelope: target task mismatch")
	ErrWorkerMismatch      = errors.New("envelope: target worker session mismatch")
	ErrStaleGeneration     = errors.New("envelope: stale lease generation")
	ErrReplay              = errors.New("envelope: replay or non-monotonic sequence")
	ErrDuplicateID         = errors.New("envelope: envelope id already seen")
	ErrDuplicateNonce      = errors.New("envelope: nonce already seen")
	ErrExpired             = errors.New("envelope: envelope expired or outside clock skew")
	ErrMissingFields       = errors.New("envelope: required fields missing")
	ErrSessionBlocked      = errors.New("envelope: session is BLOCKED; rebind required")
	ErrConflict            = errors.New("envelope: conflicting control instruction")
	ErrNotControl          = errors.New("envelope: input is not a trusted control envelope")
	ErrEmptyScope          = errors.New("envelope: scope correction requires scope or body")
)

// DefaultAllowedIssuerRoles is the policy allowlist for control issuers.
func DefaultAllowedIssuerRoles() map[string]struct{} {
	return map[string]struct{}{
		RoleOrchestrator: {},
		RoleCoordinator:  {},
		RoleAuditor:      {},
	}
}

// CloneScope returns a deep copy of s (nil-safe).
func CloneScope(s *Scope) *Scope {
	if s == nil {
		return nil
	}
	out := &Scope{
		Exclusive: s.Exclusive,
		Note:      s.Note,
	}
	if len(s.PackageAllowlist) > 0 {
		out.PackageAllowlist = append([]string(nil), s.PackageAllowlist...)
	}
	return out
}

// EqualScope reports whether a and b describe the same scope.
func EqualScope(a, b *Scope) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Exclusive != b.Exclusive || a.Note != b.Note {
		return false
	}
	if len(a.PackageAllowlist) != len(b.PackageAllowlist) {
		return false
	}
	for i := range a.PackageAllowlist {
		if a.PackageAllowlist[i] != b.PackageAllowlist[i] {
			return false
		}
	}
	return true
}

// ValidateUnsigned checks structural completeness before signing or verifying.
// It does not check the MAC.
func (e *Envelope) ValidateUnsigned() error {
	if e == nil {
		return fmt.Errorf("%w: nil envelope", ErrMissingFields)
	}
	if e.Version == "" || e.ID == "" || e.Kind == "" || e.Nonce == "" {
		return fmt.Errorf("%w: version/id/kind/nonce", ErrMissingFields)
	}
	if e.Sequence == 0 {
		return fmt.Errorf("%w: sequence must be >= 1", ErrMissingFields)
	}
	if e.IssuerRole == "" || e.IssuerSession == "" || e.PolicyAuthority == "" {
		return fmt.Errorf("%w: issuer/policy", ErrMissingFields)
	}
	if e.TargetTask == "" || e.TargetWorkerSession == "" {
		return fmt.Errorf("%w: target binding", ErrMissingFields)
	}
	if e.LeaseGeneration < 0 {
		return fmt.Errorf("%w: negative lease generation", ErrMissingFields)
	}
	if e.IssuedAtUnix <= 0 || e.ExpiresAtUnix <= 0 {
		return fmt.Errorf("%w: timestamps", ErrMissingFields)
	}
	if e.ExpiresAtUnix < e.IssuedAtUnix {
		return fmt.Errorf("%w: expires before issued", ErrMissingFields)
	}
	switch e.Kind {
	case KindScopeCorrection:
		if e.Scope == nil && e.Body == "" {
			return ErrEmptyScope
		}
	default:
		return fmt.Errorf("%w: %q", ErrUnknownKind, e.Kind)
	}
	return nil
}

// withinSkew reports whether now is inside [issued - skew, expires + skew].
func withinSkew(issued, expires int64, now time.Time, skew time.Duration) bool {
	n := now.Unix()
	skewSec := int64(skew / time.Second)
	if skewSec < 0 {
		skewSec = 0
	}
	if n+skewSec < issued {
		return false // issued too far in the future
	}
	if n-skewSec > expires {
		return false // expired
	}
	return true
}
