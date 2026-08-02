package mail

import (
	"path/filepath"
	"testing"
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
