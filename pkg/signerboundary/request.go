package signerboundary

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// SignRequest is the only authorized signing input. A bare byte blob is
// rejected: the server binds peer credentials to these exact fields and
// refuses replayed nonces (FAC-169 §7 — "different token alone" is not enough).
type SignRequest struct {
	// Op classifies the signature purpose (receipt, verdict, journal, …).
	Op string `json:"op"`
	// CandidateSHA is the exact commit under review (required for verdict/receipt).
	CandidateSHA string `json:"candidate_sha"`
	// BaseSHA is the immutable base the candidate was branched from.
	BaseSHA string `json:"base_sha"`
	// PatchID is an optional patch/diff identity (empty allowed when not patch-scoped).
	PatchID string `json:"patch_id,omitempty"`
	// Verdict is the decision string when Op is verdict-like (APPROVED/REJECTED/…).
	Verdict string `json:"verdict,omitempty"`
	// SessionID is the agent/coordinator session that may not be forged by workers.
	SessionID string `json:"session_id"`
	// Nonce is a unique anti-replay value (generated if empty on the client).
	Nonce string `json:"nonce"`
	// Payload is the canonical body bytes being signed (receipt JSON, etc.).
	Payload []byte `json:"-"`
	// PayloadHex is wire form of Payload.
	PayloadHex string `json:"payload_hex,omitempty"`
}

// Validate delegates to ValidateProduction (only admission surface).
func (r SignRequest) Validate() error {
	return r.ValidateProduction()
}

// Canonical binds all authorization fields for MAC / signature preimage.
func (r SignRequest) Canonical() []byte {
	var b strings.Builder
	b.WriteString(r.Op)
	b.WriteByte(0)
	b.WriteString(r.CandidateSHA)
	b.WriteByte(0)
	b.WriteString(r.BaseSHA)
	b.WriteByte(0)
	b.WriteString(r.PatchID)
	b.WriteByte(0)
	b.WriteString(r.Verdict)
	b.WriteByte(0)
	b.WriteString(r.SessionID)
	b.WriteByte(0)
	b.WriteString(r.Nonce)
	b.WriteByte(0)
	payload := r.Payload
	if len(payload) == 0 && r.PayloadHex != "" {
		if raw, err := hex.DecodeString(r.PayloadHex); err == nil {
			payload = raw
		}
	}
	b.Write(payload)
	return []byte(b.String())
}

// ProbeReceipt is an unambiguous structured proof record for live probes.
type ProbeReceipt struct {
	Version       int    `json:"v"`
	Platform      string `json:"platform"`
	Operation     string `json:"operation"`
	OK            bool   `json:"ok"`
	ExpectedErrno string `json:"expected_errno,omitempty"`
	ObservedErrno string `json:"observed_errno,omitempty"`
	SignerPID     int    `json:"signer_pid,omitempty"`
	SignerUID     int    `json:"signer_uid,omitempty"`
	Detail        string `json:"detail,omitempty"`
	RulesetABI    string `json:"ruleset_abi,omitempty"`
	RestrictSelf  bool   `json:"restrict_self,omitempty"`
}

// errnoToken matches ObservedErrno against ExpectedErrno tokens (pipe-separated).
func errnoMatches(expected, observed string) bool {
	if expected == "" {
		return true // ops that do not assert errno
	}
	obs := strings.ToLower(observed)
	for _, tok := range strings.Split(expected, "|") {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		if strings.Contains(obs, tok) {
			return true
		}
	}
	return false
}

// EncodeProbeDigest builds the attestation probe string only when every receipt
// is OK and ObservedErrno matches ExpectedErrno where set (FAC-169 §b).
func EncodeProbeDigest(receipts []ProbeReceipt) (string, error) {
	var parts []string
	need := map[string]bool{
		ProbeKeyUnreadable:    false,
		ProbeAttachDenied:     false,
		ProbeIPCAuthDenied:    false,
		ProbeAuthorizedSignOK: false,
		ProbePathHardened:     false,
		ProbeKeyNonExport:     false,
	}
	for _, r := range receipts {
		if r.Version != 1 {
			return "", fmt.Errorf("%w: probe receipt version", ErrProvisioning)
		}
		if !r.OK {
			return "", fmt.Errorf("%w: probe %s not ok: %s", ErrAdversarialSuccess, r.Operation, r.Detail)
		}
		if r.ExpectedErrno != "" && !errnoMatches(r.ExpectedErrno, r.ObservedErrno) {
			return "", fmt.Errorf("%w: probe %s ObservedErrno %q does not match ExpectedErrno %q",
				ErrProvisioning, r.Operation, r.ObservedErrno, r.ExpectedErrno)
		}
		switch r.Operation {
		case "key-read":
			need[ProbeKeyUnreadable] = true
			parts = append(parts, ProbeKeyUnreadable)
		case "attach":
			if r.SignerPID <= 0 || r.SignerUID <= 0 {
				return "", fmt.Errorf("%w: attach receipt missing live signer pid/uid", ErrProvisioning)
			}
			need[ProbeAttachDenied] = true
			parts = append(parts, ProbeAttachDenied)
		case "ipc-unauth":
			need[ProbeIPCAuthDenied] = true
			parts = append(parts, ProbeIPCAuthDenied)
		case "ipc-auth":
			need[ProbeAuthorizedSignOK] = true
			parts = append(parts, ProbeAuthorizedSignOK)
		case "path-harden":
			need[ProbePathHardened] = true
			parts = append(parts, ProbePathHardened)
		case "key-non-export":
			need[ProbeKeyNonExport] = true
			parts = append(parts, ProbeKeyNonExport)
		default:
			return "", fmt.Errorf("%w: unknown probe operation %q", ErrProvisioning, r.Operation)
		}
	}
	for k, ok := range need {
		if !ok {
			return "", fmt.Errorf("%w: missing structured probe %s", ErrBoundaryNotEstablished, k)
		}
	}
	return strings.Join(parts, ","), nil
}
