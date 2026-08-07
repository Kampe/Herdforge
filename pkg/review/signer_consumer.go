package review

import (
	"fmt"

	"github.com/Kampe/Herdforge/pkg/signerboundary"
)

// SignerConsumer is the consumer-side API FAC-169 publishes for FAC-176 to wire
// into the review pipeline. It is NOT yet on any production path: nothing in
// ledger.go or pipeline.go calls it, so no verdict is signed through it today.
// Wiring it (and the ordering vs the review ledger) is FAC-176's work, which is
// blocked by this ticket.
//
// Contract when wired: the coordinator review path admits a durable grant, then
// requests an exact sign-verdict over the OS boundary. Builders never hold
// session material or peer-UID authorization.
type SignerConsumer struct {
	rs *signerboundary.ReviewerSigner
}

// NewSignerConsumer opens the reviewer signer (must run as HERD_REQUESTER_UID).
func NewSignerConsumer(keyDir, repoRoot, identity string) (*SignerConsumer, error) {
	rs, err := signerboundary.OpenReviewerSigner(signerboundary.Options{
		KeyDir: keyDir, RepoRoot: repoRoot, Identity: identity, RequireSeparateUID: true,
	})
	if err != nil {
		return nil, err
	}
	return &SignerConsumer{rs: rs}, nil
}

// AdmitAndSign records a durable grant then signs the admitted verdict.
// Intended coordinator path for FAC-145/FAC-169 signed receipts once FAC-176
// wires it; not reached by ledger.go or pipeline.go today.
func (c *SignerConsumer) AdmitAndSign(candidateSHA, baseSHA, patchID, verdict, sessionID string, payload []byte) ([]byte, error) {
	if c == nil || c.rs == nil {
		return nil, fmt.Errorf("review: signer consumer not open")
	}
	if _, err := c.rs.Admit(candidateSHA, baseSHA, patchID, verdict, sessionID, 0); err != nil {
		return nil, err
	}
	return c.rs.SignAdmittedVerdict(candidateSHA, baseSHA, patchID, verdict, sessionID, payload)
}

// SignLedgerVerdict is the call review recording will make once wired: binds exact
// SHA/verdict into a signed receipt for the ledger. Returns signature bytes.
func SignLedgerVerdict(keyDir, repoRoot, identity, candidateSHA, baseSHA, patchID, verdict, sessionID string, payload []byte) ([]byte, error) {
	c, err := NewSignerConsumer(keyDir, repoRoot, identity)
	if err != nil {
		return nil, err
	}
	return c.AdmitAndSign(candidateSHA, baseSHA, patchID, verdict, sessionID, payload)
}

// PublicKey returns the verification key for receipt consumers.
func (c *SignerConsumer) PublicKey() []byte {
	if c == nil || c.rs == nil {
		return nil
	}
	return c.rs.PublicKey()
}
