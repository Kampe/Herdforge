package scope

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RejectedEvidence is an immutable record of a publication attempt a scope
// gate refused — e.g. the PR #92 quarantine after FAC-69. It is preserved
// exactly as recorded; RejectedEvidenceStore exposes no delete or update.
type RejectedEvidence struct {
	Version    int            `json:"version"`
	Reason     string         `json:"reason"`
	Scope      AdmissionScope `json:"scope"`
	Reference  string         `json:"reference,omitempty"`
	RecordedAt time.Time      `json:"recorded_at"`
	Digest     string         `json:"digest"`
}

type rejectedEvidenceForDigest struct {
	Version    int            `json:"version"`
	Reason     string         `json:"reason"`
	Scope      AdmissionScope `json:"scope"`
	Reference  string         `json:"reference,omitempty"`
	RecordedAt time.Time      `json:"recorded_at"`
}

func computeEvidenceDigest(e RejectedEvidence) string {
	payload := rejectedEvidenceForDigest{
		Version: e.Version, Reason: e.Reason, Scope: e.Scope, Reference: e.Reference, RecordedAt: e.RecordedAt,
	}
	data, _ := json.Marshal(payload)
	return "sha256:" + digestBytes(data)
}

// RejectedEvidenceStore persists refused publication attempts as
// atomically-written, mode-0600 JSON files named by their self-authenticating
// digest, mirroring verifier.FileReceiptStore's install pattern. There is
// deliberately no delete or update method.
type RejectedEvidenceStore struct {
	Dir string
}

func NewRejectedEvidenceStore(dir string) (*RejectedEvidenceStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("scope: rejected evidence directory is empty")
	}
	return &RejectedEvidenceStore{Dir: filepath.Clean(dir)}, nil
}

// Persist records a refusal. Persisting the identical event twice (same
// reason, scope, reference, and timestamp) is an idempotent no-op; persisting
// a different event under a digest that already exists on disk is an
// immutability violation and fails.
func (s *RejectedEvidenceStore) Persist(reason string, recordedScope AdmissionScope, reference string, recordedAt time.Time) (*RejectedEvidence, error) {
	if s == nil {
		return nil, errors.New("scope: nil rejected evidence store")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, errors.New("scope: rejected evidence reason is required")
	}
	evidence := RejectedEvidence{Version: Version1, Reason: reason, Scope: recordedScope, Reference: reference, RecordedAt: recordedAt.UTC()}
	evidence.Digest = computeEvidenceDigest(evidence)

	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("scope: create rejected evidence store: %w", err)
	}
	path, err := s.pathFor(evidence.Digest)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		return nil, fmt.Errorf("scope: encode rejected evidence: %w", err)
	}
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != string(data) {
			return nil, fmt.Errorf("scope: rejected evidence %s already recorded with different content (immutability violation)", evidence.Digest)
		}
		return &evidence, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("scope: read existing rejected evidence: %w", err)
	}

	tmp, err := os.CreateTemp(s.Dir, ".rejected-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("scope: create rejected evidence temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("scope: protect rejected evidence temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("scope: write rejected evidence: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("scope: sync rejected evidence: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("scope: close rejected evidence: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, fmt.Errorf("scope: install rejected evidence: %w", err)
	}
	return &evidence, nil
}

func (s *RejectedEvidenceStore) Load(digest string) (RejectedEvidence, error) {
	var evidence RejectedEvidence
	if s == nil {
		return evidence, errors.New("scope: nil rejected evidence store")
	}
	path, err := s.pathFor(digest)
	if err != nil {
		return evidence, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return evidence, fmt.Errorf("scope: read rejected evidence: %w", err)
	}
	if err := json.Unmarshal(data, &evidence); err != nil {
		return evidence, fmt.Errorf("scope: decode rejected evidence: %w", err)
	}
	if evidence.Digest != digest {
		return evidence, errors.New("scope: rejected evidence filename digest does not match payload")
	}
	return evidence, nil
}

func (s *RejectedEvidenceStore) pathFor(digest string) (string, error) {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", errors.New("scope: rejected evidence digest must be a full sha256 digest")
	}
	suffix := strings.TrimPrefix(digest, "sha256:")
	if _, err := hex.DecodeString(suffix); err != nil {
		return "", errors.New("scope: rejected evidence digest must contain only hexadecimal characters")
	}
	return filepath.Join(s.Dir, suffix+".json"), nil
}
