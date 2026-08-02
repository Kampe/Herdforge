package mail

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMailbox_SendAndRead(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "herd-mail.jsonl")

	mb := NewMailbox(mailFile)

	env, err := mb.SendMessage("agent-smith", "agent-reviewer", "Review Request", "Please review PR #42")
	if err != nil || env == nil {
		t.Fatalf("expected message sent, got err: %v", err)
	}

	inbox, err := mb.ReadInbox("agent-reviewer")
	if err != nil || len(inbox) != 1 {
		t.Fatalf("expected 1 message in inbox, got %d (err: %v)", len(inbox), err)
	}

	if inbox[0].Subject != "Review Request" || inbox[0].Sender != "agent-smith" {
		t.Errorf("unexpected message envelope fields: %+v", inbox[0])
	}
}

func TestMailbox_ReadInbox_RecipientAll(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "herd-mail.jsonl")
	mb := NewMailbox(mailFile)

	mb.SendMessage("agent-foo", "agent-bar", "Subj", "Body")
	mb.SendMessage("agent-foo", "all", "Broadcast", "For everyone")

	inbox, err := mb.ReadInbox("agent-bar")
	if err != nil || len(inbox) != 2 {
		t.Fatalf("expected 2 messages (direct + 'all'), got %d (err: %v)", len(inbox), err)
	}
}

func TestMailbox_ReadInbox_NoMailFile(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "nonexistent.jsonl")
	mb := NewMailbox(mailFile)

	inbox, err := mb.ReadInbox("anyone")
	if err != nil || len(inbox) != 0 {
		t.Fatalf("expected 0 messages when no mail file exists, got %d (err: %v)", len(inbox), err)
	}
}

func TestMailbox_ReadInbox_ReadFileError(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "herd-mail.jsonl")
	mb := NewMailbox(mailFile)

	if err := os.MkdirAll(mailFile, 0755); err != nil {
		t.Fatal(err)
	}

	_, err := mb.ReadInbox("anyone")
	if err == nil {
		t.Fatal("expected error when mail file is a directory")
	}
}

func TestMailbox_ReadInbox_MalformedLine(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "herd-mail.jsonl")
	mb := NewMailbox(mailFile)

	mb.SendMessage("agent-foo", "agent-bar", "Subj", "Body")

	f, err := os.OpenFile(mailFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not-json\n")
	f.Close()

	inbox, err := mb.ReadInbox("agent-bar")
	if err != nil || len(inbox) != 1 {
		t.Fatalf("expected 1 message (malformed line skipped), got %d (err: %v)", len(inbox), err)
	}
}

func TestMailbox_ReadInbox_EmptyLine(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "herd-mail.jsonl")
	mb := NewMailbox(mailFile)

	mb.SendMessage("agent-foo", "agent-bar", "Subj", "Body")

	f, err := os.OpenFile(mailFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\n")
	f.Close()

	inbox, err := mb.ReadInbox("agent-bar")
	if err != nil || len(inbox) != 1 {
		t.Fatalf("expected 1 message, got %d (err: %v)", len(inbox), err)
	}
}

func TestMailbox_EmptyInbox(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "herd-mail.jsonl")
	mb := NewMailbox(mailFile)

	mb.SendMessage("agent-foo", "agent-bar", "Subj", "Body")

	inbox, err := mb.ReadInbox("no-one")
	if err != nil || len(inbox) != 0 {
		t.Fatalf("expected 0 messages for unmatched recipient, got %d (err: %v)", len(inbox), err)
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"one line", "hello", 1},
		{"two lines", "hello\nworld", 2},
		{"trailing newline", "hello\nworld\n", 2},
		{"leading newline", "\nhello", 2},
		{"blank lines", "\n\n", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitLines(tt.in)
			if len(got) != tt.want {
				t.Errorf("splitLines(%q) got %d lines, want %d", tt.in, len(got), tt.want)
			}
		})
	}
}

func TestNewID(t *testing.T) {
	id1 := newID()
	id2 := newID()

	if id1 == "" {
		t.Fatal("expected non-empty id")
	}
	if !strings.Contains(id1, "-") {
		t.Error("expected id to contain hostname separator")
	}
	if id1 == id2 {
		t.Error("expected consecutive newID() calls to produce different values")
	}
}

func TestNewMailbox(t *testing.T) {
	mb := NewMailbox("/tmp/test-mail.jsonl")
	if mb.MailFile != "/tmp/test-mail.jsonl" {
		t.Errorf("expected /tmp/test-mail.jsonl, got %s", mb.MailFile)
	}
}

func TestEnvelopeJSONRoundTrip(t *testing.T) {
	env := &Envelope{
		ID:        "host-123",
		Sender:    "alice",
		Recipient: "bob",
		Subject:   "Hello",
		Body:      "World",
		Read:      false,
		Timestamp: time.Now(),
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var dec Envelope
	if err := json.Unmarshal(data, &dec); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if dec.Sender != "alice" || dec.Recipient != "bob" {
		t.Errorf("envelope round-trip failed: %+v", dec)
	}
}
