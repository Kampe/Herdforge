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
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op", Reason: "2982",
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
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
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
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
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
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairAmbiguous) {
		t.Fatalf("duplicate malformed rows must refuse, got %v", err)
	}
}

func TestRepairRefusesWhenAWellFormedRowAlreadyCarriesTheID(t *testing.T) {
	mb, _ := mailboxWith(t, goodRow(t, "host-81751-1789141629774", "orchestrator", 7), legacyRow)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairAmbiguous) {
		t.Fatalf("an existing well-formed row with the same id must refuse, got %v", err)
	}
}

func TestRepairLeavesUnrelatedMalformedRowsUntouched(t *testing.T) {
	truncated := `{"id": "other-row", "sender": "x", "timestamp": "2026-09-1`
	mb, path := mailboxWith(t, truncated, legacyRow)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
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
		ID: "only-row", Fingerprint: sha256Hex(truncated), Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairUnsupported) {
		t.Fatalf("a defect other than the legacy timestamp must refuse, got %v", err)
	}
}

func TestRepairRefusesPrivilegedSignedControlMessage(t *testing.T) {
	privileged := `{"id": "priv-1", "sender": "control", "recipient": "orchestrator", "subject": "s", ` +
		`"body": "b", "signature": "abc123", "timestamp": "2026-09-11T10:47:09.000000-0500"}`
	mb, path := mailboxWith(t, privileged)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "priv-1", Fingerprint: sha256Hex(privileged), Act: true, Actor: "op",
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
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op", Reason: "2982",
	})
	if err != nil {
		t.Fatal(err)
	}
	records := readRepairRecords(t, path)
	if len(records) != 2 {
		t.Fatalf("want a prepare record and a result record, got %d", len(records))
	}
	prepare := records[0]
	if prepare.OriginalLine != legacyRow {
		t.Fatal("audit artifact did not retain the original corrupt bytes verbatim")
	}
	if prepare.OriginalSHA256 != sha256Hex(legacyRow) || prepare.RepairedSHA256 != plan.RepairedSHA256 {
		t.Fatalf("audit artifact digests do not bind the exact bytes: %+v", prepare)
	}
}

// readRepairRecords decodes the audit artifact as the JSONL it is.
func readRepairRecords(t *testing.T, mailPath string) []RepairPlan {
	t.Helper()
	raw, err := os.ReadFile(mailPath + ".repair.jsonl")
	if err != nil {
		t.Fatalf("no repair audit artifact: %v", err)
	}
	var out []RepairPlan
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec RepairPlan
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("audit record is not valid JSON: %v", err)
		}
		out = append(out, rec)
	}
	return out
}

// The durable artifact must never claim a repair succeeded before it has. The
// earlier single-record form stamped applied=true and wrote it BEFORE the
// sequence reservation, the mailbox write and the readback, so any later
// failure left permanent evidence of a success that never happened.
func TestRepairAuditRecordsPrepareBeforeResult(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op", Reason: "3019",
	}); err != nil {
		t.Fatal(err)
	}
	records := readRepairRecords(t, path)
	if len(records) != 2 {
		t.Fatalf("want exactly two audit records, got %d", len(records))
	}
	prepare, result := records[0], records[1]
	if prepare.Phase != RepairPhasePrepare {
		t.Fatalf("first record is not a prepare record: %+v", prepare)
	}
	if prepare.Applied {
		t.Fatal("the pre-mutation record claimed the repair was already applied")
	}
	if prepare.PreparedAt.IsZero() {
		t.Fatal("prepare record carries no prepared_at")
	}
	if result.Phase != RepairPhaseResult || result.Outcome != RepairOutcomeApplied || !result.Applied {
		t.Fatalf("second record is not a successful result record: %+v", result)
	}
	if result.CompletedAt.Before(prepare.PreparedAt) {
		t.Fatalf("result predates prepare: %s vs %s", result.CompletedAt, prepare.PreparedAt)
	}
	// Both records must bind the same bytes, or they describe different events.
	if result.OriginalSHA256 != prepare.OriginalSHA256 || result.RepairedSHA256 != prepare.RepairedSHA256 {
		t.Fatal("result record does not bind the same bytes as the prepare record")
	}
	if result.Actor != "op" {
		t.Fatalf("result record lost the actor: %q", result.Actor)
	}
}

