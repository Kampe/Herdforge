package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/mail"
)

func TestMailRepairCLISequenceOrderReportAndAct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-mail.jsonl")
	rows := make([]string, 0, 5)
	for i, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		seq := int64(i + 1)
		if id == "a5" {
			seq = 1
		}
		env := mail.Envelope{
			ID: id, Sequence: seq, Sender: "peer", Recipient: "alice",
			Subject: "s", Body: "b", Timestamp: time.Date(2026, 9, 22, 0, 0, int(seq), 0, time.UTC),
		}
		raw, err := json.Marshal(&env)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, string(raw))
	}
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	planPath := filepath.Join(t.TempDir(), "plan.json")
	code, stdout, stderr := runRepairCLI("--sequence-order", "--mail", path, "--plan-out", planPath)
	if code != 0 {
		t.Fatalf("report-only exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stderr, "REPORT ONLY") {
		t.Fatalf("report-only must say so, stderr=%s", stderr)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("decode summary: %v %s", err, stdout)
	}
	if summary["changed"] != float64(1) {
		t.Fatalf("summary %+v", summary)
	}
	digest, _ := summary["plan_sha256"].(string)
	if digest == "" {
		t.Fatal("missing plan_sha256")
	}

	code, _, stderr = runRepairCLI(
		"--sequence-order", "--mail", path,
		"--act", "--actor", "op",
		"--plan-file", planPath, "--plan-digest", digest,
	)
	if code != 0 {
		t.Fatalf("act exit %d stderr %s", code, stderr)
	}

	fb := t.TempDir()
	source, err := mail.SourceFingerprint(path, fb)
	if err != nil {
		t.Fatal(err)
	}
	page, err := mail.NewMailbox(path).ReadBoundedControl(context.Background(), "alice", mail.Cursor{Recipient: "alice", Source: source}, mail.BoundedOptions{Limit: 6, MaxBytes: 18000, FeedbackDir: fb})
	if err != nil {
		t.Fatalf("bounded after CLI act: %v", err)
	}
	if len(page.Envelopes) != 5 {
		t.Fatalf("got %d envelopes", len(page.Envelopes))
	}
}

func TestMailRepairCLISequenceOrderRejectsIDMix(t *testing.T) {
	code, _, stderr := runRepairCLI("--sequence-order", "--id", "x")
	if code != 2 || !strings.Contains(stderr, "cannot be combined with --id") {
		t.Fatalf("exit %d stderr %s", code, stderr)
	}
}
