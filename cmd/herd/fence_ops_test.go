package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-785 hermetic fixtures only. The real FAC-655 operation
// 9eedc96717a346bfc65786e709c3a9df is OFF LIMITS and is never referenced by
// any fixture below.

const (
	fenceOpFixtureAmbiguous = "00000000feed0000face0000000000a1"
	fenceOpFixtureApplied   = "00000000feed0000face0000000000a2"
	fenceOpFixtureUnknown   = "00000000feed0000face0000000000ff"
	fenceOpFixtureRepo      = "/tmp/fac785-fixture-repo"
	fenceOpFixtureProject   = "proj-fixture"
	fenceOpFixtureTaskRef   = "FAC-785X"
	fenceOpFixtureTaskID    = "board-fixture-1"
	fenceOpFixtureWorkerTok = "fixture-worker-token-0123456789"
)

// fenceOpIntentKey mirrors the production providerIntentKey shape:
// provider:<repo>/<provider>/<project>/<taskRef>:g<generation>:<kind>.
func fenceOpIntentKey(generation int, kind string) string {
	return fmt.Sprintf("provider:%s/kaneo/%s/%s:g%d:%s", fenceOpFixtureRepo, fenceOpFixtureProject, fenceOpFixtureTaskRef, generation, kind)
}

type fenceOpFixture struct {
	claimDir    string
	outboxPath  string
	fencePath   string
	opID        string
	intentKey   string
	ambiguousRc provider.OpReceipt
}

func seedFenceOpFixture(t *testing.T, opID string, ambiguous bool) *fenceOpFixture {
	t.Helper()
	claimDir := t.TempDir()
	f := &fenceOpFixture{
		claimDir:   claimDir,
		outboxPath: filepath.Join(claimDir, "outbox.db"),
		fencePath:  filepath.Join(claimDir, "fences.db"),
		opID:       opID,
		intentKey:  fenceOpIntentKey(1, "status:done"),
	}
	rc := provider.OpReceipt{
		OpID:           opID,
		TaskID:         fenceOpFixtureTaskID,
		FenceToken:     1,
		Revision:       "42",
		BaseRevision:   "41",
		ExpectedStatus: "done",
		Ambiguous:      ambiguous,
	}
	f.ambiguousRc = rc

	outbox, err := claim.NewSQLiteOutbox(f.outboxPath)
	if err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if _, err := outbox.Enqueue(context.Background(), claim.OutboxIntent{
		IdempotencyKey: f.intentKey,
		Kind:           "status:done",
		Payload:        []byte(opID),
	}); err != nil {
		t.Fatalf("seed enqueue: %v", err)
	}
	if err := outbox.Close(); err != nil {
		t.Fatalf("close outbox: %v", err)
	}

	fences, err := provider.NewSQLiteFenceStore(f.fencePath)
	if err != nil {
		t.Fatalf("seed fence store: %v", err)
	}
	ctx := context.Background()
	if ambiguous {
		err = fences.MarkAmbiguous(ctx, rc)
	} else {
		err = fences.MarkApplied(ctx, rc)
	}
	if err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
	if err := fences.Close(); err != nil {
		t.Fatalf("close fence store: %v", err)
	}
	return f
}

// fenceOpSnapshot captures the local bookkeeping state so tests can assert
// zero mutation across a command run.
type fenceOpSnapshot struct {
	OutboxStatus claim.OutboxStatus
	Attempts     int
	Applied      bool
	Ambiguous    bool
	RecordCount  int
	ReceiptCount int
}

func snapshotFenceOpFixture(t *testing.T, f *fenceOpFixture) fenceOpSnapshot {
	t.Helper()
	outbox, err := claim.NewSQLiteOutbox(f.outboxPath)
	if err != nil {
		t.Fatalf("snapshot outbox: %v", err)
	}
	defer outbox.Close()
	pending, err := outbox.Pending(context.Background())
	if err != nil {
		t.Fatalf("snapshot pending: %v", err)
	}
	snap := fenceOpSnapshot{RecordCount: len(pending)}
	for _, rec := range pending {
		if rec.IdempotencyKey == f.intentKey {
			snap.OutboxStatus = rec.Status
			snap.Attempts = rec.Attempts
		}
	}
	fences, err := provider.NewSQLiteFenceStore(f.fencePath)
	if err != nil {
		t.Fatalf("snapshot fences: %v", err)
	}
	defer fences.Close()
	rc, err := fences.LookupApplied(context.Background(), f.opID)
	if err != nil {
		t.Fatalf("snapshot receipt: %v", err)
	}
	if rc != nil {
		snap.Applied = !rc.Ambiguous
		snap.Ambiguous = rc.Ambiguous
		snap.ReceiptCount = 1
	}
	return snap
}

