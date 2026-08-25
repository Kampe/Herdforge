package main

import (
	"os"
	"path/filepath"
	"testing"
)

// FAC-629: the feedback producer and the control bus use DIFFERENT SCHEMAS, and
// that is the real split -- not just two paths. Feedback writes
// id(int)/from/to/summary/message/read_at; mail.Envelope expects
// id(string)/sender/recipient/subject/body/read. A direct unmarshal fails on the
// id type alone, which is why the first attempt returned zero envelopes from a
// file that plainly had two.
func TestReadFeedbackMailbox_TranslatesProducerSchema(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_MAIL_DIR", dir)
	line := `{"id":7,"type":"message","from":"forge-orchestrator","to":"lane-x",` +
		`"timestamp":"2026-08-24T20:56:59Z","summary":"FLEET_FEEDBACK epoch",` +
		`"message":"report blockers","category":"informational","read_at":null}`
	if err := os.WriteFile(filepath.Join(dir, "lane-x.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readFeedbackMailbox("lane-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(got))
	}
	e := got[0]
	if e.Sender != "forge-orchestrator" || e.Recipient != "lane-x" {
		t.Errorf("from/to not mapped to sender/recipient: %+v", e)
	}
	if e.Subject != "FLEET_FEEDBACK epoch" || e.Body != "report blockers" {
		t.Errorf("summary/message not mapped to subject/body: %+v", e)
	}
	if e.Read {
		t.Error("read_at null must mean unread")
	}
	if e.Timestamp.IsZero() {
		t.Error("timestamp must be parsed")
	}
}

// An absent mailbox is normal: most lanes are never polled. It must not error.
func TestReadFeedbackMailbox_MissingIsNotAnError(t *testing.T) {
	t.Setenv("HERD_MAIL_DIR", t.TempDir())
	got, err := readFeedbackMailbox("never-polled")
	if err != nil {
		t.Fatalf("absent mailbox must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want none, got %d", len(got))
	}
}

// read_at set means the lane already consumed it.
func TestReadFeedbackMailbox_ReadAtMarksRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_MAIL_DIR", dir)
	line := `{"id":1,"from":"a","to":"lane-y","timestamp":"2026-08-24T20:56:59Z",` +
		`"summary":"s","message":"m","read_at":"2026-08-24T21:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "lane-y.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := readFeedbackMailbox("lane-y")
	if len(got) != 1 || !got[0].Read {
		t.Fatalf("read_at must mark the envelope read: %+v", got)
	}
}
