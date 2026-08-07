package signerboundary

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Attestation integrity is bound to the session key so a same-UID worker
// cannot mint AgentsExcluded=true by rewriting isolation.json (FAC-169 §6).
// ProfilePath is never absolute and never authoritative alone.

// signAttestation sets IntegrityMAC over canonical attestation fields.
func signAttestation(att *Attestation, key SessionKey) error {
	if len(key) < 16 {
		return fmt.Errorf("%w: session key required to bind attestation", ErrProvisioning)
	}
	// Never store absolute profile paths.
	if att.ProfilePath != "" && (strings.HasPrefix(att.ProfilePath, "/") || strings.Contains(att.ProfilePath, ":\\")) {
		att.ProfilePath = ""
	}
	// Socket paths in attestation are runtime — store basename only for portability.
	if att.SocketPath != "" {
		// Keep full path for dial but integrity covers mechanism+digest+uid+exe+pid
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(attCanonical(*att))
	att.IntegrityMAC = hex.EncodeToString(mac.Sum(nil))
	return nil
}

func verifyAttestationMAC(att Attestation, key SessionKey) error {
	if strings.TrimSpace(att.IntegrityMAC) == "" {
		return fmt.Errorf("%w: attestation missing integrity MAC", ErrBoundaryNotEstablished)
	}
	if len(key) < 16 {
		return fmt.Errorf("%w: session key required to verify attestation", ErrProvisioning)
	}
	want, err := hex.DecodeString(att.IntegrityMAC)
	if err != nil {
		return fmt.Errorf("%w: corrupt integrity MAC", ErrBoundaryNotEstablished)
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(attCanonical(att))
	got := mac.Sum(nil)
	if !hmac.Equal(want, got) {
		return fmt.Errorf("%w: attestation integrity MAC mismatch (worker-mutable JSON rejected)", ErrBoundaryNotEstablished)
	}
	return nil
}

func attCanonical(att Attestation) []byte {
	// Exclude IntegrityMAC from the preimage.
	type wire struct {
		Mechanism      string `json:"mechanism"`
		KeyOwnerUID    int    `json:"key_owner_uid"`
		AgentsExcluded bool   `json:"agents_excluded"`
		Platform       string `json:"platform"`
		SocketPath     string `json:"socket_path"`
		SignerPID      int    `json:"signer_pid"`
		AuthorizedExe  string `json:"authorized_exe"`
		ProbeDigest    string `json:"probe_digest"`
		ProfilePath    string `json:"profile_path"`
	}
	w := wire{
		Mechanism:      att.Mechanism,
		KeyOwnerUID:    att.KeyOwnerUID,
		AgentsExcluded: att.AgentsExcluded,
		Platform:       att.Platform,
		SocketPath:     att.SocketPath,
		SignerPID:      att.SignerPID,
		AuthorizedExe:  att.AuthorizedExe,
		ProbeDigest:    att.ProbeDigest,
		ProfilePath:    att.ProfilePath,
	}
	b, _ := json.Marshal(w)
	return b
}
