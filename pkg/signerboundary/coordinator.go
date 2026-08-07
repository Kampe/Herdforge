package signerboundary

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// ReviewerSigner is the coordinator-side consumer of the OS signing boundary.
// FAC-145 / review path: Admit then SignVerdict — never sign without a durable
// ledger grant that serve (S) will re-read from disk.
type ReviewerSigner struct {
	boundary *Boundary
	ledger   *DurableAdmissionLedger
	keyDir   string
}

// OpenReviewerSigner opens an established boundary and the durable admission ledger.
// Must run as HERD_REQUESTER_UID.
func OpenReviewerSigner(opts Options) (*ReviewerSigner, error) {
	if strings.TrimSpace(opts.KeyDir) == "" {
		return nil, fmt.Errorf("%w: KeyDir required", ErrBoundaryNotEstablished)
	}
	b, err := Open(opts)
	if err != nil {
		return nil, err
	}
	topo, err := RequireTopology()
	if err != nil {
		return nil, err
	}
	led, err := OpenAdmissionLedgerTopo(AdmissionLedgerPath(opts.KeyDir), topo)
	if err != nil {
		return nil, err
	}
	return &ReviewerSigner{boundary: b, ledger: led, keyDir: opts.KeyDir}, nil
}

// Admit records a durable grant for a specific exact verdict (FAC-145 channel).
func (r *ReviewerSigner) Admit(candidateSHA, baseSHA, patchID, verdict, sessionID string, ttlSec int64) (tokenID string, err error) {
	if r == nil || r.ledger == nil {
		return "", ErrBoundaryNotEstablished
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tokenID = hex.EncodeToString(buf)
	rec := AdmissionRecord{
		TokenID:      tokenID,
		CandidateSHA: candidateSHA,
		BaseSHA:      baseSHA,
		PatchID:      patchID,
		SessionID:    sessionID,
		Verdict:      verdict,
		SingleUse:    true,
	}
	if ttlSec > 0 {
		rec.ExpiresUnix = nowUnix() + ttlSec
		rec.SingleUse = false
	}
	if err := r.ledger.AppendGrant(rec); err != nil {
		return "", err
	}
	return tokenID, nil
}

// SignAdmittedVerdict requires a prior Admit grant; serve re-checks the ledger.
func (r *ReviewerSigner) SignAdmittedVerdict(candidateSHA, baseSHA, patchID, verdict, sessionID string, payload []byte) ([]byte, error) {
	if r == nil || r.boundary == nil {
		return nil, ErrBoundaryNotEstablished
	}
	// Local structural check + ensure we are not agent-role.
	if err := refuseAgentRole(); err != nil {
		return nil, err
	}
	return r.boundary.SignVerdict(candidateSHA, baseSHA, patchID, verdict, sessionID, payload)
}

// PublicKey returns the verification key.
func (r *ReviewerSigner) PublicKey() []byte {
	if r == nil || r.boundary == nil {
		return nil
	}
	return r.boundary.PublicKey()
}

// Boundary exposes the underlying handle for advanced ops.
func (r *ReviewerSigner) Boundary() *Boundary {
	if r == nil {
		return nil
	}
	return r.boundary
}

// LedgerPath is the durable admission file path (FAC-145 install surface).
func (r *ReviewerSigner) LedgerPath() string {
	if r == nil || r.ledger == nil {
		return ""
	}
	return r.ledger.Path()
}

// MustBeRequester fails closed when process is not R.
func MustBeRequester() error {
	_, err := RequireTopology()
	return err
}

// SessionFDEnv is the FD number env name for R (value is the integer FD, not secret bytes).
const SessionFDEnv = "HERD_SIGNER_SESSION_KEY_FD"

// WriteSessionKeyToFD writes the session key hex line to an open FD (R handoff).
func WriteSessionKeyToFD(fd int, sk SessionKey) error {
	f := os.NewFile(uintptr(fd), "session-handoff")
	if f == nil {
		return fmt.Errorf("%w: invalid session handoff fd", ErrProvisioning)
	}
	// Do not close caller-owned FD.
	line := hex.EncodeToString(sk) + "\n"
	_, err := f.Write([]byte(line))
	return err
}

func nowUnix() int64 {
	return nowUTC().Unix()
}
