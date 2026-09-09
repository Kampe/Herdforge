package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/mail"
)

func TestWarnUnknownMailParticipants(t *testing.T) {
	tests := []struct {
		name         string
		sender       string
		recipient    string
		agents       []herdr.AgentEntry
		wantWarnings []string
	}{
		{
			name:      "both participants are known",
			sender:    "coordinator",
			recipient: "forge-worker",
			agents: []herdr.AgentEntry{
				{Name: "coordinator", Status: "working"},
				{Name: "forge-worker", Status: "done"},
			},
		},
		{
			name:         "unknown recipient is warned but permitted",
			sender:       "coordinator",
			recipient:    "typo-worker",
			agents:       []herdr.AgentEntry{{Name: "coordinator", Status: "working"}},
			wantWarnings: []string{"recipient \"typo-worker\"", "message will still be filed"},
		},
		{
			name:         "unknown sender and recipient are both diagnosed",
			sender:       "old-coordinator",
			recipient:    "old-worker",
			agents:       []herdr.AgentEntry{{Name: "forge-worker", Status: "idle"}},
			wantWarnings: []string{"sender \"old-coordinator\"", "recipient \"old-worker\""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := listMailAgents
			defer func() { listMailAgents = restore }()
			listMailAgents = func() ([]herdr.AgentEntry, error) { return tt.agents, nil }

			output := captureStderr(t, func() {
				warnUnknownMailParticipants(tt.sender, tt.recipient)
			})
			for _, warning := range tt.wantWarnings {
				if !strings.Contains(output, warning) {
					t.Fatalf("warning output %q does not contain %q", output, warning)
				}
			}
			if len(tt.wantWarnings) == 0 && output != "" {
				t.Fatalf("known participants produced warning: %q", output)
			}
		})
	}
}

func TestWarnUnknownMailParticipantsKeepsSendPermissiveWhenHerdrUnavailable(t *testing.T) {
	restore := listMailAgents
	defer func() { listMailAgents = restore }()
	listMailAgents = func() ([]herdr.AgentEntry, error) { return nil, os.ErrNotExist }

	output := captureStderr(t, func() {
		warnUnknownMailParticipants("coordinator", "future-lane")
	})
	if !strings.Contains(output, "recipient liveness unavailable") {
		t.Fatalf("missing unavailable-census diagnostic: %q", output)
	}
	if strings.Contains(output, "message will still be filed") {
		t.Fatalf("unavailable census was reported as a definitive unknown: %q", output)
	}
}

