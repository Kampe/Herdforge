package signerboundary

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// SessionKey binds request fields for anti-replay/integrity. It is NOT the
// OS authorization boundary: peer UID topology is. Session material must not
// be worker-readable; with R!=B, it lives only in the requester process.
type SessionKey []byte

// NewSessionKey returns a 32-byte random session key.
func NewSessionKey() (SessionKey, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return SessionKey(b), nil
}

// BindRequestMAC covers the full SignRequest canonical form.
func (k SessionKey) BindRequestMAC(r SignRequest) string {
	m := hmac.New(sha256.New, []byte(k))
	_, _ = m.Write(r.Canonical())
	return hex.EncodeToString(m.Sum(nil))
}

// CheckRequestMAC validates BindRequestMAC.
func (k SessionKey) CheckRequestMAC(r SignRequest, macHex string) bool {
	want, err := hex.DecodeString(macHex)
	if err != nil {
		return false
	}
	got, err := hex.DecodeString(k.BindRequestMAC(r))
	if err != nil {
		return false
	}
	return hmac.Equal(want, got)
}

// EnsureNonce fills Nonce if empty.
func (r *SignRequest) EnsureNonce() error {
	if r.Nonce != "" {
		return nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	r.Nonce = hex.EncodeToString(buf)
	return nil
}

// SignWithKey is used only inside the signer process (key never exported).
func SignWithKey(priv ed25519.PrivateKey, msg []byte) ([]byte, error) {
	if len(priv) == 0 {
		return nil, ErrBoundaryNotEstablished
	}
	return ed25519.Sign(priv, msg), nil
}

// RefuseExport documents that private key bytes must never be returned over IPC.
func RefuseExport() error {
	return fmt.Errorf("signerboundary: private key export refused (non-exportable)")
}

// AgentEnvForbidden is NOT authority (FAC-169: do not use HERD_ROLE as auth).
// Retained only as a defensive diagnostic.
func AgentEnvForbidden() bool {
	return os.Getenv("HERD_ROLE") == "agent"
}
