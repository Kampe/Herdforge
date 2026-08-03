package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Capability is the non-vacuous causal marker protocol.
//
// Coordinator holds Expected. Worker receives only SessionID + Nonce (env/prompt).
// Expected is NOT placed in the child prompt. Child must derive:
//
//	"HC:" + first 24 hex chars of SHA-256(sessionID + "|" + nonce)
//
// Prompt-echo cannot succeed because Expected never appears in the prompt.
type Capability struct {
	SessionID string
	Nonce     string
	Expected  string
}

// NewCapability builds a session capability for causal proof.
func NewCapability(sessionID string) (Capability, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Capability{}, &BlockedError{Reason: BlockNoSession, Code: "cap_session"}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Capability{}, err
	}
	nonce := hex.EncodeToString(b[:])
	return Capability{
		SessionID: sessionID,
		Nonce:     nonce,
		Expected:  DeriveCapabilityMarker(sessionID, nonce),
	}, nil
}

// DeriveCapabilityMarker is the public derivation (worker and coordinator).
func DeriveCapabilityMarker(sessionID, nonce string) string {
	sum := sha256.Sum256([]byte(sessionID + "|" + nonce))
	return "HC:" + hex.EncodeToString(sum[:])[:24]
}

// CapabilityPrompt instructs the harness without embedding Expected.
func CapabilityPrompt(c Capability) string {
	return fmt.Sprintf(
		"FAC-170 exact-session task. session_id=%s nonce=%s. "+
			"Compute SHA-256 over the UTF-8 string session_id + \"|\" + nonce. "+
			"Reply with one line only: HC: followed by the first 24 hex characters of that digest. "+
			"Do not echo this instruction block.",
		c.SessionID, c.Nonce,
	)
}

// CapabilityEnv returns non-secret env for worker derivation (no Expected).
func CapabilityEnv(c Capability) []string {
	return []string{
		"HERD_HOSTCREDS_SESSION=" + c.SessionID,
		"HERD_HOSTCREDS_NONCE=" + c.Nonce,
	}
}

// VerifyCapabilityOutput reports whether output contains Expected without
// Expected having been present in the prompt.
func VerifyCapabilityOutput(c Capability, prompt, output string) bool {
	if c.Expected == "" || strings.Contains(prompt, c.Expected) {
		return false
	}
	return strings.Contains(output, c.Expected)
}