func TestWarnUnknownMailParticipantsExplainsLivePaneSplit(t *testing.T) {
	restore := listMailAgents
	defer func() { listMailAgents = restore }()
	listMailAgents = func() ([]herdr.AgentEntry, error) {
		return []herdr.AgentEntry{
			{Name: "coordinator", Status: "working", PaneID: "pane-1"},
			{Name: "worker", Status: "idle", PaneID: "pane-2"},
		}, nil
	}

	output := captureStderr(t, func() {
		warnUnknownMailParticipants("worker", "coordinator")
	})
	for _, want := range []string{"recipient \"coordinator\" has a live pane", "durable-only", "herd send"} {
		if !strings.Contains(output, want) {
			t.Fatalf("live-pane hint %q missing from %q", want, output)
		}
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = original }()
	fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return string(output)
}

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
	requireHerdrForLiveTest(t)
	dir := sandbox(t)
	mailFile := filepath.Join(dir, ".herd", "mail.jsonl")
	out, stderr, err := runHerdWithSeparateOutput(t, dir, nil, "mail", "send", "--from", "coordinator", "--to", "worker-a", "--subject", "handoff", "--body", "ready", "--mail", mailFile)
	if err != nil {
		t.Fatalf("herd mail send: %v\nstdout=%s\nstderr=%s", err, out, stderr)
	}
	if !strings.Contains(string(stderr), `recipient "worker-a"`) {
		t.Fatalf("herd mail send did not preserve unknown-recipient warning on stderr: %s", stderr)
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

func TestMailAckCLIReadThenAckSuppressesWake(t *testing.T) {
	proc := startFakeCommand(t)
	repo := queuedSendRepo(t)
	bin, logPath, _ := installQueuedSendFake(t, "idle", strconv.Itoa(proc.Pid))
	env := queuedSendEnv(bin, repo)
	mailFile := filepath.Join(repo, ".herd", "control-mail.jsonl")
	body := "report remains pending after read"

	out, _, err := runHerdWithSeparateOutput(t, repo, env, "mail", "send", "--from", "worker", "--to", "worker", "--subject", "FAC-773 report", "--body", body, "--mail", mailFile)
	if err != nil {
		t.Fatalf("mail send: %v\n%s", err, out)
	}
	var sent mail.Envelope
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatalf("decode sent envelope: %v\n%s", err, out)
	}

	out, err = runHerd(t, repo, env, "mail", "read", "--recipient", "worker", "--mail", mailFile)
	if err != nil || !strings.Contains(string(out), body) {
		t.Fatalf("mail read: %v\n%s", err, out)
	}
	box := mail.NewMailbox(mailFile)
	pending, err := box.PendingRoutine("worker")
	if err != nil || len(pending) != 1 || pending[0].ID != sent.ID {
		t.Fatalf("read changed pending state: pending=%+v err=%v", pending, err)
	}

	out, err = runHerd(t, repo, env, "mail", "ack", "--recipient", "worker", "--id", sent.ID, "--mail", mailFile)
	if err != nil || !strings.Contains(string(out), "handled "+sent.ID) {
		t.Fatalf("mail ack: %v\n%s", err, out)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runHerd(t, repo, env, "watch", "--wake", "--recipient", "worker", "--workspace", "wK", "--interval", "1", "--timeout", "1")
	if exitCode(err) != 2 {
		t.Fatalf("post-ack watch exit=%d, want bounded timeout; output=%s", exitCode(err), out)
	}
	log := fakeCallLog(t, logPath)
	if strings.Contains(log, "pane read") || strings.Contains(log, "agent prompt") || strings.Contains(log, "agent send-keys") {
		t.Fatalf("acknowledged report caused pane activity:\n%s", log)
	}
	handled, err := box.Handled("worker", sent.ID)
	if err != nil || !handled {
		t.Fatalf("ack state = %t, %v", handled, err)
	}
}

func TestMailAckCLIFailsClosedAndLeavesEnvelopeUnacknowledged(t *testing.T) {
	tests := []struct {
		name       string
		subject    string
		recipient  string
		ackID      string
		envelopeID string
		makeBroken bool
	}{
		{name: "wrong recipient", subject: "FAC-773 report", recipient: "other", ackID: "report-id"},
		{name: "missing id", subject: "FAC-773 report", recipient: "worker", ackID: "missing-id", envelopeID: "present-id"},
		{name: "control envelope", subject: mail.ControlSubjectPrefix + " issue", recipient: "worker", ackID: "control-id"},
		{name: "callback envelope", subject: "complete: FAC-773", recipient: "worker", ackID: "callback-id"},
		{name: "failed ack write", subject: "FAC-773 report", recipient: "worker", ackID: "report-id", makeBroken: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := queuedSendRepo(t)
			mailFile := filepath.Join(repo, ".herd", "control-mail.jsonl")
			box := mail.NewMailbox(mailFile)
			envelopeID := tt.envelopeID
			if envelopeID == "" {
				envelopeID = tt.ackID
			}
			if err := box.AppendEnvelopeContext(context.Background(), &mail.Envelope{
				ID: envelopeID, Sender: "worker", Recipient: "worker", Subject: tt.subject, Body: "payload",
			}); err != nil {
				t.Fatal(err)
			}
			if tt.makeBroken {
				if err := os.Mkdir(mail.HandledStatePath(mailFile), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			out, err := runHerd(t, repo, nil, "mail", "ack", "--recipient", tt.recipient, "--id", tt.ackID, "--mail", mailFile)
			if err == nil {
				t.Fatalf("ack unexpectedly succeeded: %s", out)
			}
			handled, hErr := box.Handled("worker", tt.ackID)
			if hErr != nil {
				if !tt.makeBroken {
					t.Fatal(hErr)
				}
			} else if handled {
				t.Fatalf("rejected envelope was acknowledged")
			}
		})
	}
}

// runHerdWithSeparateOutput keeps machine-readable stdout independent from
// diagnostics. Mail send deliberately warns about unknown participants on
// stderr, and callers must not have that warning corrupt a JSON response.
func runHerdWithSeparateOutput(t *testing.T, dir string, env []string, args ...string) ([]byte, []byte, error) {
	t.Helper()
	cmd := exec.Command(buildHerd(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func TestMailCLIEmptyInboxUsesEmptyJSONArray(t *testing.T) {
	dir := sandbox(t)
	mailFile := filepath.Join(dir, ".herd", "mail.jsonl")
	out, err := runHerd(t, dir, nil, "mail", "inbox", "--recipient", "never-seen", "--mail", mailFile)
	if err != nil {
		t.Fatalf("herd mail inbox: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `mail inbox: recipient "never-seen" has no mailbox history`) || !bytes.HasSuffix(out, []byte("[]\n")) {
		t.Fatalf("empty inbox output = %q, want an unseen-recipient warning followed by []", out)
	}
}

func TestMailCLISendPreservesPayloadSourcesByteForByte(t *testing.T) {
	const body = "literal `identifier` and $(printf truncated)\nsecond line"

	tests := []struct {
		name  string
		args  func(mailFile, payloadFile string) []string
		stdin bool
	}{
		{
			name: "file",
			args: func(mailFile, payloadFile string) []string {
				return []string{"mail", "send", "--from", "coordinator", "--to", "worker-file", "--file", payloadFile, "--mail", mailFile}
			},
		},
		{
			name: "stdin",
			args: func(mailFile, _ string) []string {
				return []string{"mail", "send", "--from", "coordinator", "--to", "worker-stdin", "--mail", mailFile}
			},
			stdin: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := sandbox(t)
			mailFile := filepath.Join(dir, ".herd", "mail.jsonl")
			payloadFile := filepath.Join(dir, "payload.txt")
			if err := os.WriteFile(payloadFile, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdin []byte
			if tt.stdin {
				stdin = []byte(body)
			}
			if out, err := runHerdWithStdin(t, dir, stdin, tt.args(mailFile, payloadFile)...); err != nil {
				t.Fatalf("herd mail send: %v\n%s", err, out)
			}

			out, err := runHerd(t, dir, nil, "mail", "inbox", "--recipient", map[bool]string{true: "worker-stdin", false: "worker-file"}[tt.stdin], "--mail", mailFile)
			if err != nil {
				t.Fatalf("herd mail inbox: %v\n%s", err, out)
			}
			var inbox []struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(out, &inbox); err != nil {
				t.Fatalf("decode inbox output: %v\n%s", err, out)
			}
			if len(inbox) != 1 || inbox[0].Body != body {
				t.Fatalf("payload was not delivered byte-for-byte: %+v", inbox)
			}
		})
	}
}

func runHerdWithStdin(t *testing.T, dir string, stdin []byte, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(buildHerd(t), args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd.CombinedOutput()
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
