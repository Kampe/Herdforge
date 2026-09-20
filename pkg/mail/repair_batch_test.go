package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

const secondRepairID = "host-309-1789142802839"

func repairBatchFixture() (string, []RepairRequest) {
	second := strings.ReplaceAll(legacyRow, "host-81751-1789141629774", secondRepairID)
	second = strings.ReplaceAll(second, "10:47:09", "11:06:42")
	return second, []RepairRequest{
		{ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Actor: "op", Act: true},
		{ID: secondRepairID, Fingerprint: sha256Hex(second), Actor: "op", Act: true},
	}
}

func TestRepairBatchAppliesTogetherAndPreservesFraming(t *testing.T) {
	second, requests := repairBatchFixture()
	keep := goodRow(t, "keep", "elsewhere", 3179)
	before := "\n" + legacyRow + "\n\n" + keep + "\r\n" + second
	mb, path := rawMailbox(t, before)
	// Request order differs from physical row order; pairing must be explicit.
	requests[0], requests[1] = requests[1], requests[0]
	for i := range requests {
		requests[i].Act = false
		requests[i].Fingerprint = ""
	}
	plans, err := mb.RepairMalformedRows(context.Background(), requests)
	if err != nil || len(plans) != 2 {
		t.Fatalf("batch report failed: plans=%v err=%v", plans, err)
	}
	assertNoRepairSideEffects(t, path, []byte(before), "", false)
	for i, plan := range plans {
		if plan.Applied || plan.ID != requests[i].ID || plan.AssignedSequence != int64(3180+i) {
			t.Fatalf("batch report lost selection or sequence order: %+v", plan)
		}
		requests[i].Act, requests[i].Fingerprint = true, plan.OriginalSHA256
	}
	// Neither singleton can recover this mailbox by itself.
	if _, err := mb.RepairMalformedRow(context.Background(), requests[0]); !errors.Is(err, ErrRepairUnrelatedCorruption) {
		t.Fatalf("single selection must refuse its malformed neighbour: %v", err)
	}
	writes := 0
	restore := writeFileAtomicFn
	writeFileAtomicFn = func(p string, data []byte, perm os.FileMode) error {
		if p == path {
			writes++
			records := readRepairRecords(t, path)
			if len(records) != 2 || records[0].OriginalLine != second || records[1].OriginalLine != legacyRow {
				t.Fatalf("both exact originals must be durable before replacement: %+v", records)
			}
			for _, record := range records {
				if record.Phase != RepairPhasePrepare || record.Applied || sha256Hex(record.OriginalLine) != record.OriginalSHA256 {
					t.Fatalf("invalid prepare evidence: %+v", record)
				}
			}
		}
		return restore(p, data, perm)
	}
	t.Cleanup(func() { writeFileAtomicFn = restore })
	plans, err = mb.RepairMalformedRows(context.Background(), requests)
	if err != nil || len(plans) != 2 {
		t.Fatalf("batch apply failed: plans=%v err=%v", plans, err)
	}
	if writes != 1 {
		t.Fatalf("batch must replace the mailbox once, got %d writes", writes)
	}
	want := strings.Replace(before, legacyRow, plans[1].RepairedLine, 1)
	want = strings.Replace(want, second, plans[0].RepairedLine, 1)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("batch changed framing or unselected bytes: err=%v", err)
	}
	for _, plan := range plans {
		if !plan.Applied || plan.Outcome != RepairOutcomeApplied {
			t.Fatalf("batch result was not applied: %+v", plan)
		}
	}
	envelopes, err := mb.ReadInbox("orchestrator")
	if err != nil || len(envelopes) != 2 {
		t.Fatalf("strict inbox remains blocked: rows=%d err=%v", len(envelopes), err)
	}
	for _, env := range envelopes {
		if env.Body != "two defects" || env.Sender != "startup-fix" || env.Subject != "finding" {
			t.Fatalf("payload changed: %+v", env)
		}
		wantTime := "2026-09-11T15:47:09Z"
		if env.ID == secondRepairID {
			wantTime = "2026-09-11T16:06:42Z"
		}
		if env.Timestamp.UTC().Format("2006-01-02T15:04:05Z") != wantTime {
			t.Fatalf("timestamp instant changed: %+v", env)
		}
	}
	if records := readRepairRecords(t, path); len(records) != 4 {
		t.Fatalf("want prepare and result for both rows, got %d", len(records))
	}
	next, err := mb.nextSequenceLocked()
	if err != nil || next != 3182 {
		t.Fatalf("next send could reuse a repaired sequence: next=%d err=%v", next, err)
	}
}

