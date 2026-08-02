package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQueryRelevantPatterns_NoErrorsDir(t *testing.T) {
	tmpDir := t.TempDir()
	// No errors/ directory created — should return nil, nil
	mem := NewMemoryStore(tmpDir)
	matched, err := mem.QueryRelevantPatterns("anything")
	if err != nil {
		t.Fatalf("expected nil error for missing errors dir, got: %v", err)
	}
	if len(matched) != 0 {
		t.Errorf("expected 0 matches for missing dir, got %d", len(matched))
	}
}

func TestQueryRelevantPatterns_MultiplePatternsFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	mem := NewMemoryStore(tmpDir)

	// Record two patterns
	mem.RecordErrorPattern("go", "nil-ptr", "nil pointer dereference in Go", "check for nil")
	mem.RecordErrorPattern("zsh", "sigpipe", "SIGPIPE in piped commands", "use herestrings")

	// Query for one
	matched, err := mem.QueryRelevantPatterns("sigpipe")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if len(matched) != 1 {
		t.Fatalf("expected 1 match for 'sigpipe', got %d", len(matched))
	}
	if matched[0].Domain != "zsh" {
		t.Errorf("expected domain 'zsh', got %s", matched[0].Domain)
	}

	// Query for domain match
	matched, err = mem.QueryRelevantPatterns("go")
	if err != nil || len(matched) != 1 {
		t.Fatalf("expected 1 match for 'go', got %d (err: %v)", len(matched), err)
	}
}

func TestRecordErrorPattern_FailsOnBadDir(t *testing.T) {
	mem := NewMemoryStore("/nonexistent-parent-xyzzy/deep")
	_, err := mem.RecordErrorPattern("test", "fail", "test", "test")
	if err == nil {
		t.Fatal("expected error when writing to nonexistent parent")
	}
}

func TestQueryRelevantPatterns_NonJSONFilesIgnored(t *testing.T) {
	tmpDir := t.TempDir()
	errorsDir := filepath.Join(tmpDir, "errors", "test")
	os.MkdirAll(errorsDir, 0755)
	os.WriteFile(filepath.Join(errorsDir, "note.txt"), []byte("not json"), 0644)

	mem := NewMemoryStore(tmpDir)
	matched, err := mem.QueryRelevantPatterns("not")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if len(matched) != 0 {
		t.Errorf("expected 0 matches from non-JSON files, got %d", len(matched))
	}
}

func TestQueryRelevantPatterns_WalkError(t *testing.T) {
	tmpDir := t.TempDir()
	mem := NewMemoryStore(tmpDir)

	mem.RecordErrorPattern("k8s", "oom", "OOM", "Fix")

	// Make the errors root dir unreadable — on macOS this is best-effort
	errorsDir := filepath.Join(tmpDir, "errors")
	origMode := os.ModePerm
	if fi, err := os.Stat(errorsDir); err == nil {
		origMode = fi.Mode()
	}
	os.Chmod(errorsDir, 0111) // execute-only, no read
	t.Cleanup(func() { os.Chmod(errorsDir, origMode) })

	matched, err := mem.QueryRelevantPatterns("oom")
	// On some systems/filesystems removing read from dir owner still allows Walk.
	// If we got an error, great. If not, the test is still useful because we
	// verified the Walk-error path doesn't crash.
	if err != nil {
		if len(matched) != 0 {
			t.Errorf("expected 0 matches on walk error, got %d", len(matched))
		}
	} else {
		t.Log("chmod had no effect on this filesystem; walk-error path not exercised")
	}
}

func TestRecordErrorPattern_MkdirAllError(t *testing.T) {
	tmpDir := t.TempDir()
	domainPath := filepath.Join(tmpDir, "errors")
	if err := os.MkdirAll(domainPath, 0755); err != nil {
		t.Fatal(err)
	}
	blockFile := filepath.Join(domainPath, "testdomain")
	if err := os.WriteFile(blockFile, []byte("block"), 0644); err != nil {
		t.Fatal(err)
	}

	mem := NewMemoryStore(tmpDir)
	_, err := mem.RecordErrorPattern("testdomain", "test-slug", "Summary", "Fix")
	if err == nil {
		t.Fatal("expected error when MkdirAll fails due to file in the way")
	}
}

func TestQueryRelevantPatterns_NoMatch(t *testing.T) {
	tmpDir := t.TempDir()
	mem := NewMemoryStore(tmpDir)

	mem.RecordErrorPattern("zsh", "sigpipe-flake", "SIGPIPE in piped grep", "Use herestrings <<<")

	matched, err := mem.QueryRelevantPatterns("nonexistent-topic")
	if err != nil || len(matched) != 0 {
		t.Fatalf("expected 0 matches, got %d (err: %v)", len(matched), err)
	}
}

func TestQueryRelevantPatterns_UnreadableJSONFile(t *testing.T) {
	tmpDir := t.TempDir()
	mem := NewMemoryStore(tmpDir)

	mem.RecordErrorPattern("k8s", "oom", "OOM", "Fix")

	domainDir := filepath.Join(tmpDir, "errors", "k8s")
	if err := os.WriteFile(filepath.Join(domainDir, "bad.json"), []byte("{}"), 0000); err != nil {
		t.Fatal(err)
	}

	matched, err := mem.QueryRelevantPatterns("oom")
	if err != nil || len(matched) != 1 {
		t.Fatalf("expected 1 match (unreadable file skipped), got %d (err: %v)", len(matched), err)
	}
}

func TestQueryRelevantPatterns_MalformedJSONFile(t *testing.T) {
	tmpDir := t.TempDir()
	mem := NewMemoryStore(tmpDir)

	mem.RecordErrorPattern("k8s", "oom", "OOM", "Fix")

	domainDir := filepath.Join(tmpDir, "errors", "k8s")
	if err := os.WriteFile(filepath.Join(domainDir, "bad.json"), []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}

	matched, err := mem.QueryRelevantPatterns("oom")
	if err != nil || len(matched) != 1 {
		t.Fatalf("expected 1 match (malformed file skipped), got %d (err: %v)", len(matched), err)
	}
}

func TestNewMemoryStore(t *testing.T) {
	ms := NewMemoryStore("/tmp/test-mem")
	if ms.MemoryDir != "/tmp/test-mem" {
		t.Errorf("expected /tmp/test-mem, got %s", ms.MemoryDir)
	}
}
