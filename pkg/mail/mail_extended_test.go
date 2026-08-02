package mail

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadInbox_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "nonexistent.jsonl"))

	envs, err := mb.ReadInbox("anyone")
	if err != nil {
		t.Fatalf("expected nil error for missing mail file, got: %v", err)
	}
	if len(envs) != 0 {
		t.Errorf("expected 0 envelopes for missing file, got %d", len(envs))
	}
}

func TestReadInbox_EmptyLines(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "empty.jsonl")
	os.WriteFile(mailFile, []byte(""), 0644)

	mb := NewMailbox(mailFile)
	envs, err := mb.ReadInbox("")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if len(envs) != 0 {
		t.Errorf("expected 0 envelopes for empty file, got %d", len(envs))
	}
}

func TestReadInbox_BadJSONLine(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "bad.jsonl")
	os.WriteFile(mailFile, []byte("not json\n{\"id\":\"msg-1\",\"sender\":\"a\",\"recipient\":\"b\",\"body\":\"\",\"read\":false,\"timestamp\":\"2026-01-01T00:00:00Z\"}\n"), 0644)

	mb := NewMailbox(mailFile)
	envs, err := mb.ReadInbox("b")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if len(envs) != 1 {
		t.Errorf("expected 1 valid envelope, got %d", len(envs))
	}
}

func TestSendMessage_DirCreation(t *testing.T) {
	tmpDir := t.TempDir()
	deepFile := filepath.Join(tmpDir, "sub", "dir", "mail.jsonl")
	mb := NewMailbox(deepFile)
	env, err := mb.SendMessage("worker", "boss", "Subj", "Body")
	if err != nil {
		t.Fatalf("expected clean send with dir creation, got: %v", err)
	}
	if env.Subject != "Subj" {
		t.Errorf("expected Subject 'Subj', got %s", env.Subject)
	}
}
