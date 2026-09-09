package mail

import (
	"context"
	"strings"
	"testing"
)

func TestImportOrdinaryIsStableAndPreservesBody(t *testing.T) {
	box := NewMailbox(t.TempDir() + "/mail.jsonl")
	source := &Envelope{ID: "wsl-1306", Sender: "worker", Recipient: "coordinator", Subject: "FAC-773 report", Body: "literal $(not shell)\nsecond line", Read: true}
	first, err := box.ImportOrdinary(context.Background(), "wsl-box", "coordinator", source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := box.ImportOrdinary(context.Background(), "wsl-box", "coordinator", source)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.ID != ImportedOrdinaryID("wsl-box", source.ID) {
		t.Fatalf("import identities = %q, %q", first.ID, second.ID)
	}
	if first.Body != source.Body || first.OriginalSourceHost != "wsl-box" || first.OriginalSourceID != source.ID || first.Read {
		t.Fatalf("import changed source data: %+v", first)
	}
	envs, err := box.ReadInbox("coordinator")
	if err != nil || len(envs) != 1 {
		t.Fatalf("local inbox = %+v, %v", envs, err)
	}
	if pending, err := box.PendingOrdinary("coordinator"); err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}

func TestImportOrdinaryRejectsChangedIdentityAndForeignClasses(t *testing.T) {
	box := NewMailbox(t.TempDir() + "/mail.jsonl")
	base := &Envelope{ID: "source-1", Sender: "worker", Recipient: "coordinator", Subject: "report", Body: "one"}
	if _, err := box.ImportOrdinary(nil, "wsl-box", "coordinator", base); err != nil {
		t.Fatal(err)
	}
	changed := *base
	changed.Body = "two"
	if _, err := box.ImportOrdinary(nil, "wsl-box", "coordinator", &changed); err == nil || !strings.Contains(err.Error(), "changed bytes") {
		t.Fatalf("changed source accepted: %v", err)
	}
	foreign := *base
	foreign.ID = "source-2"
	foreign.Recipient = "other"
	if _, err := box.ImportOrdinary(nil, "wsl-box", "coordinator", &foreign); err == nil {
		t.Fatal("foreign recipient accepted")
	}
	for _, subject := range []string{QueuedDeliverySubject, ControlSubjectPrefix + " issue", "complete: FAC-1", "blocked: FAC-1"} {
		env := *base
		env.ID, env.Subject = subject, subject
		if _, err := box.ImportOrdinary(nil, "wsl-box", "coordinator", &env); err == nil {
			t.Fatalf("special subject %q accepted", subject)
		}
	}
}

func TestOrdinaryStatusRequiresLocalAck(t *testing.T) {
	box := NewMailbox(t.TempDir() + "/mail.jsonl")
	env, err := box.ImportOrdinary(nil, "wsl-box", "coordinator", &Envelope{ID: "source-1", Sender: "worker", Recipient: "coordinator", Subject: "report", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	status, err := box.StatusOrdinary("coordinator", env.ID)
	if err != nil || !status.Pending || status.Handled {
		t.Fatalf("before ack status = %+v, %v", status, err)
	}
	if err := box.MarkHandled("coordinator", env.ID); err != nil {
		t.Fatal(err)
	}
	status, err = box.StatusOrdinary("coordinator", env.ID)
	if err != nil || status.Pending || !status.Handled {
		t.Fatalf("after ack status = %+v, %v", status, err)
	}
}
