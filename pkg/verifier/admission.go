package verifier

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReceiptStore is the compile-safe persistence seam for production callers.
// VerifyAndPersist stores every terminal outcome; ReceiptAdmission accepts only
// a current PASS whose digest and candidate SHA still validate. The existing
// command/review wiring has not yet been connected to this seam and remains a
// documented FAC-122 integration item.
type ReceiptStore interface {
	Persist(context.Context, Receipt) error
	Load(context.Context, string) (Receipt, error)
}

// FileReceiptStore persists receipts as atomically replaced, mode-0600 JSON
// files named by their self-authenticating SHA-256 digest.
type FileReceiptStore struct {
	Dir string
}

func NewFileReceiptStore(dir string) (*FileReceiptStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("receipt store directory is empty")
	}
	return &FileReceiptStore{Dir: filepath.Clean(dir)}, nil
}

func (s *FileReceiptStore) Persist(ctx context.Context, receipt Receipt) error {
	if s == nil {
		return errors.New("nil receipt store")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := receipt.ValidateDigest(); err != nil {
		return fmt.Errorf("refuse invalid receipt: %w", err)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create receipt store: %w", err)
	}
	path, err := s.pathFor(receipt.Digest)
	if err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode receipt: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, ".receipt-*.tmp")
	if err != nil {
		return fmt.Errorf("create receipt temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect receipt temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write receipt: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync receipt: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close receipt: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install receipt: %w", err)
	}
	return nil
}

func (s *FileReceiptStore) Load(ctx context.Context, digest string) (Receipt, error) {
	var receipt Receipt
	if s == nil {
		return receipt, errors.New("nil receipt store")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	path, err := s.pathFor(digest)
	if err != nil {
		return receipt, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return receipt, fmt.Errorf("read receipt: %w", err)
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		return receipt, fmt.Errorf("decode receipt: %w", err)
	}
	if receipt.Digest != digest {
		return receipt, errors.New("receipt filename digest does not match payload")
	}
	if err := receipt.ValidateDigest(); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (s *FileReceiptStore) pathFor(digest string) (string, error) {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", errors.New("receipt digest must be a full sha256 digest")
	}
	suffix := strings.TrimPrefix(digest, "sha256:")
	if _, err := hex.DecodeString(suffix); err != nil {
		return "", errors.New("receipt digest must contain only hexadecimal characters")
	}
	return filepath.Join(s.Dir, suffix+".json"), nil
}

// VerifyAndPersist is the production-facing seam: verification produces a
// receipt and persists it before returning. A caller must still pass the
// receipt digest to ReceiptAdmission before review admission.
func (v *Verifier) VerifyAndPersist(ctx context.Context, dir string, req VerificationRequest, store ReceiptStore) (*Receipt, error) {
	if store == nil {
		return nil, errors.New("receipt store is required")
	}
	receipt, err := v.VerifyCandidate(ctx, dir, req)
	if err != nil {
		return nil, err
	}
	if err := store.Persist(ctx, *receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}

// ReceiptAdmission is the compile-safe review gate. It deliberately requires
// the caller to supply the digest it observed for the current candidate;
// loading any latest receipt is not sufficient.
type ReceiptAdmission struct {
	Store ReceiptStore
}

func NewReceiptAdmission(store ReceiptStore) *ReceiptAdmission {
	return &ReceiptAdmission{Store: store}
}

func (a *ReceiptAdmission) RequireCurrentPassing(ctx context.Context, dir, digest string) (*Receipt, error) {
	if a == nil || a.Store == nil {
		return nil, errors.New("receipt admission store is required")
	}
	receipt, err := a.Store.Load(ctx, digest)
	if err != nil {
		return nil, err
	}
	if receipt.Outcome != OutcomePASS {
		return nil, fmt.Errorf("receipt outcome %s is not PASS", receipt.Outcome)
	}
	if receipt.Digest != digest {
		return nil, errors.New("receipt digest is not current")
	}
	if err := receipt.ValidateReceipt(ctx, dir); err != nil {
		return nil, err
	}
	return &receipt, nil
}
