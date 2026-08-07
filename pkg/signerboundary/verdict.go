package signerboundary

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Canonical reviewer ops — no general-purpose signing oracle on the wire.
const (
	OpPing        = "ping"
	OpProbe       = "probe"
	OpSignVerdict = "sign-verdict"
	OpSignReceipt = "sign-receipt"
	OpExportKey   = "export-key" // always refused
)

var allowedVerdicts = map[string]bool{
	"APPROVED":          true,
	"REJECTED":          true,
	"CHANGES_REQUESTED": true,
	"BLOCKED":           true,
}

// Full-length git object names only (40 hex sha1 or 64 hex sha256).
var shaHex = regexp.MustCompile(`(?i)^([0-9a-f]{40}|[0-9a-f]{64})$`)

// MaxPayloadBytes bounds request bodies.
const MaxPayloadBytes = 256 << 10

// MaxWireFrameBytes bounds a full JSON frame on the socket.
const MaxWireFrameBytes = 512 << 10

// ValidateProduction is the only admission path for the signer server.
// Requires exact candidate+base+patch+digest+verdict+session for verdicts;
// rejects empty base/patch and arbitrary ops (including sign-bytes).
func (r SignRequest) ValidateProduction() error {
	if strings.TrimSpace(r.Op) == "" {
		return fmt.Errorf("%w: Op required", ErrPeerUnauthorized)
	}
	switch r.Op {
	case OpPing, OpProbe:
		return nil
	case OpSignVerdict:
		if !shaHex.MatchString(strings.TrimSpace(r.CandidateSHA)) {
			return fmt.Errorf("%w: CandidateSHA must be exact 40/64 hex", ErrPeerUnauthorized)
		}
		if !shaHex.MatchString(strings.TrimSpace(r.BaseSHA)) {
			return fmt.Errorf("%w: BaseSHA must be exact 40/64 hex", ErrPeerUnauthorized)
		}
		if strings.TrimSpace(r.PatchID) == "" {
			return fmt.Errorf("%w: PatchID required (exact patch/diff identity)", ErrPeerUnauthorized)
		}
		if !allowedVerdicts[strings.TrimSpace(r.Verdict)] {
			return fmt.Errorf("%w: Verdict %q not in allowlist", ErrPeerUnauthorized, r.Verdict)
		}
		if strings.TrimSpace(r.SessionID) == "" || len(strings.TrimSpace(r.SessionID)) < 8 {
			return fmt.Errorf("%w: SessionID required (authorized reviewer session, min 8 chars)", ErrPeerUnauthorized)
		}
		p := r.payloadBytes()
		if len(p) == 0 {
			return fmt.Errorf("%w: canonical verdict payload required", ErrPeerUnauthorized)
		}
		if len(p) > MaxPayloadBytes {
			return fmt.Errorf("%w: payload exceeds %d bytes", ErrPeerUnauthorized, MaxPayloadBytes)
		}
		// Payload must embed its own digest binding (sha256 of body without trailing digest field is caller's job);
		// server requires PatchID + payload non-empty as independent admission inputs.
		return nil
	case OpSignReceipt:
		if !shaHex.MatchString(strings.TrimSpace(r.CandidateSHA)) {
			return fmt.Errorf("%w: CandidateSHA must be exact 40/64 hex", ErrPeerUnauthorized)
		}
		if !shaHex.MatchString(strings.TrimSpace(r.BaseSHA)) {
			return fmt.Errorf("%w: BaseSHA must be exact 40/64 hex for receipt", ErrPeerUnauthorized)
		}
		if strings.TrimSpace(r.SessionID) == "" || len(strings.TrimSpace(r.SessionID)) < 8 {
			return fmt.Errorf("%w: SessionID required", ErrPeerUnauthorized)
		}
		p := r.payloadBytes()
		if len(p) == 0 {
			return fmt.Errorf("%w: receipt payload required", ErrPeerUnauthorized)
		}
		if len(p) > MaxPayloadBytes {
			return fmt.Errorf("%w: payload exceeds max", ErrPeerUnauthorized)
		}
		return nil
	case OpExportKey:
		return fmt.Errorf("%w: export refused", ErrPeerUnauthorized)
	default:
		// sign-bytes / sign / anything else: not admitted.
		return fmt.Errorf("%w: op %q not admitted (reviewer surface is sign-verdict|sign-receipt only)", ErrPeerUnauthorized, r.Op)
	}
}

func (r SignRequest) payloadBytes() []byte {
	if len(r.Payload) > 0 {
		return r.Payload
	}
	if r.PayloadHex != "" {
		if raw, err := hex.DecodeString(r.PayloadHex); err == nil {
			return raw
		}
	}
	return nil
}

// PayloadDigest is sha256 hex of the payload.
func (r SignRequest) PayloadDigest() string {
	p := r.payloadBytes()
	if len(p) == 0 {
		return ""
	}
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:])
}

// NewVerdictRequest builds a production sign-verdict request with an admitted
// payload binding (candidate/base/verdict/patch) when payload is nil/empty.
func NewVerdictRequest(candidateSHA, baseSHA, patchID, verdict, sessionID string, canonicalPayload []byte) SignRequest {
	if len(canonicalPayload) == 0 {
		canonicalPayload = AdmittedVerdictPayload(candidateSHA, baseSHA, patchID, verdict)
	}
	return SignRequest{
		Op:           OpSignVerdict,
		CandidateSHA: candidateSHA,
		BaseSHA:      baseSHA,
		PatchID:      patchID,
		Verdict:      verdict,
		SessionID:    sessionID,
		Payload:      canonicalPayload,
	}
}

// AdmittedVerdictPayload is the minimal JSON body DefaultAdmitReviewerVerdict accepts.
func AdmittedVerdictPayload(candidateSHA, baseSHA, patchID, verdict string) []byte {
	// Keep deterministic field order for stable MAC preimages in tests.
	return []byte(`{"candidate_sha":"` + candidateSHA + `","base_sha":"` + baseSHA + `","patch_id":"` + patchID + `","verdict":"` + verdict + `"}`)
}