func TestRepairBatchRefusesUnsafeSelectionWithoutMutation(t *testing.T) {
	for _, name := range []string{"third-malformed", "unknown-identity", "stale-second", "privileged-second", "duplicate-row", "duplicate-key", "unknown-id", "duplicate-selection", "missing-fingerprint", "mixed-act", "range-overflow"} {
		t.Run(name, func(t *testing.T) {
			second, requests := repairBatchFixture()
			rows := []string{legacyRow, second}
			wantErr := ErrRepairUnrelatedCorruption
			seq := "100"
			switch name {
			case "third-malformed":
				rows = append(rows, strings.Replace(second, secondRepairID, "third", 1))
			case "unknown-identity":
				rows = append(rows, `{"body":"unreadable owner"}`)
			case "stale-second":
				requests[1].Fingerprint = sha256Hex("old bytes")
				wantErr = ErrRepairStale
			case "privileged-second":
				rows[1] = strings.Replace(second, `"read": false`, `"read": false, "signature":"authority"`, 1)
				requests[1].Fingerprint = sha256Hex(rows[1])
				wantErr = ErrRepairPrivileged
			case "duplicate-row":
				rows = append(rows, second)
				wantErr = ErrRepairAmbiguous
			case "duplicate-key":
				rows = append(rows, strings.Replace(second, `"read": false`, `"read": false, "read": true`, 1))
				wantErr = ErrRepairDuplicateKeys
			case "unknown-id":
				requests[1].ID = "absent"
			case "duplicate-selection":
				requests[1] = requests[0]
				wantErr = ErrRepairAmbiguous
			case "missing-fingerprint":
				requests[1].Fingerprint = ""
				wantErr = ErrRepairFingerprintRequired
			case "mixed-act":
				requests[1].Act = false
				wantErr = nil
			case "range-overflow":
				seq = "9223372036854775806"
				wantErr = ErrSequenceExhausted
			}
			mb, path := mailboxWith(t, rows...)
			writeSeq(t, path, seq)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = mb.RepairMalformedRows(context.Background(), requests)
			if err == nil || (wantErr != nil && !errors.Is(err, wantErr)) {
				t.Fatalf("unsafe batch was not refused at its guard: got %v, want %v", err, wantErr)
			}
			assertNoRepairSideEffects(t, path, before, seq, true)
		})
	}
}

func TestRepairBatchRefusesAnOversizedSelection(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	requests := make([]RepairRequest, MaxRepairBatch+1)
	for i := range requests {
		requests[i].ID = strings.Repeat("x", i+1)
	}
	_, err := mb.RepairMalformedRows(context.Background(), requests)
	if err == nil || !strings.Contains(err.Error(), "select between") {
		t.Fatalf("oversized selection reached mailbox selection: %v", err)
	}
	assertNoRepairSideEffects(t, path, []byte(legacyRow+"\n"), "", false)
}

func TestRepairBatchSecondPrepareFailureLeavesMailboxUntouched(t *testing.T) {
	second, requests := repairBatchFixture()
	mb, path := mailboxWith(t, legacyRow, second)
	before := []byte(legacyRow + "\n" + second + "\n")
	restore := fileSyncFn
	prepares := 0
	fileSyncFn = func(f *os.File) error {
		if f.Name() == mb.RepairAuditPath() {
			prepares++
			if prepares == 2 {
				return errors.New("injected second prepare failure")
			}
		}
		return restore(f)
	}
	t.Cleanup(func() { fileSyncFn = restore })
	_, err := mb.RepairMalformedRows(context.Background(), requests)
	if err == nil || prepares != 2 || !strings.Contains(err.Error(), "injected second prepare failure") {
		t.Fatalf("prepare failure was not exercised: prepares=%d err=%v", prepares, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, before) {
		t.Fatalf("mailbox changed before every prepare was durable: %v", err)
	}
}

func TestRepairBatchReadbackRefusesAlteredSecondRow(t *testing.T) {
	second, requests := repairBatchFixture()
	mb, path := mailboxWith(t, legacyRow, second)
	tamperWriter(t, path, func(data []byte) []byte {
		return bytes.Replace(data, []byte(`"id":"`+secondRepairID+`"`), []byte(`"id":"changed"`), 1)
	})
	_, err := mb.RepairMalformedRows(context.Background(), requests)
	if !errors.Is(err, ErrRepairReadbackFailed) {
		t.Fatalf("batch readback accepted changed bytes: %v", err)
	}
	records := readRepairRecords(t, path)
	if len(records) != 4 {
		t.Fatalf("batch failure lost audit evidence: %d records", len(records))
	}
	for _, record := range records[2:] {
		if record.Outcome != RepairOutcomeFailed || record.Applied {
			t.Fatalf("failed batch claimed success: %+v", record)
		}
	}
}

func TestRepairBatchCompletionFailurePreservesBothOriginals(t *testing.T) {
	second, requests := repairBatchFixture()
	mb, path := mailboxWith(t, legacyRow, second)
	restore := fileSyncFn
	audits := 0
	fileSyncFn = func(f *os.File) error {
		if f.Name() == mb.RepairAuditPath() {
			audits++
			if audits == 4 {
				return errors.New("injected second result failure")
			}
		}
		return restore(f)
	}
	t.Cleanup(func() { fileSyncFn = restore })
	_, err := mb.RepairMalformedRows(context.Background(), requests)
	if !errors.Is(err, ErrRepairCompletionUnrecorded) || audits != 4 {
		t.Fatalf("batch completion failure was not reported: audits=%d err=%v", audits, err)
	}
	records := readRepairRecords(t, path)
	if len(records) < 2 || records[0].OriginalLine != legacyRow || records[1].OriginalLine != second {
		t.Fatalf("completion failure lost original bytes: %+v", records)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range splitLines(string(got)) {
		var env Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("completion failure preceded the batch replacement: %v", err)
		}
	}
}
