package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestQueryRelevantPatterns_SymlinkEscapeIgnored(t *testing.T) {
	outsideDir := t.TempDir()
	outsideSecretFile := filepath.Join(outsideDir, "secret.json")

	secretPat := &ErrorPattern{
		ID:         "leak-1",
		Domain:     "leak",
		Slug:       "leak-slug",
		Summary:    "LEAKED_SENSITIVE_ESCAPE_PAYLOAD",
		FixDetails: "do not leak",
	}
	secretData, err := json.Marshal(secretPat)
	if err != nil {
		t.Fatalf("failed to marshal secret pattern: %v", err)
	}
	if err := os.WriteFile(outsideSecretFile, secretData, 0600); err != nil {
		t.Fatalf("failed to write outside secret file: %v", err)
	}

	memDir := t.TempDir()
	mem := NewMemoryStore(memDir)

	// Record legitimate pattern inside store
	legit, err := mem.RecordErrorPattern("auth", "legit-slug", "LEGITIMATE_MEMORY_PAYLOAD", "ok")
	if err != nil || legit == nil {
		t.Fatalf("failed to record legit pattern: %v", err)
	}

	// Create symlink inside errors directory pointing to outside file
	errorsAuthDir := filepath.Join(memDir, "errors", "auth")
	escapeSymlink := filepath.Join(errorsAuthDir, "escape.json")
	if err := os.Symlink(outsideSecretFile, escapeSymlink); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// Verify querying for the leaked content returns nothing (escaped symlink ignored/rejected)
	leakedMatches, err := mem.QueryRelevantPatterns("LEAKED_SENSITIVE_ESCAPE_PAYLOAD")
	if err != nil {
		t.Fatalf("expected nil error on query containing uncontained symlink, got: %v", err)
	}
	if len(leakedMatches) != 0 {
		t.Fatalf("SECURITY VIOLATION: expected 0 matches for symlink escaping root, got %d: %+v", len(leakedMatches), leakedMatches[0])
	}

	// Verify legitimate patterns inside root are still discoverable
	legitMatches, err := mem.QueryRelevantPatterns("LEGITIMATE_MEMORY_PAYLOAD")
	if err != nil {
		t.Fatalf("expected nil error on legit query, got: %v", err)
	}
	if len(legitMatches) != 1 || legitMatches[0].Slug != "legit-slug" {
		t.Fatalf("expected 1 legit match, got %d (matches: %+v)", len(legitMatches), legitMatches)
	}
}

func TestQueryRelevantPatterns_SymlinkInsideAllowed(t *testing.T) {
	memDir := t.TempDir()
	mem := NewMemoryStore(memDir)

	_, err := mem.RecordErrorPattern("core", "base-slug", "SHARED_INSIDE_PATTERN", "fix")
	if err != nil {
		t.Fatalf("failed to record base pattern: %v", err)
	}

	// Create a relative symlink pointing to an internal file within errors root
	aliasDir := filepath.Join(memDir, "errors", "alias")
	if err := os.MkdirAll(aliasDir, 0755); err != nil {
		t.Fatalf("failed to mkdir alias dir: %v", err)
	}
	aliasSymlink := filepath.Join(aliasDir, "alias.json")
	targetRel := filepath.Join("..", "core", "base-slug.json")
	if err := os.Symlink(targetRel, aliasSymlink); err != nil {
		t.Fatalf("failed to create internal symlink: %v", err)
	}

	matched, err := mem.QueryRelevantPatterns("SHARED_INSIDE_PATTERN")
	if err != nil {
		t.Fatalf("unexpected error querying internal symlink: %v", err)
	}
	// Both original and alias resolve to the pattern within the root boundary
	if len(matched) < 1 {
		t.Fatalf("expected at least 1 match for internal symlink, got %d", len(matched))
	}
}

func TestRecordErrorPattern_TraversalRejected(t *testing.T) {
	memDir := t.TempDir()
	mem := NewMemoryStore(memDir)

	// Path traversal via ../ in domain must fail closed
	_, err := mem.RecordErrorPattern("../escaped", "slug", "summary", "fix")
	if err == nil {
		t.Fatal("expected error when recording pattern with path traversal in domain, got nil")
	}

	// Absolute path in domain must fail closed
	_, err = mem.RecordErrorPattern("/etc", "slug", "summary", "fix")
	if err == nil {
		t.Fatal("expected error when recording pattern with absolute path in domain, got nil")
	}
}

func TestRecordErrorPattern_SymlinkEscapeTargetRejected(t *testing.T) {
	outsideDir := t.TempDir()
	memDir := t.TempDir()

	errorsDir := filepath.Join(memDir, "errors")
	if err := os.MkdirAll(errorsDir, 0755); err != nil {
		t.Fatalf("failed to mkdir errors dir: %v", err)
	}

	// Symlink domain directory pointing to outside directory
	domainLink := filepath.Join(errorsDir, "escaped_domain")
	if err := os.Symlink(outsideDir, domainLink); err != nil {
		t.Fatalf("failed to create domain symlink: %v", err)
	}

	mem := NewMemoryStore(memDir)
	_, err := mem.RecordErrorPattern("escaped_domain", "exploit", "summary", "fix")
	if err == nil {
		t.Fatal("expected error when recording through domain symlink escaping errors root, got nil")
	}

	// Confirm no file was written to outsideDir
	entries, err := os.ReadDir(outsideDir)
	if err != nil {
		t.Fatalf("failed to read outside dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("SECURITY VIOLATION: wrote file outside errors root: %v", entries[0].Name())
	}
}
