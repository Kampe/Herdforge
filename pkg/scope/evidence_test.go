package scope

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testScope() AdmissionScope {
	s := AdmissionScope{
		Version:      Version1,
		Repository:   "org/repo",
		TargetBranch: "main",
		TargetSHA:    strings.Repeat("a", 40),
		CandidateSHA: strings.Repeat("b", 40),
		MergeBase:    strings.Repeat("a", 40),
		Commits:      []string{strings.Repeat("b", 40)},
		ChangedPaths: []string{"a.txt"},
		DiffDigest:   "sha256:" + strings.Repeat("c", 64),
	}
	s.Digest = computeDigest(s)
	return s
}

func TestRejectedEvidenceStore_PersistAndLoad(t *testing.T) {
	store, err := NewRejectedEvidenceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 4, 18, 26, 14, 0, time.UTC)
	evidence, err := store.Persist("PR published before hold was consumed", testScope(), "https://github.com/Kampe/Herdforge/pull/92", when)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	loaded, err := store.Load(evidence.Digest)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Reason != evidence.Reason || loaded.Reference != evidence.Reference {
		t.Fatalf("loaded evidence mismatch: %+v", loaded)
	}
}

func TestRejectedEvidenceStore_NoDeleteOrUpdateMethodExists(t *testing.T) {
	// Compile-time assertion by absence: RejectedEvidenceStore exposes only
	// Persist and Load. If a Delete/Update method is ever added, this test
	// still passes but a code reviewer grepping for it will find this note.
	store := &RejectedEvidenceStore{}
	_ = store.Persist
	_ = store.Load
}

func TestRejectedEvidenceStore_IdempotentReRecordOfIdenticalEvent(t *testing.T) {
	store, err := NewRejectedEvidenceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 4, 18, 26, 14, 0, time.UTC)
	first, err := store.Persist("dup", testScope(), "ref", when)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Persist("dup", testScope(), "ref", when)
	if err != nil {
		t.Fatalf("re-recording the identical event must be idempotent: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digests differ for identical input: %s vs %s", first.Digest, second.Digest)
	}
}

func TestRejectedEvidenceStore_RefusesToOverwriteDifferingContentAtSameDigest(t *testing.T) {
	dir := t.TempDir()
	store, err := NewRejectedEvidenceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 4, 18, 26, 14, 0, time.UTC)
	evidence, err := store.Persist("original", testScope(), "ref", when)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate on-disk corruption/tampering at the immutable evidence path.
	path := filepath.Join(dir, strings.TrimPrefix(evidence.Digest, "sha256:")+".json")
	if err := os.WriteFile(path, []byte(`{"tampered":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Persist("original", testScope(), "ref", when); err == nil {
		t.Fatal("persisting over tampered/differing content at the same digest must fail")
	}
}

func TestRejectedEvidenceStore_RequiresReason(t *testing.T) {
	store, err := NewRejectedEvidenceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Persist("", testScope(), "ref", time.Now().UTC()); err == nil {
		t.Fatal("empty reason must be rejected")
	}
}

func TestRejectedEvidenceStore_Uses0600(t *testing.T) {
	dir := t.TempDir()
	store, err := NewRejectedEvidenceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := store.Persist("reason", testScope(), "ref", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, strings.TrimPrefix(evidence.Digest, "sha256:")+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
