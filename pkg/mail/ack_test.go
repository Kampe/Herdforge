package mail

import (
	"path/filepath"
	"testing"
)

// TestHandledIsExplicitNotAField is the FAC-569 regression.
//
// Envelope.Read exists but NOTHING ever set it, so a pending filter on
// !env.Read matched everything forever: settled handoffs stayed pending and a
// "drain" could never complete. A queue whose acknowledgement is a field nobody
// writes is a log that looks like a queue.
func TestHandledIsExplicitNotAField(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))

	handled, err := box.Handled("sup", "id-1")
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("nothing is handled before it is acknowledged")
	}
	if err := box.MarkHandled("sup", "id-1"); err != nil {
		t.Fatal(err)
	}
	handled, err = box.Handled("sup", "id-1")
	if err != nil || !handled {
		t.Fatalf("acknowledgement must persist: %v %v", handled, err)
	}
	// Another recipient's queue is unaffected: acknowledgement is per-recipient
	// or one supervisor could clear another's work.
	if other, _ := box.Handled("other", "id-1"); other {
		t.Fatal("acknowledgement must not leak across recipients")
	}
	// Idempotent: a repeated ack is not an error and does not duplicate.
	if err := box.MarkHandled("sup", "id-1"); err != nil {
		t.Fatalf("repeated acknowledgement must be idempotent: %v", err)
	}
}

func TestMarkHandledRequiresIdentity(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	if err := box.MarkHandled("", "id"); err == nil {
		t.Fatal("an empty recipient must be refused")
	}
	if err := box.MarkHandled("sup", ""); err == nil {
		t.Fatal("an empty envelope id must be refused")
	}
}
