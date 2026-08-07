package confinement

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SecretEnv is the preferred production secret for confinement MACs.
const SecretEnv = "HERD_CONFINEMENT_SECRET"

// SecretEnvFallback is accepted so coordinators that already mint control
// envelopes can reuse the same host secret without a second key material.
const SecretEnvFallback = "HERD_CONTROL_SECRET"

// ErrMissingSecret is returned when production issuer construction has no key.
var ErrMissingSecret = errors.New("confinement: HERD_CONFINEMENT_SECRET (or HERD_CONTROL_SECRET) is required")

// HMACIssuer is the production MAC authority. It signs the complete AuthTuple
// plus canonical root/sentinel so a capability cannot be forged from strings.
type HMACIssuer struct {
	secret []byte
}

// NewHMACIssuer builds an issuer from raw secret bytes. Empty secrets fail closed.
func NewHMACIssuer(secret []byte) (*HMACIssuer, error) {
	if len(secret) == 0 {
		return nil, ErrMissingSecret
	}
	// Copy so callers cannot mutate the live key through the slice they passed.
	dup := make([]byte, len(secret))
	copy(dup, secret)
	return &HMACIssuer{secret: dup}, nil
}

// IssuerFromEnv loads the production issuer from the process environment.
func IssuerFromEnv() (*HMACIssuer, error) {
	secret := strings.TrimSpace(os.Getenv(SecretEnv))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv(SecretEnvFallback))
	}
	if secret == "" {
		return nil, ErrMissingSecret
	}
	return NewHMACIssuer([]byte(secret))
}

// Issue returns a MAC over the canonical confinement binding and a fresh nonce.
func (i *HMACIssuer) Issue(root, sentinel string, tuple AuthTuple) (IssuerProof, error) {
	if i == nil || len(i.secret) == 0 {
		return IssuerProof{}, ErrUnauthenticated
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return IssuerProof{}, fmt.Errorf("confinement: nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	mac := i.mac(root, sentinel, nonce, tuple)
	return IssuerProof{MAC: mac, Nonce: nonce}, nil
}

// Verify checks that proof was issued for the exact root/sentinel/tuple.
func (i *HMACIssuer) Verify(root, sentinel string, tuple AuthTuple, proof IssuerProof) error {
	if i == nil || len(i.secret) == 0 || len(proof.MAC) == 0 || proof.Nonce == "" {
		return ErrUnauthenticated
	}
	want := i.mac(root, sentinel, proof.Nonce, tuple)
	if !hmac.Equal(want, proof.MAC) {
		return ErrUnauthenticated
	}
	return nil
}

func (i *HMACIssuer) mac(root, sentinel, nonce string, tuple AuthTuple) []byte {
	parts := []string{
		root, sentinel, nonce,
		tuple.Repository, tuple.Task, tuple.LeaseID, tuple.Lane,
		tuple.Session, tuple.SessionGeneration, tuple.HerdrTab, tuple.HerdrPane,
		tuple.ProcessIdentity, tuple.ArgvIdentity, tuple.PolicyDigest,
	}
	parts = append(parts, tuple.AllowedRoots...)
	h := hmac.New(sha256.New, i.secret)
	_, _ = h.Write([]byte(strings.Join(parts, "\x00")))
	return h.Sum(nil)
}