// A mutation that fails after prepare must leave a FAILED result record, not
// silence and not a success.
func TestRepairAuditRecordsFailureAfterPrepare(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	restore := writeFileAtomicFn
	writeFileAtomicFn = func(p string, data []byte, perm os.FileMode) error {
		if p == path {
			return errors.New("injected mailbox write failure")
		}
		return restore(p, data, perm)
	}
	defer func() { writeFileAtomicFn = restore }()

	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	}); err == nil {
		t.Fatal("a failed mailbox write must not report success")
	}
	records := readRepairRecords(t, path)
	if len(records) != 2 {
		t.Fatalf("want prepare + failed result, got %d records", len(records))
	}
	if records[0].Phase != RepairPhasePrepare || records[0].Applied {
		t.Fatalf("prepare record is wrong: %+v", records[0])
	}
	result := records[1]
	if result.Phase != RepairPhaseResult || result.Outcome != RepairOutcomeFailed {
		t.Fatalf("failure was not recorded as a failed result: %+v", result)
	}
	if result.Applied {
		t.Fatal("a failed repair is recorded as applied")
	}
	if !strings.Contains(result.Failure, "injected mailbox write failure") {
		t.Fatalf("failure record does not say why it failed: %q", result.Failure)
	}
	// The original bytes stay recoverable from the prepare record.
	if records[0].OriginalLine != legacyRow {
		t.Fatal("prepare record lost the original bytes")
	}
}

// TestRepairAuditNeverPersistsHostAbsolutePaths is the redaction guard. The
// causes that reach the failure record wrap *os.PathError carrying the mailbox
// path, and the record is durable, so an unredacted reason would write a
// host-absolute path into an artifact the rest of this package is careful never
// to leak one into.
//
// The assertion uses the package's OWN containsAbsPath, so the test agrees with
// redactErr about what a host-absolute path is instead of inventing a second,
// weaker definition that could drift.
func TestRepairAuditNeverPersistsHostAbsolutePaths(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	if !containsAbsPath(path) {
		t.Fatalf("fixture precondition: %q is not recognized as a host-absolute path, so this test would pass vacuously", path)
	}
	restore := writeFileAtomicFn
	writeFileAtomicFn = func(p string, data []byte, perm os.FileMode) error {
		if p == path {
			// A real filesystem error shape, carrying the absolute path.
			return &os.PathError{Op: "write", Path: p, Err: errors.New("permission denied")}
		}
		return restore(p, data, perm)
	}
	defer func() { writeFileAtomicFn = restore }()

	_, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	})
	if err == nil {
		t.Fatal("a path error must not report success")
	}

	raw, readErr := os.ReadFile(path + ".repair.jsonl")
	if readErr != nil {
		t.Fatalf("audit artifact missing: %v", readErr)
	}
	// The fixture payload contains no path of its own, so NOTHING in the
	// artifact may look like one.
	if containsAbsPath(string(raw)) {
		t.Fatalf("audit artifact persisted a host-absolute path:\n%s", raw)
	}

	records := readRepairRecords(t, path)
	if len(records) != 2 {
		t.Fatalf("want prepare + failed result, got %d", len(records))
	}
	prepare, result := records[0], records[1]
	if containsAbsPath(prepare.Failure) || containsAbsPath(result.Failure) {
		t.Fatalf("a recorded failure reason carries a host-absolute path: %q / %q", prepare.Failure, result.Failure)
	}
	// Redaction must not reduce the reason to nothing: op, basename and the
	// underlying cause all have to survive, or the record stops being evidence.
	if !strings.Contains(result.Failure, "permission denied") {
		t.Fatalf("redaction dropped the underlying cause: %q", result.Failure)
	}
	if !strings.Contains(result.Failure, filepath.Base(path)) {
		t.Fatalf("redaction dropped the basename, leaving the reason unattributable: %q", result.Failure)
	}

	// The original message payload must survive byte-for-byte, and its
	// fingerprints must be unchanged — redaction touches the failure reason
	// only, never the evidence the repair is bound to.
	if prepare.OriginalLine != legacyRow {
		t.Fatal("redaction altered the original bytes")
	}
	if prepare.OriginalSHA256 != sha256Hex(legacyRow) {
		t.Fatalf("original fingerprint changed: %q", prepare.OriginalSHA256)
	}
	if result.OriginalLine != prepare.OriginalLine || result.OriginalSHA256 != prepare.OriginalSHA256 {
		t.Fatal("result record no longer binds the same original bytes as prepare")
	}
	if result.RepairedSHA256 != prepare.RepairedSHA256 {
		t.Fatal("repaired fingerprint changed between prepare and result")
	}
}

