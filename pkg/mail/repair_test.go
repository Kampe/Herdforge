package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyRow is the exact shape that blocked ingest: a manually appended row
// whose timestamp offset lacks the RFC3339 colon AND whose key spacing differs
// from the canonical encoder, so ReadInbox quarantines it while fileHasID
// cannot see it.
const legacyRow = `{"id": "host-81751-1789141629774", "sender": "startup-fix", "recipient": "orchestrator", ` +
	`"subject": "finding", "body": "two defects", "read": false, "timestamp": "2026-09-11T10:47:09.000000-0500"}`

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// mailboxWith writes rows (in order) to a temp mailbox and returns it.
func mailboxWith(t *testing.T, rows ...string) (*Mailbox, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control-mail.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewMailbox(path), path
}

func goodRow(t *testing.T, id, recipient string, seq int64) string {
	t.Helper()
	env := Envelope{ID: id, Sequence: seq, Sender: "peer", Recipient: recipient, Subject: "s", Body: "b"}
	env.Timestamp = time.Now().UTC()
	data, err := json.Marshal(&env)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRepairIsReportOnlyByDefaultAndWritesNothing(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := mb.RepairMalformedRow(context.Background(), RepairRequest{ID: "host-81751-1789141629774", Actor: "op"})
	if err != nil {
		t.Fatalf("report-only repair: %v", err)
	}
	if plan.Applied {
		t.Fatal("report-only mode reported the repair as applied")
	}
	if plan.RepairedLine == "" || plan.OriginalLine != legacyRow {
		t.Fatalf("plan did not carry both exact byte forms: %+v", plan)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("report-only mode mutated the mailbox")
	}
}

func TestRepairNormalizesLegacyOffsetAndAssignsSequence(t *testing.T) {
	mb, _ := mailboxWith(t, legacyRow)
	plan, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op", Reason: "2982",
	})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !plan.Applied || plan.AssignedSequence <= 0 {
		t.Fatalf("repair did not apply with a monotonic sequence: %+v", plan)
	}
	// The instant must be preserved exactly, not re-clocked.
	if got := plan.RepairedTimestamp.UTC().Format("2006-01-02T15:04:05Z"); got != "2026-09-11T15:47:09Z" {
		t.Fatalf("repair moved the instant: %s", got)
	}
	envs, err := mb.ReadInbox("orchestrator")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var found *Envelope
	for _, e := range envs {
		if e.ID == "host-81751-1789141629774" {
			found = e
		}
	}
	if found == nil {
		t.Fatal("repaired row is still not visible to ReadInbox")
	}
	if found.Sender != "startup-fix" || found.Body != "two defects" || found.Subject != "finding" {
		t.Fatalf("repair did not preserve the payload: %+v", found)
	}
	if found.Sequence != plan.AssignedSequence {
		t.Fatalf("persisted sequence %d != planned %d", found.Sequence, plan.AssignedSequence)
	}
}

// The dedupe scan is the second half of the block: fileHasID looks for the
// COMPACT needle, so a repaired row that keeps the manual spacing stays
// invisible to redelivery detection even once it parses.
func TestRepairedRowIsVisibleToDedupeScan(t *testing.T) {
	mb, _ := mailboxWith(t, legacyRow)
	if mb.fileHasID("host-81751-1789141629774") {
		t.Fatal("fixture precondition: the malformed row was already dedupe-visible")
	}
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op",
	}); err != nil {
		t.Fatal(err)
	}
	if !mb.fileHasID("host-81751-1789141629774") {
		t.Fatal("repaired row is still invisible to the dedupe scan; redelivery cannot be recognized")
	}
}

func TestRepairPreservesEveryOtherRowByteForByte(t *testing.T) {
	keepA := goodRow(t, "keep-a", "orchestrator", 1)
	keepB := goodRow(t, "keep-b", "someone-else", 2)
	mb, path := mailboxWith(t, keepA, legacyRow, keepB)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op",
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("row count changed: %d", len(lines))
	}
	if lines[0] != keepA || lines[2] != keepB {
		t.Fatal("repair disturbed an unrelated row")
	}
	if lines[1] == legacyRow {
		t.Fatal("target row was not replaced in place")
	}
}

func TestRepairRefusesStaleFingerprint(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	_, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex("something else"), Act: true, Actor: "op",
	})
	if !errors.Is(err, ErrRepairStale) {
		t.Fatalf("stale fingerprint must refuse, got %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), legacyRow) {
		t.Fatal("a refused repair still mutated the mailbox")
	}
}

func TestRepairAcceptsMatchingFingerprint(t *testing.T) {
	mb, _ := mailboxWith(t, legacyRow)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	}); err != nil {
		t.Fatalf("matching fingerprint must be accepted: %v", err)
	}
}

func TestRepairRefusesAmbiguousDuplicateID(t *testing.T) {
	mb, _ := mailboxWith(t, legacyRow, legacyRow)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairAmbiguous) {
		t.Fatalf("duplicate malformed rows must refuse, got %v", err)
	}
}

func TestRepairRefusesWhenAWellFormedRowAlreadyCarriesTheID(t *testing.T) {
	mb, _ := mailboxWith(t, goodRow(t, "host-81751-1789141629774", "orchestrator", 7), legacyRow)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairAmbiguous) {
		t.Fatalf("an existing well-formed row with the same id must refuse, got %v", err)
	}
}

