package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyReceiptTombstoneRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".herd", "legacy-receipts.jsonl")
	rec := legacyReceiptTombstone{TaskRef: "FAC-320", TaskID: "task-320", Reason: "pre-FAC-318 launch; re-dispatch required", Actor: "test"}
	if err := appendLegacyReceiptTombstone(path, rec); err != nil {
		t.Fatal(err)
	}
	got, err := readLegacyReceiptTombstones(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := got["FAC-320"]
	if !ok || stored.Reason != rec.Reason || stored.TaskID != rec.TaskID || stored.Actor != rec.Actor {
		t.Fatalf("tombstone mismatch: %+v", got)
	}
}

func TestLegacyReceiptLogRejectsMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-receipts.jsonl")
	if err := appendLegacyReceiptTombstone(path, legacyReceiptTombstone{TaskRef: "FAC-1", Reason: "re-dispatch", Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("not-json\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readLegacyReceiptTombstones(path); err == nil {
		t.Fatal("malformed legacy receipt log unexpectedly accepted")
	}
}
