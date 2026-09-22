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
	if report.Changed != 1 || report.Kept != 4 || report.TotalRows != 5 {
		t.Fatalf("report shape: %+v", report)
	}
	if report.Edits[0].ID != "a5" || report.Edits[0].AssignedSequence != 5 {
		t.Fatalf("plan: %+v", report.Edits[0])
	}

	raw, err := report.MarshalPlan()
	if err != nil {
		t.Fatal(err)
	}
	act, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{
		Act: true, Actor: "op", Reason: "fixture",
		Plan: raw, PlanDigest: SequenceOrderPlanDigest(raw),
	})
	if err != nil {
		t.Fatalf("act: %v", err)
	}
	if act.Edits[0].AssignedSequence != 5 {
		t.Fatalf("applied plan: %+v", act.Edits[0])
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

func TestSequenceOrderRejectsStalePlanDigest(t *testing.T) {
	mb, _ := mailboxWith(t, seqRow(t, "a1", "alice", 1), seqRow(t, "a2", "alice", 1))
	report, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := report.MarshalPlan()
	if err != nil {
		t.Fatal(err)
	}
	_, err = mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{
		Act: true, Actor: "op", Plan: raw, PlanDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err == nil || !errors.Is(err, ErrSequenceOrderPlanDigest) {
		t.Fatalf("stale plan digest must refuse, got %v", err)
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

func TestSequenceOrderBoundsConfiguredRowCeiling(t *testing.T) {
	rows := []string{seqRow(t, "head", "alice", 10)}
	for i := 0; i < 5; i++ {
		rows = append(rows, seqRow(t, fmt.Sprintf("inv-%d", i), "alice", 1))
	}
	mb, path := mailboxWith(t, rows...)
	before, _ := os.ReadFile(path)
	_, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{MaxRows: 4})
	if err == nil || !errors.Is(err, ErrSequenceOrderBound) {
		t.Fatalf("row ceiling must refuse, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("bounded refuse must not mutate")
	}
}

func TestSequenceOrderRecovers517Inversions(t *testing.T) {
	rows := []string{
		seqRow(t, "h1", "alice", 1),
		seqRow(t, "h2", "alice", 2),
		seqRow(t, "h3", "alice", 3),
		seqRow(t, "h4", "alice", 4),
	}
	for i := 0; i < 517; i++ {
		rows = append(rows, seqRow(t, fmt.Sprintf("inv-%d", i), "alice", 1))
	}
	mb, _ := mailboxWith(t, rows...)
	cur, opts := sequenceOrderPage(t, mb, "alice")
	if _, err := mb.ReadBoundedControl(context.Background(), "alice", cur, opts); err == nil || !errors.Is(err, ErrStorageUnordered) {
		t.Fatalf("517-inversion fixture must refuse paging, got %v", err)
	}
	report, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Changed != 517 || report.TotalRows != 521 {
		t.Fatalf("changed=%d total=%d", report.Changed, report.TotalRows)
	}
	raw, err := report.MarshalPlan()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mb.RepairSequenceOrder(context.Background(), SequenceOrderRequest{
		Act: true, Actor: "op", Plan: raw, PlanDigest: SequenceOrderPlanDigest(raw),
	}); err != nil {
		t.Fatalf("act 517: %v", err)
	}
	cur, opts = sequenceOrderPage(t, mb, "alice")
	opts.Limit = 1000
	opts.MaxBytes = MaxBoundedPageBytes
	page, err := mb.ReadBoundedControl(context.Background(), "alice", cur, opts)
	if err != nil {
		t.Fatalf("bounded after 517 repair: %v", err)
	}
	if len(page.Envelopes) != 521 {
		t.Fatalf("skipped messages: got %d want 521", len(page.Envelopes))
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
