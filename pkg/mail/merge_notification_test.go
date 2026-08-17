package mail

import (
	"path/filepath"
	"testing"
)

func TestPostMergeNotification_IsDurableAndIdempotent(t *testing.T) {
	mb := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	n := MergeNotification{
		TaskRef:      "FAC-353",
		CandidateSHA: "candidate-sha",
		LandedCommit: "landed-commit",
		BaseSHA:      "base-sha",
		Branch:       "herd/fac-353",
		Repository:   "Herdforge",
		BuilderID:    "session-builder-1",
	}

	first, err := mb.PostMergeNotification("coordinator", n)
	if err != nil {
		t.Fatalf("post merge notification: %v", err)
	}
	second, err := NewMailbox(mb.MailFile).PostMergeNotification("coordinator", n)
	if err != nil {
		t.Fatalf("retry merge notification: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("retry changed envelope identity: first=%q second=%q", first.ID, second.ID)
	}

	inbox, err := NewMailbox(mb.MailFile).ReadInbox(n.BuilderID)
	if err != nil {
		t.Fatalf("read builder inbox after restart: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("got %d durable notifications, want exactly one", len(inbox))
	}
	got, err := DecodeMergeNotification(inbox[0])
	if err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if *got != n {
		t.Fatalf("notification = %+v, want %+v", *got, n)
	}
}

func TestPostMergeNotification_RejectsMissingLaunchIdentity(t *testing.T) {
	_, err := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl")).PostMergeNotification("coordinator", MergeNotification{TaskRef: "FAC-353"})
	if err == nil {
		t.Fatal("missing builder identity was accepted")
	}
}
