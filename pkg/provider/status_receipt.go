package provider

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// StatusMutationReceipt is an authenticated binding of one status mutation to
// exact op, task, status, fence, actor, and base revision.
//
// On real Kaneo (live-probed 2026-08-03): custom JSON keys such as
// herdStatusReceipt are NOT persisted. The only proven atomic multi-field
// write is PUT /api/task/{id} with required schema fields including both
// status and description. The signed receipt is therefore embedded in the
// description footer of that same PUT — not a second comment, not an
// ignored custom field.
//
// Verification uses constant-time HMAC + field equality, never substring tags.
type StatusMutationReceipt struct {
	OpID         string `json:"op_id"`
	TaskID       string `json:"task_id"`
	Status       string `json:"status"`
	FenceToken   int64  `json:"fence_token"`
	BaseRevision string `json:"base_revision"`
	Actor        string `json:"actor"`
	Nonce        string `json:"nonce"`
	IssuedAtUnix int64  `json:"issued_at_unix"`
	MAC          string `json:"mac"`
}

const (
	// envFenceHMACKey is the sole production secret for status-receipt MACs.
	// Never derive from HERD_FENCE_VOLUME_ID: that identity is written into
	// mode-0644 SHARED and is not an HMAC secret.
	envFenceHMACKey = "HERD_FENCE_HMAC_KEY"
	// Max clock skew for IssuedAtUnix (replay bound).
	statusReceiptMaxAge = 24 * time.Hour

	statusReceiptFooterOpen  = "<!-- herd-status-receipt-v1:"
	statusReceiptFooterClose = "-->"
)

// StatusReceiptKey returns the HMAC key for status receipts.
// Production: HERD_FENCE_HMAC_KEY only (min 16 chars).
// Tests: fixed key when env unset (compiled binary must set the env).
func StatusReceiptKey() ([]byte, error) {
	if k := strings.TrimSpace(os.Getenv(envFenceHMACKey)); k != "" {
		if len(k) < 16 {
			return nil, fmt.Errorf("provider: %s too short (min 16)", envFenceHMACKey)
		}
		return []byte(k), nil
	}
	if testing.Testing() {
		sum := sha256.Sum256([]byte("herd-status-receipt-test-key-v1"))
		return sum[:], nil
	}
	return nil, fmt.Errorf("provider: %s required to sign status receipts (never reuse HERD_FENCE_VOLUME_ID; it is written to SHARED)", envFenceHMACKey)
}

func receiptCanonical(r StatusMutationReceipt) string {
	return strings.Join([]string{
		r.OpID,
		r.TaskID,
		NormalizeStatus(r.Status),
		strconv.FormatInt(r.FenceToken, 10),
		r.BaseRevision,
		r.Actor,
		r.Nonce,
		strconv.FormatInt(r.IssuedAtUnix, 10),
	}, "\x1f")
}

// MintStatusReceipt builds and signs a receipt for an imminent status mutate.
func MintStatusReceipt(taskID, opID, status, baseRev, actor string, fence int64) (*StatusMutationReceipt, error) {
	if taskID == "" || opID == "" || status == "" || fence <= 0 {
		return nil, fmt.Errorf("provider: status receipt requires task, op, status, fence")
	}
	key, err := StatusReceiptKey()
	if err != nil {
		return nil, err
	}
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return nil, err
	}
	r := StatusMutationReceipt{
		OpID:         opID,
		TaskID:       taskID,
		Status:       NormalizeStatus(status),
		FenceToken:   fence,
		BaseRevision: baseRev,
		Actor:        actor,
		Nonce:        hex.EncodeToString(nb[:]),
		IssuedAtUnix: time.Now().UTC().Unix(),
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(receiptCanonical(r)))
	r.MAC = hex.EncodeToString(mac.Sum(nil))
	return &r, nil
}

