package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
