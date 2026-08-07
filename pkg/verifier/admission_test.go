package verifier

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileReceiptStorePersistRejectsTamperedDigestWithoutFile(t *testing.T) {
	store, receipt := persistedReceiptFixture(t)
	receipt.Digest = "sha256:" + strings.Repeat("e", 64)
	if err := store.Persist(context.Background(), receipt); err == nil {
		t.Fatal("tampered receipt digest must be rejected")
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tampered receipt must not write files: %v", entries)
	}
}

func TestFileReceiptStorePersistUsesAtomicRenameAndCompleteFile(t *testing.T) {
	store, receipt := persistedReceiptFixture(t)
	renamed := false
	store.rename = func(oldPath, newPath string) error {
		renamed = true
		return os.Rename(oldPath, newPath)
	}
	if err := store.Persist(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	if !renamed {
		t.Fatal("receipt persistence must install the complete file with rename")
	}
	tmpFiles, err := filepath.Glob(filepath.Join(store.Dir, ".receipt-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpFiles) != 0 {
		t.Fatalf("temporary receipt files must be cleaned up: %v", tmpFiles)
	}
	loaded, err := store.Load(context.Background(), receipt.Digest)
	if err != nil || loaded.Digest != receipt.Digest {
		t.Fatalf("renamed receipt must be complete and loadable: receipt=%+v err=%v", loaded, err)
	}
}

func TestFileReceiptStorePersistUses0600(t *testing.T) {
	store, receipt := persistedReceiptFixture(t)
	if err := store.Persist(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, strings.TrimPrefix(receipt.Digest, "sha256:")+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("receipt mode = %o, want 600", got)
	}
}

func TestFileReceiptStoreLoadRejectsWrongFilenameDigest(t *testing.T) {
	store, receipt := persistedReceiptFixture(t)
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	wrongDigest := "sha256:" + strings.Repeat("f", 64)
	wrongPath := filepath.Join(store.Dir, strings.TrimPrefix(wrongDigest, "sha256:")+".json")
	if err := os.WriteFile(wrongPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), wrongDigest); err == nil {
		t.Fatal("receipt under a wrong digest filename must be rejected")
	}
}

func TestFileReceiptStoreLoadRejectsPayloadDigestMismatch(t *testing.T) {
	store, receipt := persistedReceiptFixture(t)
	tampered := receipt
	tampered.Outcome = OutcomeFAIL
	data, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, strings.TrimPrefix(receipt.Digest, "sha256:")+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), receipt.Digest); err == nil {
		t.Fatal("receipt whose payload no longer matches its digest must be rejected")
	}
}

func TestReceiptAdmissionRejectsFailAndBlockedReceipts(t *testing.T) {
	dir, candidate := verificationRepo(t)
	store, err := NewFileReceiptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	admission := NewReceiptAdmission(store)
	fail, err := NewVerifierArgs([]string{"./always-fail.sh"}).VerifyAndPersist(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
	}, store)
	if err != nil || fail.Outcome != OutcomeFAIL {
		t.Fatalf("expected persisted FAIL receipt: receipt=%+v err=%v", fail, err)
	}
	if _, err := admission.RequireCurrentPassing(context.Background(), dir, fail.Digest); err == nil || !strings.Contains(err.Error(), "not PASS") {
		t.Fatalf("FAIL receipt must be rejected by admission: %v", err)
	}

	writeFile(t, filepath.Join(dir, "dirty-before-blocked.txt"), "dirty\n")
	blocked, err := NewVerifierArgs([]string{"./check.sh"}).VerifyAndPersist(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
	}, store)
	if err != nil || blocked.Outcome != OutcomeBLOCKED {
		t.Fatalf("expected persisted BLOCKED receipt: receipt=%+v err=%v", blocked, err)
	}
	if _, err := admission.RequireCurrentPassing(context.Background(), dir, blocked.Digest); err == nil || !strings.Contains(err.Error(), "not PASS") {
		t.Fatalf("BLOCKED receipt must be rejected by admission: %v", err)
	}
}

func persistedReceiptFixture(t *testing.T) (*FileReceiptStore, Receipt) {
	t.Helper()
	store, err := NewFileReceiptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Receipt-store tests exercise persistence and digest validation, not
	// subprocess ownership. Build the same signed PASS payload directly; the
	// end-to-end verification/receipt path has dedicated tests below.
	receipt := fixturePassingReceipt(strings.Repeat("a", 40))
	return store, receipt
}
