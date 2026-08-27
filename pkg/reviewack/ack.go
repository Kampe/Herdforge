// Package reviewack is the FAC-586 durable acknowledgment that canonical
// review-ingest admitted a transported verdict. Remote-ref durability
// (verdict-push) and ledger admission are distinct claims: a review host must
// not retire a resident on transport alone.
package reviewack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DirRel is the repo-relative durable ack store on the ledger host.
	DirRel = ".herd/review/acks"
	// ConsumedDirRel records successful consumes for idempotency.
	ConsumedDirRel = ".herd/review/acks-consumed"
)

// Ack is bound to the exact candidate SHA, the reviewer launch identity, and
// the SHA-256 digest of the admitted artifact bytes.
type Ack struct {
	SHA              string `json:"sha"`
	Reviewer         string `json:"reviewer"`
	ArtifactDigest   string `json:"artifact_digest"`
	AdmittedAt       string `json:"admitted_at"`
	LaunchIdentity   string `json:"launch_identity"`
	SchemaVersion    int    `json:"schema_version"`
}

// ConsumeResult is the structured gate a review host must reason over before
// retiring a resident. Callers gate on OK; Reason/Layer are diagnostic.
type ConsumeResult struct {
	OK     bool
	Layer  string
	Reason string
	Ack    *Ack
}

var (
	ErrMissing      = errors.New("reviewack: acknowledgment missing")
	ErrMismatch     = errors.New("reviewack: acknowledgment mismatch")
	ErrAmbiguous    = errors.New("reviewack: acknowledgment ambiguous")
	ErrStale        = errors.New("reviewack: acknowledgment stale")
)

// ArtifactDigest returns the full SHA-256 hex of the verdict artifact bytes.
func ArtifactDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// FileName is the durable ack basename for one (sha, reviewer) pair.
func FileName(sha, reviewer string) string {
	return fmt.Sprintf("%s-%s.json", shortSHA(sha), sanitize(reviewer))
}

// Path joins root with the ack file for (sha, reviewer).
func Path(root, sha, reviewer string) string {
	return filepath.Join(root, DirRel, FileName(sha, reviewer))
}

