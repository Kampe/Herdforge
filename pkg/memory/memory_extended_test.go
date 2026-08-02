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
	mem := NewMemoryStore("/nonexistent")
	matched, err := mem.QueryRelevantPatterns("test")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent dir (handled by Stat check), got: %v", err)
	}
	if len(matched) != 0 {
		t.Errorf("expected 0 matches, got %d", len(matched))
	}
}
