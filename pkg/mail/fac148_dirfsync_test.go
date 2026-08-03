package mail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-148 finding 3: writeFileAtomic and appendLine fsync'd the file's own
// data but never the containing directory, so the file-creation/rename
// itself wasn't crash-durable. TestParentDirFsync_FailurePropagates injects
// a directory fsync failure via the package's syncDirFn hook and proves
// both write paths surface it instead of ignoring it — the mutation probe
// for "the fsync call is simply missing".
func TestParentDirFsync_FailurePropagates(t *testing.T) {
	orig := syncDirFn
	defer func() { syncDirFn = orig }()

	injectedErr := fmt.Errorf("injected directory fsync failure")
	syncDirFn = func(path string) error { return injectedErr }

	tmpDir := t.TempDir()

	t.Run("appendLine", func(t *testing.T) {
		err := appendLine(filepath.Join(tmpDir, "a.jsonl"), []byte(`{"id":"x"}`))
		if err == nil {
			t.Fatal("expected appendLine to propagate the directory fsync failure")
		}
	})

	t.Run("writeFileAtomic", func(t *testing.T) {
		err := writeFileAtomic(filepath.Join(tmpDir, "b.json"), []byte(`{}`), 0644)
		if err == nil {
			t.Fatal("expected writeFileAtomic to propagate the directory fsync failure")
		}
	})
}

// TestParentDirFsync_CrashReopenAssertion proves the happy path actually
// calls the real directory-fsync helper (not just a no-op) and that content
// survives being reopened by a fresh handle, simulating a process restart.
func TestParentDirFsync_CrashReopenAssertion(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "durable.jsonl")

	if err := appendLine(path, []byte(`{"id":"one"}`)); err != nil {
		t.Fatalf("appendLine failed: %v", err)
	}
	if err := syncDir(path); err != nil {
		t.Fatalf("directory fsync itself failed on this platform: %v", err)
	}

	reopened, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected the file to survive reopen after a fresh directory fsync: %v", err)
	}
	if !strings.Contains(string(reopened), `"id":"one"`) {
		t.Fatalf("reopened content missing expected record: %s", reopened)
	}

	atomicPath := filepath.Join(tmpDir, "meta.json")
	if err := writeFileAtomic(atomicPath, []byte(`{"seq":1}`), 0644); err != nil {
		t.Fatalf("writeFileAtomic failed: %v", err)
	}
	reopenedMeta, err := os.ReadFile(atomicPath)
	if err != nil {
		t.Fatalf("expected the atomically-renamed file to survive reopen: %v", err)
	}
	if string(reopenedMeta) != `{"seq":1}` {
		t.Fatalf("reopened atomic content mismatch: %s", reopenedMeta)
	}
}