// Emit writes a durable ack for a successful canonical admission. Identical
// re-emits for the same digest are idempotent (no error). A different digest
// for the same (sha, reviewer) fails closed as ambiguous.
func Emit(root string, ack Ack) error {
	root = strings.TrimSpace(root)
	ack.SHA = strings.TrimSpace(ack.SHA)
	ack.Reviewer = strings.TrimSpace(ack.Reviewer)
	ack.ArtifactDigest = strings.ToLower(strings.TrimSpace(ack.ArtifactDigest))
	ack.LaunchIdentity = strings.TrimSpace(ack.LaunchIdentity)
	if ack.LaunchIdentity == "" {
		ack.LaunchIdentity = ack.Reviewer
	}
	if root == "" || len(ack.SHA) != 40 || ack.Reviewer == "" || ack.ArtifactDigest == "" {
		return fmt.Errorf("reviewack: root, 40-char sha, reviewer, and artifact_digest are required")
	}
	if ack.SchemaVersion == 0 {
		ack.SchemaVersion = 1
	}
	if ack.AdmittedAt == "" {
		ack.AdmittedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	dir := filepath.Join(root, DirRel)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("reviewack: mkdir: %w", err)
	}
	path := Path(root, ack.SHA, ack.Reviewer)
	if existing, err := os.ReadFile(path); err == nil {
		var prev Ack
		if json.Unmarshal(existing, &prev) == nil {
			if strings.EqualFold(prev.ArtifactDigest, ack.ArtifactDigest) &&
				strings.EqualFold(prev.SHA, ack.SHA) &&
				prev.Reviewer == ack.Reviewer {
				return nil // idempotent re-emit
			}
			return fmt.Errorf("%w: existing ack digest=%s new=%s", ErrAmbiguous, prev.ArtifactDigest, ack.ArtifactDigest)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reviewack: inspect existing: %w", err)
	}
	body, err := json.MarshalIndent(ack, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("reviewack: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("reviewack: rename: %w", err)
	}
	return nil
}

// Consume verifies a durable ack matches the caller's expected bindings and
// records consumption. Duplicate consume of the same binding is OK.
//
// launchIdentity must equal the ack's LaunchIdentity (normally the exact
// reviewer agent name). A missing ack, digest/sha/reviewer mismatch, or
// identity mismatch retains the resident (OK=false) with a structured layer.
func Consume(root, sha, reviewer, wantDigest, launchIdentity string) ConsumeResult {
	root = strings.TrimSpace(root)
	sha = strings.TrimSpace(sha)
	reviewer = strings.TrimSpace(reviewer)
	wantDigest = strings.ToLower(strings.TrimSpace(wantDigest))
	launchIdentity = strings.TrimSpace(launchIdentity)
	if launchIdentity == "" {
		launchIdentity = reviewer
	}
	layer := "ingest_ack"
	if root == "" || len(sha) != 40 || reviewer == "" || wantDigest == "" {
		return ConsumeResult{OK: false, Layer: layer, Reason: "consume requires root, 40-char sha, reviewer, and artifact digest"}
	}

	// Idempotent: prior successful consume of this exact binding.
	if consumed, _ := os.ReadFile(consumedPath(root, sha, reviewer)); len(consumed) > 0 {
		var prev Ack
		if json.Unmarshal(consumed, &prev) == nil &&
			strings.EqualFold(prev.ArtifactDigest, wantDigest) &&
			strings.EqualFold(prev.SHA, sha) &&
			prev.Reviewer == reviewer &&
			prev.LaunchIdentity == launchIdentity {
			return ConsumeResult{OK: true, Layer: layer, Reason: "acknowledgment already consumed (idempotent)", Ack: &prev}
		}
	}

	path := Path(root, sha, reviewer)
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ConsumeResult{OK: false, Layer: layer, Reason: ErrMissing.Error() + "; canonical ledger admission not acknowledged"}
		}
		return ConsumeResult{OK: false, Layer: layer, Reason: "read acknowledgment: " + err.Error()}
	}
	var ack Ack
	if err := json.Unmarshal(body, &ack); err != nil {
		return ConsumeResult{OK: false, Layer: layer, Reason: "acknowledgment corrupt: " + err.Error()}
	}
	ack.SHA = strings.TrimSpace(ack.SHA)
	ack.Reviewer = strings.TrimSpace(ack.Reviewer)
	ack.ArtifactDigest = strings.ToLower(strings.TrimSpace(ack.ArtifactDigest))
	ack.LaunchIdentity = strings.TrimSpace(ack.LaunchIdentity)
	if ack.LaunchIdentity == "" {
		ack.LaunchIdentity = ack.Reviewer
	}

	switch {
	case !strings.EqualFold(ack.SHA, sha):
		return ConsumeResult{OK: false, Layer: layer, Reason: fmt.Sprintf("%v: sha got=%s want=%s", ErrMismatch, ack.SHA, sha), Ack: &ack}
	case ack.Reviewer != reviewer:
		return ConsumeResult{OK: false, Layer: layer, Reason: fmt.Sprintf("%v: reviewer got=%q want=%q", ErrMismatch, ack.Reviewer, reviewer), Ack: &ack}
	case !strings.EqualFold(ack.ArtifactDigest, wantDigest):
		return ConsumeResult{OK: false, Layer: layer, Reason: fmt.Sprintf("%v: artifact digest got=%s want=%s", ErrStale, ack.ArtifactDigest, wantDigest), Ack: &ack}
	case ack.LaunchIdentity != launchIdentity:
		return ConsumeResult{OK: false, Layer: layer, Reason: fmt.Sprintf("%v: launch identity got=%q want=%q", ErrMismatch, ack.LaunchIdentity, launchIdentity), Ack: &ack}
	}

	if err := markConsumed(root, ack); err != nil {
		return ConsumeResult{OK: false, Layer: layer, Reason: "persist consume receipt: " + err.Error(), Ack: &ack}
	}
	return ConsumeResult{OK: true, Layer: layer, Reason: "canonical ingest acknowledgment verified", Ack: &ack}
}

func markConsumed(root string, ack Ack) error {
	dir := filepath.Join(root, ConsumedDirRel)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := consumedPath(root, ack.SHA, ack.Reviewer)
	body, err := json.MarshalIndent(ack, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func consumedPath(root, sha, reviewer string) string {
	return filepath.Join(root, ConsumedDirRel, FileName(sha, reviewer))
}

func shortSHA(sha string) string {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if len(sha) >= 12 {
		return sha[:12]
	}
	return sha
}

func sanitize(reviewer string) string {
	name := strings.ToLower(strings.TrimSpace(reviewer))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name = strings.Trim(b.String(), "-")
	if name == "" {
		name = "reviewer"
	}
	if len(name) > 48 {
		name = name[:48]
	}
	return name
}