// A payload that genuinely contains an absolute path is the operator's own
// content and must be preserved verbatim. Redaction applies to failure reasons
// this package generates, never to the message being recovered.
func TestRepairPreservesAbsolutePathsInsideTheMessagePayload(t *testing.T) {
	payloadRow := `{"id": "host-pathy-1", "sender": "agent", "recipient": "orchestrator", ` +
		`"subject": "log", "body": "failed at ` + absUsersPrefix() + `someone/project/file.go", ` +
		`"read": false, "timestamp": "2026-09-11T10:47:09.000000-0500"}`
	mb, path := mailboxWith(t, payloadRow)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-pathy-1", Fingerprint: sha256Hex(payloadRow), Act: true, Actor: "op",
	}); err != nil {
		t.Fatal(err)
	}
	records := readRepairRecords(t, path)
	if len(records) == 0 || records[0].OriginalLine != payloadRow {
		t.Fatal("an operator payload containing an absolute path was not preserved verbatim")
	}
	if !strings.Contains(records[0].RepairedLine, absUsersPrefix()+"someone/project/file.go") {
		t.Fatalf("the repaired row lost the payload's own path text: %q", records[0].RepairedLine)
	}
}

// A readback mismatch is a failure like any other and must be recorded as one.
func TestRepairAuditRecordsReadbackMismatchAsFailure(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	restore := writeFileAtomicFn
	writeFileAtomicFn = func(p string, data []byte, perm os.FileMode) error {
		if p == path {
			data = []byte(`{"id":"host-81751-1789141629774","seq":1,"sender":"tampered",` +
				`"recipient":"orchestrator","subject":"finding","body":"two defects",` +
				`"read":false,"timestamp":"2026-09-11T10:47:09-05:00"}` + "\n")
		}
		return restore(p, data, perm)
	}
	defer func() { writeFileAtomicFn = restore }()

	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairReadbackFailed) {
		t.Fatalf("want a readback failure, got %v", err)
	}
	records := readRepairRecords(t, path)
	if len(records) != 2 || records[1].Outcome != RepairOutcomeFailed || records[1].Applied {
		t.Fatalf("readback mismatch was not recorded as a failed result: %+v", records)
	}
}

// If the mailbox is repaired and verified but the COMPLETION record cannot be
// written, the caller must be told it failed. Reporting success would be the
// precise lie the phase split exists to prevent, and the operator would be
// told "applied" with no record to find.
func TestRepairFailsWhenCompletionRecordCannotBeWritten(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	auditPath := path + ".repair.jsonl"
	writes := 0
	restore := fileSyncFn
	fileSyncFn = func(f *os.File) error {
		// Fail only the SECOND audit append: prepare succeeds, result does not.
		if strings.Contains(f.Name(), ".repair.jsonl") {
			writes++
			if writes > 1 {
				return errors.New("injected completion record failure")
			}
		}
		return restore(f)
	}
	defer func() { fileSyncFn = restore }()

	plan, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	})
	if !errors.Is(err, ErrRepairCompletionUnrecorded) {
		t.Fatalf("an unrecorded completion must fail clearly, got plan=%+v err=%v", plan, err)
	}
	// Recoverability: the prepare record with the original bytes is still there.
	raw, readErr := os.ReadFile(auditPath)
	if readErr != nil {
		t.Fatalf("prepare record lost: %v", readErr)
	}
	if !strings.Contains(string(raw), legacyRow) {
		t.Fatal("original bytes are not recoverable from the audit artifact")
	}
}

