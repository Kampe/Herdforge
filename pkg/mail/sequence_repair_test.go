package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func seqRow(t *testing.T, id, recipient string, seq int64) string {
	t.Helper()
	env := Envelope{
		ID: id, Sequence: seq, Sender: "peer", Recipient: recipient,
		Subject: "s", Body: "b", Timestamp: time.Date(2026, 9, 22, 0, 0, int(seq), 0, time.UTC),
	}
	data, err := json.Marshal(&env)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sequenceOrderPage(t *testing.T, mb *Mailbox, recipient string) (Cursor, BoundedOptions) {
	t.Helper()
	fb := t.TempDir()
	source, err := SourceFingerprint(mb.MailFile, fb)
	if err != nil {
		t.Fatal(err)
	}
	return Cursor{Recipient: recipient, Source: source}, BoundedOptions{Limit: 6, MaxBytes: 18000, FeedbackDir: fb}
}

func TestSequenceOrderReportOnlyThenActOnDisposableInversion(t *testing.T) {
	row1 := seqRow(t, "a1", "alice", 1)
	row2 := seqRow(t, "a2", "alice", 2)
	row3 := seqRow(t, "a3", "alice", 3)
	row4 := seqRow(t, "a4", "alice", 4)
	row5 := seqRow(t, "a5", "alice", 1)
	mb, path := mailboxWith(t, row1, row2, row3, row4, row5)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cur, opts := sequenceOrderPage(t, mb, "alice")
	_, err = mb.ReadBoundedControl(context.Background(), "alice", cur, opts)
	if err == nil || !errors.Is(err, ErrStorageUnordered) {
		t.Fatalf("disposable inversion must refuse bounded paging, got %v", err)
	}

	report, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{Reason: "fixture"})
	if err != nil {
		t.Fatalf("report-only: %v", err)
	}
	afterReport, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterReport) != string(before) {
		t.Fatal("report-only must not mutate the mailbox")
	}
	if report.Changed != 1 || report.Kept != 4 || report.TotalRows != 5 || report.Truncated {
		t.Fatalf("report shape: %+v", report)
	}
	if report.Plans[0].ID != "a5" || report.Plans[0].AssignedSequence != 5 || report.Plans[0].Applied {
		t.Fatalf("plan: %+v", report.Plans[0])
	}

	act, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{
		Act: true, Actor: "op", Reason: "fixture",
		Fingerprints: []string{report.Plans[0].OriginalSHA256},
	})
	if err != nil {
		t.Fatalf("act: %v", err)
	}
	if !act.Plans[0].Applied || act.Plans[0].AssignedSequence != 5 {
		t.Fatalf("applied plan: %+v", act.Plans[0])
	}

	cur, opts = sequenceOrderPage(t, mb, "alice")
	page, err := mb.ReadBoundedControl(context.Background(), "alice", cur, opts)
	if err != nil {
		t.Fatalf("bounded after repair: %v", err)
	}
	if len(page.Envelopes) != 5 {
		t.Fatalf("skipped messages: got %d envelopes", len(page.Envelopes))
	}
	var last int64
	ids := map[string]bool{}
	for _, env := range page.Envelopes {
		if env.Sequence <= last {
			t.Fatalf("file-order seq not restored: %d after %d", env.Sequence, last)
		}
		last = env.Sequence
		ids[env.ID] = true
	}
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		if !ids[id] {
			t.Fatalf("missing id %s after repair", id)
		}
	}
}

func TestSequenceOrderRejectsDuplicateIDs(t *testing.T) {
	mb, _ := mailboxWith(t, seqRow(t, "dup", "alice", 1), seqRow(t, "dup", "alice", 2))
	_, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{})
	if err == nil || !errors.Is(err, ErrRepairAmbiguous) {
		t.Fatalf("duplicate ids must refuse, got %v", err)
	}
}

func TestSequenceOrderPreservesSignedControls(t *testing.T) {
	signed := `{"id":"signed-1","seq":1,"sender":"peer","recipient":"alice","subject":"s","body":"b","read":false,"timestamp":"2026-09-22T00:00:01Z","signature":"deadbeef"}`
	mb, path := mailboxWith(t, seqRow(t, "ok", "alice", 4), signed)
	before, _ := os.ReadFile(path)
	_, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{Act: true, Actor: "op"})
	if err == nil || !errors.Is(err, ErrRepairPrivileged) {
		t.Fatalf("inverting signed row must refuse, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("privileged refuse must leave mailbox bytes alone")
	}
}

func TestSequenceOrderRejectsStaleFingerprint(t *testing.T) {
	mb, _ := mailboxWith(t, seqRow(t, "a1", "alice", 1), seqRow(t, "a2", "alice", 1))
	report, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{
		Act: true, Actor: "op", Fingerprints: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	if err == nil || !errors.Is(err, ErrRepairStale) {
		t.Fatalf("stale fingerprint must refuse, got %v", err)
	}
	if report.Changed != 1 {
		t.Fatalf("changed = %d", report.Changed)
	}
}

func TestSequenceOrderRejectsStaleCursor(t *testing.T) {
	mb, path := mailboxWith(t, seqRow(t, "a1", "alice", 1), seqRow(t, "a2", "alice", 1))
	source, err := SourceFingerprint(path, "")
	if err != nil {
		t.Fatal(err)
	}
	cur := Cursor{Recipient: "alice", Source: source, Control: 99, ControlAnchor: "deadbeef"}
	_, err = mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{
		Cursor: cur.String(), Recipient: "alice",
	})
	if err == nil || !errors.Is(err, ErrSequenceOrderStaleCursor) {
		t.Fatalf("stale cursor must refuse, got %v", err)
	}
}

func TestSequenceOrderBoundsLargePlans(t *testing.T) {
	rows := make([]string, 0, MaxSequenceOrderRepairs+2)
	rows = append(rows, seqRow(t, "head", "alice", 10))
	for i := 0; i < MaxSequenceOrderRepairs+1; i++ {
		rows = append(rows, seqRow(t, fmt.Sprintf("inv-%d", i), "alice", 1))
	}
	mb, path := mailboxWith(t, rows...)
	before, _ := os.ReadFile(path)
	report, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{})
	if err != nil {
		t.Fatalf("report-only over bound: %v", err)
	}
	if !report.Truncated || report.Changed != MaxSequenceOrderRepairs+1 || len(report.Plans) != MaxSequenceOrderRepairs {
		t.Fatalf("truncated report: changed=%d plans=%d truncated=%v", report.Changed, len(report.Plans), report.Truncated)
	}
	_, err = mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{Act: true, Actor: "op"})
	if err == nil || !errors.Is(err, ErrSequenceOrderBound) {
		t.Fatalf("act over bound must refuse, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("bounded refuse must not mutate")
	}
}

func TestSequenceOrderKeepsMonotonicSignedRow(t *testing.T) {
	signed := `{"id":"signed-ok","seq":1,"sender":"peer","recipient":"alice","subject":"s","body":"b","read":false,"timestamp":"2026-09-22T00:00:01Z","signature":"deadbeef"}`
	mb, path := mailboxWith(t, signed, seqRow(t, "later", "alice", 2))
	before, _ := os.ReadFile(path)
	report, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{Act: true, Actor: "op"})
	if err != nil {
		t.Fatalf("monotonic signed row must be kept, got %v", err)
	}
	if report.Changed != 0 {
		t.Fatalf("changed = %d, want 0", report.Changed)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("clean signed store must keep exact bytes")
	}
}
