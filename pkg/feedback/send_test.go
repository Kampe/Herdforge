package feedback

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultDurableMailWritesHerdMailShapeAndAssignsMonotonicIDs(t *testing.T) {
	mailDir := t.TempDir()
	send := DefaultDurableMail(mailDir, "coordinator-1")
	if err := send(context.Background(), "smith", "FLEET_FEEDBACK E1", "body one"); err != nil {
		t.Fatal(err)
	}
	if err := send(context.Background(), "smith", "FLEET_FEEDBACK E1", "body two"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(mailDir, "smith.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %s", len(lines), raw)
	}
	var first, second mailEnvelope
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first.ID != 1 || second.ID != 2 {
		t.Fatalf("ids = %d, %d, want 1, 2", first.ID, second.ID)
	}
	if first.From != "coordinator-1" || first.To != "smith" || first.Type != "message" {
		t.Fatalf("envelope shape drifted: %+v", first)
	}
	if first.Summary != "FLEET_FEEDBACK E1" || first.Message != "body one" {
		t.Fatalf("summary/body not preserved verbatim: %+v", first)
	}
}

func TestDefaultDurableMailRequiresRecipient(t *testing.T) {
	send := DefaultDurableMail(t.TempDir(), "coordinator-1")
	if err := send(context.Background(), "", "s", "b"); err == nil {
		t.Fatal("an empty recipient lane must be rejected")
	}
}

func TestRecordReplyIsCountedByReplyFromLanes(t *testing.T) {
	mailDir := t.TempDir()
	if err := RecordReply(context.Background(), mailDir, "lane-a", "coordinator", "FLEET_FEEDBACK E1 lane-a blocker=NONE"); err != nil {
		t.Fatal(err)
	}
	got, missing, err := ReplyFromLanes(filepath.Join(mailDir, "coordinator.jsonl"), "E1", []string{"lane-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "lane-a" {
		t.Fatalf("got replies = %v, want [lane-a]", got)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}
}

func TestRecordReplyIgnoresOrdinarySend(t *testing.T) {
	mailDir := t.TempDir()
	if err := RecordReply(context.Background(), mailDir, "lane-a", "coordinator", "ordinary prompt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mailDir, "coordinator.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("ordinary send created durable reply inbox: %v", err)
	}
}

func TestDefaultWakeUsesConfiguredBinaryWhenSet(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-send.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	wake := DefaultWake(script)
	if err := wake(context.Background(), "lane-1", "nudge"); err != nil {
		t.Fatalf("wake() = %v, want nil", err)
	}
}

func TestDefaultWakeSurfacesConfiguredBinaryFailure(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-send.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	wake := DefaultWake(script)
	if err := wake(context.Background(), "lane-1", "nudge"); err == nil {
		t.Fatal("a failing configured send binary must surface an error")
	}
}
