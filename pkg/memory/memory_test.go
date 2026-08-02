package memory

import (
	"testing"
)

func TestMemoryStore_RecordAndQuery(t *testing.T) {
	tmpDir := t.TempDir()
	mem := NewMemoryStore(tmpDir)

	pat, err := mem.RecordErrorPattern("zsh", "sigpipe-flake", "SIGPIPE in piped grep", "Use herestrings <<<")
	if err != nil || pat == nil {
		t.Fatalf("expected pattern recorded, got err: %v", err)
	}

	matched, err := mem.QueryRelevantPatterns("sigpipe")
	if err != nil || len(matched) != 1 {
		t.Fatalf("expected 1 matched pattern, got %d (err: %v)", len(matched), err)
	}

	if matched[0].Slug != "sigpipe-flake" || matched[0].Domain != "zsh" {
		t.Errorf("unexpected pattern fields: %+v", matched[0])
	}
}
