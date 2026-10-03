package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mail"
)

func TestMailPendingAndStatusShowQueuedDurableUntilIdleDrainOnce(t *testing.T) {
	proc := startFakeCommand(t)
	repo := queuedSendRepo(t)
	bin, logPath, statusPath := installQueuedSendFake(t, "working", strconv.Itoa(proc.Pid))
	env := queuedSendEnv(bin, repo)
	mailFile := filepath.Join(repo, ".herd", "control-mail.jsonl")
	body := "queued-durable visibility payload"

	out, err := runHerd(t, repo, env, "send", "worker", body, "--no-verify")
	if err != nil {
		t.Fatalf("queue: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "queued-durable") {
		t.Fatalf("want queued-durable send, got %s", out)
	}

	out, err = runHerd(t, repo, env, "mail", "pending", "--recipient", "worker", "--mail", mailFile)
	if err != nil {
		t.Fatalf("pending: %v\n%s", err, out)
	}
	var pending []mail.Envelope
	if err := json.Unmarshal(out, &pending); err != nil {
		t.Fatalf("decode pending: %v\n%s", err, out)
	}
	if len(pending) != 1 || pending[0].Subject != mail.QueuedDeliverySubject || pending[0].Body != body {
		t.Fatalf("pending did not show queued-durable: %+v", pending)
	}
	id := pending[0].ID

	out, err = runHerd(t, repo, env, "mail", "status", "--recipient", "worker", "--id", id, "--mail", mailFile)
	if err != nil {
		t.Fatalf("status queued: %v\n%s", err, out)
	}
	var status mail.OrdinaryStatus
	if err := json.Unmarshal(out, &status); err != nil {
		t.Fatalf("decode status: %v\n%s", err, out)
	}
	if status.Ordinary || !status.Queued || !status.Pending || status.Handled || status.Kind != mail.EnvelopeKindQueuedDurable {
		t.Fatalf("status before consume = %+v", status)
	}

	if out, err := runHerd(t, repo, env, "send", "worker", "--drain"); err == nil {
		t.Fatalf("drain while working must refuse, got %s", out)
	}

	if err := os.WriteFile(statusPath, []byte("idle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runHerd(t, repo, env, "send", "worker", "--drain")
	if err != nil || !strings.Contains(string(out), "acknowledged=true") {
		t.Fatalf("idle drain: %v\n%s", err, out)
	}
	if strings.Count(fakeCallLog(t, logPath), "agent prompt") != 1 {
		t.Fatalf("want one consumption prompt:\n%s", fakeCallLog(t, logPath))
	}

	out, err = runHerd(t, repo, env, "mail", "status", "--recipient", "worker", "--id", id, "--mail", mailFile)
	if err != nil {
		t.Fatalf("status consumed: %v\n%s", err, out)
	}
	if err := json.Unmarshal(out, &status); err != nil {
		t.Fatal(err)
	}
	if status.Queued || status.Pending || !status.Handled || status.Ordinary {
		t.Fatalf("status after consume = %+v", status)
	}

	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runHerd(t, repo, env, "send", "worker", "--drain")
	if err != nil {
		t.Fatalf("repeat drain: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "no pending durable envelopes") {
		t.Fatalf("repeat drain output = %s", out)
	}
	if strings.Contains(fakeCallLog(t, logPath), "agent prompt") {
		t.Fatalf("restart redelivered:\n%s", fakeCallLog(t, logPath))
	}

	out, err = runHerd(t, repo, env, "mail", "pending", "--recipient", "worker", "--mail", mailFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &pending); err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("consumed envelope still pending: %+v", pending)
	}
}
