package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

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