// The CLI already demands --actor; the package must demand it too, or a direct
// caller can apply an unattributed mutation.
func TestRepairActRequiresActorAtThePackageBoundary(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true,
	}); !errors.Is(err, ErrRepairActorRequired) {
		t.Fatalf("acting without an actor must refuse, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("an unattributed act mutated the mailbox")
	}
	if _, statErr := os.Stat(path + ".repair.jsonl"); !os.IsNotExist(statErr) {
		t.Fatal("an unattributed act wrote an audit record")
	}
	// Report-only never needed an actor and still does not.
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{ID: "host-81751-1789141629774"}); err != nil {
		t.Fatalf("report-only must not require an actor: %v", err)
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
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
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
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
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
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
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
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
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

// Acting without naming the exact bytes is not a repair, it is a guess.
func TestRepairActRequiresExactFingerprint(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairFingerprintRequired) {
		t.Fatalf("--act without a fingerprint must refuse, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("a fingerprint-less act mutated the mailbox")
	}
	// Report-only still works without one, and hands back the fingerprint to use.
	plan, err := mb.RepairMalformedRow(context.Background(), RepairRequest{ID: "host-81751-1789141629774"})
	if err != nil {
		t.Fatalf("report-only must not require a fingerprint: %v", err)
	}
	if plan.OriginalSHA256 != sha256Hex(legacyRow) {
		t.Fatalf("report did not emit the fingerprint an act needs: %q", plan.OriginalSHA256)
	}
}

// quarantineCopies appends n IDENTICAL quarantine records, exactly as repeated
// ReadInbox calls over an unrepaired row do.
func quarantineCopies(t *testing.T, mailPath, line string, n int) {
	t.Helper()
	var buf strings.Builder
	for i := 0; i < n; i++ {
		rec, err := json.Marshal(QuarantineEntry{Line: line, Reason: "cannot parse", Timestamp: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(rec)
		buf.WriteString("\n")
	}
	if err := os.WriteFile(mailPath+".quarantine.jsonl", []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every reader re-quarantines an unrepaired row, so the artifact fills up with
// identical copies. That is normal and must never make the row permanently
// unrepairable.
func TestRepairIgnoresRepeatedIdenticalQuarantineCopies(t *testing.T) {
	mb, path := mailboxWith(t, legacyRow)
	quarantineCopies(t, path, legacyRow, 7)
	plan, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	})
	if err != nil {
		t.Fatalf("7 identical quarantine copies must not block the repair: %v", err)
	}
	if !plan.Applied {
		t.Fatal("repair did not apply")
	}
}

// Two DIFFERENT originals under one id means the row moved between reads: the
// bytes an operator reviewed are not necessarily the bytes on disk.
func TestRepairRefusesConflictingQuarantinedOriginals(t *testing.T) {
	variant := strings.Replace(legacyRow, `"body": "two defects"`, `"body": "different body"`, 1)
	if variant == legacyRow {
		t.Fatal("fixture did not produce a distinct original")
	}
	mb, path := mailboxWith(t, legacyRow)
	rec1, _ := json.Marshal(QuarantineEntry{Line: legacyRow, Reason: "cannot parse", Timestamp: time.Now()})
	rec2, _ := json.Marshal(QuarantineEntry{Line: variant, Reason: "cannot parse", Timestamp: time.Now()})
	if err := os.WriteFile(path+".quarantine.jsonl", append(append(rec1, '\n'), append(rec2, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairConflictingOriginals) {
		t.Fatalf("conflicting quarantined originals must refuse, got %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), legacyRow) {
		t.Fatal("a refused repair mutated the mailbox")
	}
}

// A quarantined original that no longer matches the live row is stale evidence.
func TestRepairRefusesWhenQuarantinedOriginalDiffersFromLiveRow(t *testing.T) {
	variant := strings.Replace(legacyRow, `"body": "two defects"`, `"body": "older body"`, 1)
	mb, path := mailboxWith(t, legacyRow)
	quarantineCopies(t, path, variant, 3)
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	}); !errors.Is(err, ErrRepairStale) {
		t.Fatalf("a quarantined original that is not the live row must refuse, got %v", err)
	}
}

// Quarantine records for OTHER ids, however many, are none of this repair's
// business.
func TestRepairIgnoresQuarantineRecordsForOtherIDs(t *testing.T) {
	otherA := `{"id": "other-a", "sender": "x", "recipient": "y", "subject": "s", "body": "b", "read": false, "timestamp": "2026-09-11T10:47:09.000000-0500"}`
	otherB := `{"id": "other-b", "sender": "x", "recipient": "y", "subject": "s", "body": "b", "read": false, "timestamp": "2026-09-11T11:47:09.000000-0500"}`
	mb, path := mailboxWith(t, legacyRow)
	var buf strings.Builder
	for _, line := range []string{otherA, otherB, otherA, legacyRow} {
		rec, _ := json.Marshal(QuarantineEntry{Line: line, Reason: "cannot parse", Timestamp: time.Now()})
		buf.Write(rec)
		buf.WriteString("\n")
	}
	if err := os.WriteFile(path+".quarantine.jsonl", []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mb.RepairMalformedRow(context.Background(), RepairRequest{
		ID: "host-81751-1789141629774", Fingerprint: sha256Hex(legacyRow), Act: true, Actor: "op",
	}); err != nil {
		t.Fatalf("unrelated quarantined ids must not block the repair: %v", err)
	}
}
