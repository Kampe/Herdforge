package toolprobe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Receipt is a signed, versioned proof that a surface did (or did not) execute
// a real tool for the bound identity.
type Receipt struct {
	SchemaVersion int       `json:"schema_version"`
	Identity      Identity  `json:"identity"`
	Status        Status    `json:"status"`
	Reason        string    `json:"reason,omitempty"`
	ProbedAt      time.Time `json:"probed_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	// ArtifactProof is content evidence (e.g. sha256 of sentinel bytes).
	ArtifactProof string `json:"artifact_proof,omitempty"`
	// Signature is sha256 over the canonical receipt body (excludes Signature).
	Signature string `json:"signature"`
}

// DefaultTTL is how long a PASS/INCAPABLE receipt remains authoritative.
const DefaultTTL = 6 * time.Hour

// Fresh reports whether the receipt is still within its TTL at now.
func (r Receipt) Fresh(now time.Time) bool {
	if r.SchemaVersion != SchemaVersion {
		return false
	}
	if r.ProbedAt.IsZero() || r.ExpiresAt.IsZero() {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return !now.Before(r.ProbedAt.UTC()) && now.Before(r.ExpiresAt.UTC())
}

// Passes is true only for a schema-current, signed, unexpired PASS.
func (r Receipt) Passes(now time.Time) bool {
	if !r.Status.WriteCapable() || !r.Fresh(now) {
		return false
	}
	return r.VerifySignature() == nil
}

// Canonical bytes exclude Signature so Sign/Verify share one form.
func (r Receipt) canonical() ([]byte, error) {
	body := struct {
		SchemaVersion int       `json:"schema_version"`
		Identity      Identity  `json:"identity"`
		Status        Status    `json:"status"`
		Reason        string    `json:"reason,omitempty"`
		ProbedAt      time.Time `json:"probed_at"`
		ExpiresAt     time.Time `json:"expires_at"`
		ArtifactProof string    `json:"artifact_proof,omitempty"`
	}{
		SchemaVersion: r.SchemaVersion,
		Identity:      r.Identity,
		Status:        r.Status,
		Reason:        r.Reason,
		ProbedAt:      r.ProbedAt.UTC(),
		ExpiresAt:     r.ExpiresAt.UTC(),
		ArtifactProof: r.ArtifactProof,
	}
	return json.Marshal(body)
}

// Sign sets Signature from the canonical body. Call after filling all fields.
func (r *Receipt) Sign() error {
	if r == nil {
		return fmt.Errorf("toolprobe: nil receipt")
	}
	if err := r.Identity.Valid(); err != nil {
		return err
	}
	if !r.Status.Valid() {
		return fmt.Errorf("toolprobe: invalid status %q", r.Status)
	}
	if r.SchemaVersion == 0 {
		r.SchemaVersion = SchemaVersion
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("toolprobe: unsupported schema version %d", r.SchemaVersion)
	}
	b, err := r.canonical()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	r.Signature = "sha256:" + hex.EncodeToString(sum[:])
	return nil
}

// VerifySignature recomputes the canonical digest.
func (r Receipt) VerifySignature() error {
	if !strings.HasPrefix(r.Signature, "sha256:") {
		return fmt.Errorf("toolprobe: missing signature")
	}
	b, err := r.canonical()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if r.Signature != want {
		return fmt.Errorf("toolprobe: signature mismatch")
	}
	return nil
}

// NewReceipt builds and signs a receipt for id with the given status.
func NewReceipt(id Identity, status Status, reason, artifactProof string, now time.Time, ttl time.Duration) (Receipt, error) {
	if err := id.Valid(); err != nil {
		return Receipt{}, err
	}
	if !status.Valid() {
		return Receipt{}, fmt.Errorf("toolprobe: invalid status %q", status)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	r := Receipt{
		SchemaVersion: SchemaVersion,
		Identity:      id,
		Status:        status,
		Reason:        reason,
		ProbedAt:      now.UTC(),
		ExpiresAt:     now.UTC().Add(ttl),
		ArtifactProof: artifactProof,
	}
	if err := r.Sign(); err != nil {
		return Receipt{}, err
	}
	return r, nil
}