// runFenceOpCLI executes the real herd binary with a hermetic environment.
func runFenceOpCLI(t *testing.T, f *fenceOpFixture, brokerURL string, args ...string) (int, string, string) {
	t.Helper()
	binary := buildHerd(t)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.claimDir,
		"HERD_CLAIM_DIR=" + f.claimDir,
	}
	if brokerURL != "" {
		env = append(env,
			"HERD_FENCE_BROKER_URL="+brokerURL,
			"HERD_FENCE_BROKER_TOKEN="+fenceOpFixtureWorkerTok)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = f.claimDir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if !asExitError(err, &ee) {
			t.Fatalf("run herd %v: %v", args, err)
		}
		exit = ee.ExitCode()
	}
	return exit, stdout.String(), stderr.String()
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

func decodeFenceOpJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("status output is not JSON: %v\n%s", err, out)
	}
	return m
}

// recordingBroker is a hermetic broker stub that records every request's
// method and path and serves a canned /v1/ops/<opID> response.
type recordingBroker struct {
	mu       sync.Mutex
	requests []string
	handler  func(opID string) (int, string)
}

func (r *recordingBroker) record(method, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, method+" "+path)
}

func (r *recordingBroker) allRequests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

func newRecordingBroker(t *testing.T, handler func(opID string) (int, string)) (*recordingBroker, *httptest.Server) {
	t.Helper()
	rb := &recordingBroker{handler: handler}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rb.record(req.Method, req.URL.Path)
		opID := strings.TrimPrefix(req.URL.Path, "/v1/ops/")
		code, body := rb.handler(opID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return rb, srv
}

// --- status: read-only discipline ---

func TestFenceOpStatusIsReadOnly(t *testing.T) {
	f := seedFenceOpFixture(t, fenceOpFixtureAmbiguous, true)
	before := snapshotFenceOpFixture(t, f)

	// Broker path: every request the command makes must be a read-only GET
	// for the exact op; no PUT/POST may ever occur.
	rb, srv := newRecordingBroker(t, func(opID string) (int, string) {
		return 404, `{"applied": false}`
	})
	exit, stdout, stderr := runFenceOpCLI(t, f, srv.URL, "fence-op", "status", f.opID, "--json")
	_ = stdout
	_ = stderr
	// The command must fail closed: the broker does not know the op.
	if exit == 0 {
		t.Fatalf("status of an op with no upstream evidence must exit non-zero, got 0")
	}
	for _, req := range rb.allRequests() {
		if !strings.HasPrefix(req, "GET ") {
			t.Fatalf("non-GET request issued by read-only status: %q", req)
		}
		if !strings.HasSuffix(req, "/v1/ops/"+f.opID) {
			t.Fatalf("status queried an unexpected path: %q", req)
		}
	}

	after := snapshotFenceOpFixture(t, f)
	if after != before {
		t.Fatalf("read-only status mutated local bookkeeping: before=%+v after=%+v", before, after)
	}
}

func TestFenceOpStatusUnknownOpFailsClosed(t *testing.T) {
	f := seedFenceOpFixture(t, fenceOpFixtureUnknown, true)
	before := snapshotFenceOpFixture(t, f)
	// Query an op ID that has no fixture evidence anywhere (valid hex shape).
	ghost := "00000000feed0000face000000000009"
	exit, stdout, stderr := runFenceOpCLI(t, f, "", "fence-op", "status", ghost, "--json")
	if exit == 0 {
		t.Fatalf("unknown op must exit non-zero, got 0 (stdout=%s stderr=%s)", stdout, stderr)
	}
	m := decodeFenceOpJSON(t, stdout)
	if m["applied"] == true || m["ambiguous"] == true {
		t.Fatalf("unknown op must never be reported applied/ambiguous: %v", m)
	}
	if state, _ := m["state"].(string); state != "unknown" {
		t.Fatalf("expected honest state=unknown, got %v", m["state"])
	}
	after := snapshotFenceOpFixture(t, f)
	if after != before {
		t.Fatalf("unknown-op status mutated local bookkeeping: before=%+v after=%+v", before, after)
	}
}

func TestFenceOpStatusAmbiguousOpHonest(t *testing.T) {
	f := seedFenceOpFixture(t, fenceOpFixtureAmbiguous, true)
	exit, stdout, stderr := runFenceOpCLI(t, f, "", "fence-op", "status", f.opID, "--json")
	if exit == 0 {
		t.Fatalf("ambiguous op is not a success: exit 0 (stdout=%s stderr=%s)", stdout, stderr)
	}
	m := decodeFenceOpJSON(t, stdout)
	if m["applied"] != false || m["ambiguous"] != true {
		t.Fatalf("ambiguous fixture must report applied=false ambiguous=true, got %v", m)
	}
	if m["op_id"] != f.opID {
		t.Fatalf("op_id mismatch: %v", m["op_id"])
	}
	if m["task_id"] != fenceOpFixtureTaskID {
		t.Fatalf("task_id mismatch: %v", m["task_id"])
	}
	if m["expected_status"] != "done" {
		t.Fatalf("expected_status mismatch: %v", m["expected_status"])
	}
	// Exact binding identity must be emitted (from the paired outbox record).
	ob, _ := m["outbox"].(map[string]any)
	if ob == nil {
		t.Fatalf("outbox record missing from status output: %v", m)
	}
	id, _ := ob["identity"].(map[string]any)
	if id == nil || id["repo"] != fenceOpFixtureRepo || id["project"] != fenceOpFixtureProject || id["task_ref"] != fenceOpFixtureTaskRef {
		t.Fatalf("identity binding missing or wrong: %v", ob)
	}
}

func TestFenceOpStatusAppliedOp(t *testing.T) {
	f := seedFenceOpFixture(t, fenceOpFixtureApplied, false)
	exit, stdout, stderr := runFenceOpCLI(t, f, "", "fence-op", "status", f.opID, "--json")
	if exit != 0 {
		t.Fatalf("proven-applied op should exit 0, got %d (stdout=%s stderr=%s)", exit, stdout, stderr)
	}
	m := decodeFenceOpJSON(t, stdout)
	if m["applied"] != true || m["ambiguous"] == true {
		t.Fatalf("applied fixture must report applied=true, got %v", m)
	}
}

func TestFenceOpStatusRetainsIdentityForAppliedOutbox(t *testing.T) {
	f := seedFenceOpFixture(t, fenceOpFixtureApplied, false)
	// Mark the outbox record applied in the store to simulate completed local settlement.
	outbox, err := claim.NewSQLiteOutbox(f.outboxPath)
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	if err := outbox.ForceMarkApplied(context.Background(), f.intentKey, time.Now()); err != nil {
		t.Fatalf("force mark applied: %v", err)
	}
	outbox.Close()

	// 1. Plain status should retain outbox identity and report applied status.
	exit, stdout, stderr := runFenceOpCLI(t, f, "", "fence-op", "status", f.opID, "--json")
	if exit != 0 {
		t.Fatalf("status for applied outbox should exit 0, got %d (stdout=%s stderr=%s)", exit, stdout, stderr)
	}
	m := decodeFenceOpJSON(t, stdout)
	if m["applied"] != true {
		t.Fatalf("expected applied=true, got %v", m)
	}
	ob, _ := m["outbox"].(map[string]any)
	if ob == nil {
		t.Fatalf("outbox record missing from applied status output: %v", m)
	}
	if status, _ := ob["status"].(string); status != "applied" {
		t.Fatalf("expected outbox.status=applied, got %v", ob["status"])
	}
	id, _ := ob["identity"].(map[string]any)
	if id == nil || id["repo"] != fenceOpFixtureRepo || id["project"] != fenceOpFixtureProject || id["task_ref"] != fenceOpFixtureTaskRef {
		t.Fatalf("identity binding missing or wrong on applied outbox: %v", ob)
	}

	// 2. Caller-supplied --repo, --project, --task-ref, --task should confirm binding and succeed.
	exit, stdout, stderr = runFenceOpCLI(t, f, "", "fence-op", "status", f.opID, "--json",
		"--repo", fenceOpFixtureRepo,
		"--project", fenceOpFixtureProject,
		"--task-ref", fenceOpFixtureTaskRef,
		"--task", fenceOpFixtureTaskID,
	)
	if exit != 0 {
		t.Fatalf("matching binding flags on applied outbox should exit 0, got %d (stdout=%s stderr=%s)", exit, stdout, stderr)
	}

	// 3. Mismatched binding flag on applied outbox must refuse.
	exit, stdout, stderr = runFenceOpCLI(t, f, "", "fence-op", "status", f.opID, "--json",
		"--repo", "/tmp/mismatched-repo",
	)
	if exit == 0 {
		t.Fatalf("mismatched repo on applied outbox must refuse with non-zero exit, got 0")
	}
}

func TestFenceOpStatusBindingMismatchRefuses(t *testing.T) {
	f := seedFenceOpFixture(t, fenceOpFixtureAmbiguous, true)
	before := snapshotFenceOpFixture(t, f)
	exit, stdout, stderr := runFenceOpCLI(t, f, "", "fence-op", "status", f.opID, "--json",
		"--repo", "/tmp/some-other-repo")
	if exit == 0 {
		t.Fatalf("binding mismatch must exit non-zero, got 0 (stdout=%s)", stdout)
	}
	if !strings.Contains(stderr+stdout, "mismatch") {
		t.Fatalf("refusal must name the binding mismatch, got stderr=%q stdout=%q", stderr, stdout)
	}
	after := snapshotFenceOpFixture(t, f)
	if after != before {
		t.Fatalf("binding-mismatch run mutated local bookkeeping: before=%+v after=%+v", before, after)
	}
}

func TestFenceOpStatusBrokerUnavailableFailsClosed(t *testing.T) {
	f := seedFenceOpFixture(t, fenceOpFixtureAmbiguous, true)
	before := snapshotFenceOpFixture(t, f)
	// A configured-but-dead broker: serve nothing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // port now closed
	exit, stdout, stderr := runFenceOpCLI(t, f, srv.URL, "fence-op", "status", f.opID, "--json")
	if exit == 0 {
		t.Fatalf("unavailable upstream must fail closed non-zero, got 0 (stdout=%s)", stdout)
	}
	after := snapshotFenceOpFixture(t, f)
	if after != before {
		t.Fatalf("unavailable-upstream run mutated local bookkeeping: before=%+v after=%+v", before, after)
	}
	_ = stderr
}

func TestFenceOpStatusHTTP200ErrorBodyFailsClosed(t *testing.T) {
	f := seedFenceOpFixture(t, fenceOpFixtureAmbiguous, true)
	rb, srv := newRecordingBroker(t, func(opID string) (int, string) {
		return 200, `{"error":"internal ledger mismatch"}`
	})
	exit, _, stderr := runFenceOpCLI(t, f, srv.URL, "fence-op", "status", f.opID, "--json")
	if exit == 0 {
		t.Fatalf("HTTP 200 error body must fail closed non-zero, got 0")
	}
	if len(rb.allRequests()) == 0 {
		t.Fatalf("expected the broker to be consulted")
	}
	_ = stderr
}

func TestFenceOpStatusMalformedBodyFailsClosed(t *testing.T) {
	f := seedFenceOpFixture(t, fenceOpFixtureAmbiguous, true)
	_, srv := newRecordingBroker(t, func(opID string) (int, string) {
		return 200, `not-json-at-all`
	})
	exit, _, _ := runFenceOpCLI(t, f, srv.URL, "fence-op", "status", f.opID, "--json")
	if exit == 0 {
		t.Fatalf("malformed broker body must fail closed non-zero, got 0")
	}
}

func TestFenceOpStatusNeverMintsNewOps(t *testing.T) {
	f := seedFenceOpFixture(t, fenceOpFixtureAmbiguous, true)
	before := snapshotFenceOpFixture(t, f)
	for i := 0; i < 2; i++ {
		runFenceOpCLI(t, f, "", "fence-op", "status", f.opID, "--json")
	}
	after := snapshotFenceOpFixture(t, f)
	if after.RecordCount != before.RecordCount || after.ReceiptCount != before.ReceiptCount {
		t.Fatalf("status minted new bookkeeping: before=%+v after=%+v", before, after)
	}
}

// --- reconcile: report-only default, settle authority refusal ---

func TestFenceReconcileReportOnlyDefault(t *testing.T) {
	// Proven-applied receipt + pending outbox record: report-only must NOT
	// settle (no ForceMarkApplied), even though the proof exists.
	f := seedFenceOpFixture(t, fenceOpFixtureApplied, false)
	before := snapshotFenceOpFixture(t, f)
	exit, stdout, stderr := runFenceOpCLI(t, f, "", "fence-op", "reconcile", "--json")
	if exit == 0 {
		t.Fatalf("pending work must keep a non-zero exit, got 0 (stdout=%s stderr=%s)", stdout, stderr)
	}
	m := decodeFenceOpJSON(t, stdout)
	if got, _ := m["settled"].(float64); got != 0 {
		t.Fatalf("report-only reconcile must settle nothing, settled=%v", m["settled"])
	}
	if got, _ := m["would_settle"].(float64); got != 1 {
		t.Fatalf("expected the proven-applied record reported as would_settle=1, got %v", m["would_settle"])
	}
	after := snapshotFenceOpFixture(t, f)
	if after.OutboxStatus != before.OutboxStatus || after.OutboxStatus == claim.OutboxApplied {
		t.Fatalf("report-only reconcile settled the record: before=%+v after=%+v", before, after)
	}
}

func TestFenceReconcileRefusesSettleWhenUpstreamAbsent(t *testing.T) {
	// Ambiguous receipt + no broker: absence of upstream evidence must never
	// authorize settlement, replay, or clearing.
	f := seedFenceOpFixture(t, fenceOpFixtureAmbiguous, true)
	before := snapshotFenceOpFixture(t, f)
	exit, stdout, _ := runFenceOpCLI(t, f, "", "fence-op", "reconcile", "--json")
	if exit == 0 {
		t.Fatalf("unproven records must keep a non-zero exit, got 0")
	}
	m := decodeFenceOpJSON(t, stdout)
	if got, _ := m["would_settle"].(float64); got != 0 {
		t.Fatalf("absent upstream evidence must never yield would_settle, got %v", m["would_settle"])
	}
	if got, _ := m["still_pending"].(float64); got != 1 {
		t.Fatalf("expected still_pending=1, got %v", m["still_pending"])
	}
	after := snapshotFenceOpFixture(t, f)
	if after != before {
		t.Fatalf("no-proof reconcile mutated local bookkeeping: before=%+v after=%+v", before, after)
	}
}

func TestFenceOpSettleRequiresCoordinatorAuthority(t *testing.T) {
	// --settle is refused outright in this slice: no existing non-forgeable
	// coordinator credential can be delivered to a one-shot CLI process.
	// Even with a proven-applied receipt, settle must refuse, exit non-zero,
	// name the missing prerequisite, and change nothing.
	f := seedFenceOpFixture(t, fenceOpFixtureApplied, false)
	before := snapshotFenceOpFixture(t, f)
	exit, stdout, stderr := runFenceOpCLI(t, f, "", "fence-op", "reconcile", "--op", f.opID, "--settle", "--json")
	if exit == 0 {
		t.Fatalf("--settle without coordinator authority must be refused, got exit 0 (stdout=%s)", stdout)
	}
	combined := stdout + stderr
	if !strings.Contains(combined, "unavailable") {
		t.Fatalf("refusal must state --settle is unavailable, got: %s", combined)
	}
	after := snapshotFenceOpFixture(t, f)
	if after != before {
		t.Fatalf("--settle refusal mutated local bookkeeping: before=%+v after=%+v", before, after)
	}
	if after.OutboxStatus == claim.OutboxApplied && before.OutboxStatus != claim.OutboxApplied {
		t.Fatalf("--settle refusal fabricated a success receipt")
	}
}

// --- pkg/claim controls: settle idempotence through the existing primitive ---

// TestFenceSettlePrimitiveIdempotentAndConcurrencySafe pins the existing
// ForceMarkApplied idempotence the future authorized settle path will rely
// on: repeated and concurrent settlement close the record exactly once and
// never fabricate a receipt. It uses ONLY existing exported APIs.
func TestFenceSettlePrimitiveIdempotentAndConcurrencySafe(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "outbox.db")
	ob, err := claim.NewSQLiteOutbox(path)
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	defer ob.Close()
	key := fenceOpIntentKey(1, "status:done")
	if _, err := ob.Enqueue(ctx, claim.OutboxIntent{IdempotencyKey: key, Kind: "status:done", Payload: []byte(fenceOpFixtureApplied)}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Repeated ForceMarkApplied on a record is idempotent.
	for i := 0; i < 3; i++ {
		if err := ob.ForceMarkApplied(ctx, key, time.Now()); err != nil {
			t.Fatalf("ForceMarkApplied #%d: %v", i, err)
		}
	}
	rec, err := ob.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec == nil || rec.Status != claim.OutboxApplied {
		t.Fatalf("expected applied after repeated settle, got %+v", rec)
	}

	// A fresh pending record: sequential reconcile settles exactly once; a
	// second sweep is a no-op (idempotent, nothing re-closed).
	keySweep := key + "-sweep"
	if _, err := ob.Enqueue(ctx, claim.OutboxIntent{IdempotencyKey: keySweep, Kind: "status:done", Payload: []byte(fenceOpFixtureAmbiguous)}); err != nil {
		t.Fatalf("enqueue sweep record: %v", err)
	}
	mgr := claim.NewClaimManager(nil, claim.WithDurableOutbox(ob), claim.WithSettlerID("settler-a"))
	verify := func(ctx context.Context, r *claim.OutboxRecord) (bool, error) { return true, nil }
	closed, stillPending, err := mgr.ReconcileProviderTransitions(ctx, verify)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if closed != 1 || stillPending != 0 {
		t.Fatalf("expected 1 closed / 0 pending, got closed=%d pending=%d", closed, stillPending)
	}
	closed, stillPending, err = mgr.ReconcileProviderTransitions(ctx, verify)
	if err != nil {
		t.Fatalf("reconcile #2: %v", err)
	}
	if closed != 0 || stillPending != 0 {
		t.Fatalf("repeated reconcile must settle nothing again, got closed=%d pending=%d", closed, stillPending)
	}

	// Concurrent settlement: two managers over the same outbox reconciling
	// the same pending record both succeed and the record ends applied
	// exactly (monotonic, no fabricated receipt).
	ob2, err := claim.NewSQLiteOutbox(path)
	if err != nil {
		t.Fatalf("open outbox2: %v", err)
	}
	defer ob2.Close()
	keyB := key + "-b"
	if _, err := ob2.Enqueue(ctx, claim.OutboxIntent{IdempotencyKey: keyB, Kind: "status:done", Payload: []byte(fenceOpFixtureAmbiguous)}); err != nil {
		t.Fatalf("enqueue2: %v", err)
	}
	mgrA := claim.NewClaimManager(nil, claim.WithDurableOutbox(ob), claim.WithSettlerID("settler-a"))
	mgrB := claim.NewClaimManager(nil, claim.WithDurableOutbox(ob2), claim.WithSettlerID("settler-b"))
	type result struct {
		closed int
		err    error
	}
	results := make(chan result, 2)
	for _, mgr := range []*claim.ClaimManager{mgrA, mgrB} {
		go func(m *claim.ClaimManager) {
			closed, _, err := m.ReconcileProviderTransitions(ctx, verify)
			results <- result{closed: closed, err: err}
		}(mgr)
	}
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent reconcile: %v", r.err)
		}
	}
	recB, err := ob.Get(ctx, keyB)
	if err != nil {
		t.Fatalf("get settled record: %v", err)
	}
	if recB == nil || recB.Status != claim.OutboxApplied {
		t.Fatalf("concurrent settlement must leave the record applied exactly once, got %+v", recB)
	}
}