// VerifyStatusReceipt checks MAC and field binding (constant-time compare).
func VerifyStatusReceipt(r *StatusMutationReceipt, wantTask, wantOp, wantStatus string, wantFence int64) error {
	if r == nil {
		return fmt.Errorf("provider: nil status receipt")
	}
	key, err := StatusReceiptKey()
	if err != nil {
		return err
	}
	if r.OpID != wantOp || r.TaskID != wantTask {
		return fmt.Errorf("provider: receipt op/task mismatch")
	}
	if NormalizeStatus(r.Status) != NormalizeStatus(wantStatus) {
		return fmt.Errorf("provider: receipt status mismatch")
	}
	if wantFence > 0 && r.FenceToken != wantFence {
		return fmt.Errorf("provider: receipt fence mismatch")
	}
	if r.Nonce == "" || r.MAC == "" {
		return fmt.Errorf("provider: receipt missing nonce/mac")
	}
	now := time.Now().UTC().Unix()
	if r.IssuedAtUnix > now+60 || now-r.IssuedAtUnix > int64(statusReceiptMaxAge.Seconds()) {
		return fmt.Errorf("provider: receipt issued_at out of range")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(receiptCanonical(*r)))
	wantMAC := mac.Sum(nil)
	got, err := hex.DecodeString(r.MAC)
	if err != nil || !hmac.Equal(wantMAC, got) {
		return fmt.Errorf("provider: status receipt MAC invalid (forged or wrong key)")
	}
	return nil
}

// EncodeStatusReceiptJSON serializes the receipt for description embedding.
func EncodeStatusReceiptJSON(r *StatusMutationReceipt) (string, error) {
	if r == nil {
		return "", fmt.Errorf("nil receipt")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeStatusReceiptJSON parses a receipt from description readback.
func DecodeStatusReceiptJSON(s string) (*StatusMutationReceipt, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty receipt")
	}
	var r StatusMutationReceipt
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// MatchStatusOpEvidence is deliberately always false: forgeable substring
// tags ([herd-status-op:...]) are never authentic evidence (FAC-147 lm0ihu).
func MatchStatusOpEvidence(liveBody, opID, status string) bool {
	_ = liveBody
	_ = opID
	_ = status
	return false
}

// StatusOpEvidenceBody is empty: never dual-write post-status comment tags.
func StatusOpEvidenceBody(opID, status string) string {
	_ = opID
	_ = status
	return ""
}

// TaskHasVerifiedStatusReceipt reports whether the task carries a signed
// receipt (from description footer readback) for the exact binding.
func TaskHasVerifiedStatusReceipt(t *Task, wantTask, wantOp, wantStatus string, wantFence int64) bool {
	if t == nil {
		return false
	}
	if NormalizeStatus(t.Status) != NormalizeStatus(wantStatus) {
		return false
	}
	receipt := strings.TrimSpace(t.StatusReceipt)
	if receipt == "" {
		receipt = ParseStatusReceiptFromDescription(t.Description)
	}
	if receipt == "" {
		return false
	}
	r, err := DecodeStatusReceiptJSON(receipt)
	if err != nil {
		return false
	}
	return VerifyStatusReceipt(r, wantTask, wantOp, wantStatus, wantFence) == nil
}

// ParseStatusReceiptFromDescription extracts an embedded receipt footer.
// Format (last occurrence wins): <!-- herd-status-receipt-v1: <json> -->
func ParseStatusReceiptFromDescription(desc string) string {
	idx := strings.LastIndex(desc, statusReceiptFooterOpen)
	if idx < 0 {
		return ""
	}
	rest := desc[idx+len(statusReceiptFooterOpen):]
	end := strings.Index(rest, statusReceiptFooterClose)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// EmbedStatusReceiptInDescription replaces any prior receipt footer and
// appends the signed receipt so a single PUT of description+status is one
// atomic remote effect on real Kaneo.
func EmbedStatusReceiptInDescription(desc, receiptJSON string) string {
	for {
		idx := strings.LastIndex(desc, statusReceiptFooterOpen)
		if idx < 0 {
			break
		}
		rest := desc[idx:]
		endRel := strings.Index(rest, statusReceiptFooterClose)
		if endRel < 0 {
			desc = strings.TrimRight(desc[:idx], "\n \t")
			break
		}
		desc = strings.TrimRight(desc[:idx], "\n \t")
	}
	footer := statusReceiptFooterOpen + " " + receiptJSON + " " + statusReceiptFooterClose
	if desc == "" {
		return footer
	}
	return desc + "\n\n" + footer
}

// StripStatusReceiptFooter returns description without receipt footers
// (for human-facing display / re-embed).
func StripStatusReceiptFooter(desc string) string {
	for {
		idx := strings.LastIndex(desc, statusReceiptFooterOpen)
		if idx < 0 {
			return strings.TrimRight(desc, "\n \t")
		}
		rest := desc[idx:]
		endRel := strings.Index(rest, statusReceiptFooterClose)
		if endRel < 0 {
			return strings.TrimRight(desc[:idx], "\n \t")
		}
		desc = strings.TrimRight(desc[:idx], "\n \t")
	}
}
