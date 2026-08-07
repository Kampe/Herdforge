package envelope

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// IssueOpts are the per-message fields a coordinator supplies when minting a
// control envelope. Sequence is assigned monotonically by the Issuer per
// (target worker session, target task) unless Sequence is non-zero (tests).
type IssueOpts struct {
	Kind                Kind
	TargetTask          string
	LeaseGeneration     int64
	TargetWorkerSession string
	Body                string
	Scope               *Scope
	// Sequence, when non-zero, overrides the issuer's auto-increment (tests).
	Sequence uint64
	// TTL bounds ExpiresAt; zero uses DefaultTTL.
	TTL time.Duration
	// ID and Nonce, when empty, are random 128-bit hex strings.
	ID    string
	Nonce string
}

// DefaultTTL is how long a freshly issued envelope remains acceptable.
const DefaultTTL = 15 * time.Minute

// Issuer mints MAC-signed control envelopes. It is the coordinator-side
// producer; workers never issue.
type Issuer struct {
	mu       sync.Mutex
	secret   []byte
	role     string
	session  string
	authority string
	// lastSeq[workerSession+"\x00"+task] → last issued sequence
	lastSeq map[string]uint64
	now     func() time.Time
}

// NewIssuer builds a fail-closed Issuer. Empty secret/role/session is refused.
func NewIssuer(secret, role, session string) (*Issuer, error) {
	if secret == "" {
		return nil, ErrMissingSecret
	}
	if role == "" || session == "" {
		return nil, fmt.Errorf("%w: issuer role and session required", ErrMissingBinding)
	}
	if _, ok := DefaultAllowedIssuerRoles()[role]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnauthorizedIssuer, role)
	}
	return &Issuer{
		secret:    []byte(secret),
		role:      role,
		session:   session,
		authority: DefaultPolicyAuthority,
		lastSeq:   make(map[string]uint64),
		now:       time.Now,
	}, nil
}

// Issue creates, sequences, and signs a control envelope bound to opts.
func (i *Issuer) Issue(opts IssueOpts) (*Envelope, error) {
	if i == nil || len(i.secret) == 0 {
		return nil, ErrMissingSecret
	}
	if opts.Kind == "" {
		opts.Kind = KindScopeCorrection
	}
	if opts.TargetTask == "" || opts.TargetWorkerSession == "" {
		return nil, fmt.Errorf("%w: target task and worker session", ErrMissingBinding)
	}
	if opts.LeaseGeneration < 0 {
		return nil, fmt.Errorf("%w: lease generation", ErrMissingFields)
	}
	if opts.Kind == KindScopeCorrection && opts.Scope == nil && opts.Body == "" {
		return nil, ErrEmptyScope
	}

	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	now := i.now()
	id := opts.ID
	if id == "" {
		var err error
		id, err = randomHex(16)
		if err != nil {
			return nil, err
		}
	}
	nonce := opts.Nonce
	if nonce == "" {
		var err error
		nonce, err = randomHex(16)
		if err != nil {
			return nil, err
		}
	}

	seq := opts.Sequence
	if seq == 0 {
		key := opts.TargetWorkerSession + "\x00" + opts.TargetTask
		i.mu.Lock()
		i.lastSeq[key]++
		seq = i.lastSeq[key]
		i.mu.Unlock()
	} else {
		key := opts.TargetWorkerSession + "\x00" + opts.TargetTask
		i.mu.Lock()
		if seq > i.lastSeq[key] {
			i.lastSeq[key] = seq
		}
		i.mu.Unlock()
	}

	e := &Envelope{
		Version:             "1",
		ID:                  id,
		Kind:                opts.Kind,
		Sequence:            seq,
		Nonce:               nonce,
		IssuerRole:          i.role,
		IssuerSession:       i.session,
		PolicyAuthority:     i.authority,
		TargetTask:          opts.TargetTask,
		LeaseGeneration:     opts.LeaseGeneration,
		TargetWorkerSession: opts.TargetWorkerSession,
		IssuedAtUnix:        now.Unix(),
		ExpiresAtUnix:       now.Add(ttl).Unix(),
		Body:                opts.Body,
		Scope:               CloneScope(opts.Scope),
	}
	if err := Sign(i.secret, e); err != nil {
		return nil, err
	}
	return e, nil
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("envelope: entropy: %w", err)
	}
	return hex.EncodeToString(b), nil
}