func TestRepairLeavesUnrelatedMalformedRowsUntouched(t *testing.T) {
	truncated := `{"id": "other-row", "sender": "x", "timestamp": "2026-09-1`
	mb, path := mailboxWith(t, truncated, legacyRow)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op",
	}); err != nil {
		t.Fatalf("an unrelated malformed row must not block the targeted repair: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), truncated) {
		t.Fatal("repair swept an unrelated malformed row it was not asked to touch")
	}
}

func TestRepairRefusesUnsupportedDefect(t *testing.T) {
	truncated := `{"id": "only-row", "sender": "x", "timestamp": "2026-09-1`
	mb, _ := mailboxWith(t, truncated)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "only-row", Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairUnsupported) {
		t.Fatalf("a defect other than the legacy timestamp must refuse, got %v", err)
	}
}

func TestRepairRefusesPrivilegedSignedControlMessage(t *testing.T) {
	privileged := `{"id": "priv-1", "sender": "control", "recipient": "orchestrator", "subject": "s", ` +
		`"body": "b", "signature": "abc123", "timestamp": "2026-09-11T10:47:09.000000-0500"}`
	mb, path := mailboxWith(t, privileged)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "priv-1", Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairPrivileged) {
		t.Fatalf("a signed control message must never be silently rewritten, got %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), privileged) {
		t.Fatal("a refused privileged repair still mutated the mailbox")
	}
}

func TestRepairAuditArtifactRetainsOriginalCorruptBytes(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	plan, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op", Reason: "2982",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path + ".repair.jsonl")
	if err != nil {
		t.Fatalf("no repair audit artifact: %v", err)
	}
	var rec RepairPlan
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.OriginalLine != legacyRow {
		t.Fatal("audit artifact did not retain the original corrupt bytes verbatim")
	}
	if rec.OriginalSHA256 != sha256Hex(legacyRow) || rec.RepairedSHA256 != plan.RepairedSHA256 {
		t.Fatalf("audit artifact digests do not bind the exact bytes: %+v", rec)
	}
}

func TestRepairRefusesWhenMailboxLockIsHeld(t *testing.T) {
	mb, _ := mailboxWith(t, legacyRow)
	mb.SetLockTimeout(150 * 1e6)
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = mb.withFileLock(func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	other := NewMailbox(mb.MailFile)
	other.SetLockTimeout(150 * 1e6)
	if _, err := other.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op",
	}); err == nil {
		t.Fatal("repair proceeded while another holder had the canonical mailbox lock")
	}
}

func TestRepairFailsClosedWhenDurableWriteFails(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	restore := fileSyncFn
	fileSyncFn = func(*os.File) error { return errors.New("injected fsync failure") }
	defer func() { fileSyncFn = restore }()

	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op",
	}); err == nil {
		t.Fatal("a failed durable write must fail closed")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), legacyRow) {
		t.Fatal("mailbox lost the original row after a failed durable write")
	}
}

// A copied mailbox, or one whose .seq sidecar was lost, must not have its
// repaired row filed beneath the history already in the file.
func TestRepairSequenceIsAboveExistingRowsWhenSidecarIsMissing(t *testing.T) {
	high := goodRow(t, "already-here", "orchestrator", 3179)
	mb, path := mailboxWith(t, high, legacyRow)
	if _, err := os.Stat(path + ".seq"); !os.IsNotExist(err) {
		t.Fatal("fixture precondition: sequence sidecar should be absent")
	}
	plan, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.AssignedSequence <= 3179 {
		t.Fatalf("repair filed beneath existing history: seq %d, highest existing 3179", plan.AssignedSequence)
	}
	// The durable counter must also be ahead, or the next ordinary send
	// collides with the row we just wrote.
	next, err := mb.peekNextSequence()
	if err != nil {
		t.Fatal(err)
	}
	if next <= plan.AssignedSequence {
		t.Fatalf("durable counter %d is not ahead of the repaired row %d", next, plan.AssignedSequence)
	}
}

// TestRepairFailsClosedWhenReadbackMismatches proves the durable readback is
// load-bearing, not decoration: if what lands on disk is not what the repair
// intended, the operation must report failure rather than claim success. The
// injected writer stands in for a truncated or racing write.
func TestRepairFailsClosedWhenReadbackMismatches(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	restore := writeFileAtomicFn
	writeFileAtomicFn = func(p string, data []byte, perm os.FileMode) error {
		if p == path {
			// Land a well-formed but DIFFERENT row than the one planned.
			data = []byte(`{"id":"host-81751-1789141629774","seq":1,"sender":"tampered",` +
				`"recipient":"orchestrator","subject":"finding","body":"two defects",` +
				`"read":false,"timestamp":"2026-09-11T10:47:09-05:00"}` + "\n")
		}
		return restore(p, data, perm)
	}
	defer func() { writeFileAtomicFn = restore }()

	plan, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op",
	})
	if !errors.Is(err, ErrRepairReadbackFailed) {
		t.Fatalf("a durable row that differs from the repaired row must fail closed, got plan=%+v err=%v", plan, err)
	}
	// The original corrupt bytes must still be recoverable from the audit
	// artifact even though the mailbox write went wrong.
	raw, readErr := os.ReadFile(path + ".repair.jsonl")
	if readErr != nil {
		t.Fatalf("audit artifact missing after a failed repair: %v", readErr)
	}
	if !strings.Contains(string(raw), "2026-09-11T10:47:09.000000-0500") {
		t.Fatal("audit artifact did not retain the original corrupt bytes after a failed repair")
	}
}
