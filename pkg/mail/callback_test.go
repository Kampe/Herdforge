package mail

import (
	"path/filepath"
	"testing"
)

func newTestMailbox(t *testing.T) *Mailbox {
	t.Helper()
	return NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
}

func TestPostAndDrainCallbacks(t *testing.T) {
	m := newTestMailbox(t)

	if _, err := m.PostCallback("task-fac-64", Callback{Ref: "FAC-64", Kind: CallbackComplete, SHA: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PostCallback("task-fac-72", Callback{Ref: "FAC-72", Kind: CallbackBlocked, Detail: "needs a decision"}); err != nil {
		t.Fatal(err)
	}
	// A plain non-callback message in the same inbox must be ignored.
	if _, err := m.SendMessage("someone", CoordinatorInbox, "hello", "just a note"); err != nil {
		t.Fatal(err)
	}

	cbs, err := m.DrainCallbacks()
	if err != nil {
		t.Fatal(err)
	}
	if len(cbs) != 2 {
		t.Fatalf("want 2 callbacks (plain msg skipped), got %d: %+v", len(cbs), cbs)
	}
	byRef := map[string]Callback{}
	for _, c := range cbs {
		byRef[c.Ref] = c
	}
	if byRef["FAC-64"].Kind != CallbackComplete || byRef["FAC-64"].SHA != "abc123" {
		t.Fatalf("FAC-64 callback wrong: %+v", byRef["FAC-64"])
	}
	if byRef["FAC-72"].Kind != CallbackBlocked || byRef["FAC-72"].Detail != "needs a decision" {
		t.Fatalf("FAC-72 callback wrong: %+v", byRef["FAC-72"])
	}
}

func TestDrainCallbacks_EmptyInbox(t *testing.T) {
	m := newTestMailbox(t)
	cbs, err := m.DrainCallbacks()
	if err != nil {
		t.Fatal(err)
	}
	if len(cbs) != 0 {
		t.Fatalf("empty inbox must drain nothing, got %d", len(cbs))
	}
}
