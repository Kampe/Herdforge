package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mail"
)

func TestControlMailPathUsesRepositoryCommonRoot(t *testing.T) {
	root, err := canonicalHerdRoot()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_MAIL_FILE", "nested/shared-mail.jsonl")

	got, err := controlMailPath("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "nested", "shared-mail.jsonl")
	if got != want {
		t.Fatalf("shared mail path = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Dir(got)); !os.IsNotExist(err) {
		t.Fatalf("path resolver unexpectedly created %q", filepath.Dir(got))
	}
}

func TestControlMailPathRejectsAbsoluteEnvironmentPath(t *testing.T) {
	t.Setenv("HERD_MAIL_FILE", filepath.Join(t.TempDir(), "mail.jsonl"))
	if _, err := controlMailPath(""); err == nil {
		t.Fatal("absolute HERD_MAIL_FILE must be rejected")
	}
}

func TestSharedMailPathRoundTripsAcrossLaneHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "shared-mail.jsonl")
	planner := mail.NewMailbox(path)
	supervisor := mail.NewMailbox(path)
	if _, err := planner.SendMessage("scout-planner", "forge-review-harvest-supervisor", "handoff", "merge-ready"); err != nil {
		t.Fatal(err)
	}
	inbox, err := supervisor.ReadInbox("forge-review-harvest-supervisor")
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].Body != "merge-ready" {
		t.Fatalf("supervisor did not receive planner bytes: %+v", inbox)
	}
}

func TestMailCLIHelpDistinguishesOrdinaryAndControlMail(t *testing.T) {
	out, err := runHerd(t, sandbox(t), nil, "mail", "--help")
	if err != nil {
		t.Fatalf("herd mail --help: %v\n%s", err, out)
	}
	text := strings.ToLower(string(out))
	if !strings.Contains(text, "ordinary durable messages") || !strings.Contains(text, "privileged authenticated control") {
		t.Fatalf("mail help did not distinguish message classes: %s", text)
	}
}

func TestMailCLISendsAndReadsDurableMessage(t *testing.T) {
	dir := sandbox(t)
	mailFile := filepath.Join(dir, ".herd", "mail.jsonl")
	out, err := runHerd(t, dir, nil, "mail", "send", "--from", "coordinator", "--to", "worker-a", "--subject", "handoff", "--body", "ready", "--mail", mailFile)
	if err != nil {
		t.Fatalf("herd mail send: %v\n%s", err, out)
	}
	var sent struct {
		Recipient string `json:"recipient"`
		Subject   string `json:"subject"`
		Body      string `json:"body"`
	}
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatalf("decode send output: %v\n%s", err, out)
	}
	if sent.Recipient != "worker-a" || sent.Subject != "handoff" || sent.Body != "ready" {
		t.Fatalf("unexpected sent envelope: %+v", sent)
	}

	out, err = runHerd(t, dir, nil, "mail", "inbox", "--recipient", "worker-a", "--mail", mailFile)
	if err != nil {
		t.Fatalf("herd mail inbox: %v\n%s", err, out)
	}
	var inbox []struct {
		Recipient string `json:"recipient"`
		Body      string `json:"body"`
	}
	if err := json.Unmarshal(out, &inbox); err != nil {
		t.Fatalf("decode inbox output: %v\n%s", err, out)
	}
	if len(inbox) != 1 || inbox[0].Recipient != "worker-a" || inbox[0].Body != "ready" {
		t.Fatalf("unexpected inbox: %+v", inbox)
	}
}

func TestMailCLIRejectsInvalidRecipientAndMalformedControlEnvelope(t *testing.T) {
	dir := sandbox(t)
	if out, err := runHerd(t, dir, nil, "mail", "send", "--from", "coordinator", "--to", " ", "--body", "ready"); err == nil {
		t.Fatalf("invalid recipient succeeded: %s", out)
	}
	if out, err := runHerd(t, dir, nil, "mail", "control", "issue", "--task", "FAC-248"); err == nil {
		t.Fatalf("malformed control envelope succeeded: %s", out)
	}
	if out, err := runHerd(t, dir, nil, "control", "issue", "--task", "FAC-248"); err == nil {
		t.Fatalf("existing control issue must remain fail-closed: %s", out)
	}
	if out, err := runHerd(t, dir, nil, "control", "drain", "--task", "FAC-248"); err == nil {
		t.Fatalf("existing control drain must remain fail-closed: %s", out)
	}
}
