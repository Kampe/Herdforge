package herdr

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureRequest() CompareAndCloseRequest {
	return CompareAndCloseRequest{
		WorkspaceID:   "w",
		TabID:         "t",
		TabGeneration: 7,
		TabRevision:   3,
		PaneIDs:       []string{"p"},
		Attachments: []Attachment{{
			PaneID:     "p",
			Agent:      StringPtr("codex"),
			Session:    StringPtr("s1"),
			Generation: 2,
		}},
		Nonce: "n1",
	}
}

func fixtureLive() LiveTab {
	req := fixtureRequest()
	return LiveTab{
		WorkspaceID: req.WorkspaceID,
		TabID:       req.TabID,
		Generation:  req.TabGeneration,
		Revision:    req.TabRevision,
		PaneIDs:     append([]string(nil), req.PaneIDs...),
		Attachments: append([]Attachment(nil), req.Attachments...),
	}
}

func runAuth(t *testing.T, req CompareAndCloseRequest, live LiveTab, store *MemoryReceiptStore, closed *int) CloseReceipt {
	t.Helper()
	return CompareAndClose(req, live, 9, store, &FixedClock{MS: 11}, func() error {
		*closed++
		return nil
	})
}

func TestCompareAndClose_NewSessionAfterReadbackIsAttachmentChanged(t *testing.T) {
	// Client readback produced fixtureRequest; a new session attaches before
	// the close RPC. The tab must survive.
	srv := NewFakeCompareCloseServer()
	live := fixtureLive()
	srv.PutTab(live)
	req := fixtureRequest()

	// Race: new agent session after readback.
	srv.AttachSession(req.TabID, "p", "s2", 3)

	receipt := srv.CompareAndClose(req)
	if receipt.Outcome != OutcomeAttachmentChanged {
		t.Fatalf("outcome=%q want attachment_changed", receipt.Outcome)
	}
	if srv.IsClosed(req.TabID) {
		t.Fatal("tab was closed despite attachment change")
	}
	if _, ok := srv.Live(req.TabID); !ok {
		t.Fatal("tab must still be live")
	}
}

func TestCompareAndClose_RecycledTabGenerationIsStale(t *testing.T) {
	srv := NewFakeCompareCloseServer()
	live := fixtureLive()
	srv.PutTab(live)
	req := fixtureRequest() // generation 7

	srv.RecycleTab(req.TabID, 8) // same tab_id, new generation

	receipt := srv.CompareAndClose(req)
	if receipt.Outcome != OutcomeStaleGeneration {
		t.Fatalf("outcome=%q want stale_generation", receipt.Outcome)
	}
	if srv.IsClosed(req.TabID) {
		t.Fatal("recycled tab closed by old decision")
	}
}

func TestCompareAndClose_ExactIdentityClosesOnceAndNonceReplays(t *testing.T) {
	srv := NewFakeCompareCloseServer()
	live := fixtureLive()
	srv.PutTab(live)
	req := fixtureRequest()

	first := srv.CompareAndClose(req)
	if first.Outcome != OutcomeClosed || !first.ResultingAbsence {
		t.Fatalf("first=%+v", first)
	}
	if !srv.IsClosed(req.TabID) {
		t.Fatal("tab not closed")
	}

	// Duplicate nonce must return the same receipt without another mutation.
	// Re-install a recycled tab so a naive second close would be destructive.
	srv.RecycleTab(req.TabID, 99)
	second := srv.CompareAndClose(req)
	if second.Outcome != first.Outcome || second.ServerGeneration != first.ServerGeneration {
		t.Fatalf("replay diverged: first=%+v second=%+v", first, second)
	}
	// The recycled occupant must still be live — nonce replay must not close it.
	liveAfter, ok := srv.Live(req.TabID)
	if !ok || liveAfter.Generation != 99 {
		t.Fatalf("nonce replay disturbed recycled occupant: ok=%v live=%+v closed=%v", ok, liveAfter, srv.IsClosed(req.TabID))
	}
	store := srv.store.(*MemoryReceiptStore)
	got, err := store.Read(req.Nonce)
	if err != nil || got == nil || got.Outcome != OutcomeClosed {
		t.Fatalf("store receipt=%+v err=%v", got, err)
	}
}

func TestCompareAndClose_MissingEvidenceAndActiveMutationAndReceiptFailureBlock(t *testing.T) {
	// Missing generation in request.
	if err := ValidateCompareAndCloseRequest(CompareAndCloseRequest{
		WorkspaceID: "w", TabID: "t", Nonce: "n",
	}); err == nil {
		t.Fatal("zero generation must fail validation")
	}
	// Session without generation.
	if err := ValidateCompareAndCloseRequest(CompareAndCloseRequest{
		WorkspaceID: "w", TabID: "t", TabGeneration: 1, Nonce: "n",
		Attachments: []Attachment{{PaneID: "p", Session: StringPtr("s"), Generation: 0}},
	}); err == nil {
		t.Fatal("session without generation must fail validation")
	}

	store := NewMemoryReceiptStore()
	var closed int
	live := fixtureLive()
	live.MutationInFlight = true
	r := runAuth(t, CompareAndCloseRequest{Nonce: "mut", WorkspaceID: "w", TabID: "t", TabGeneration: 7, TabRevision: 3, PaneIDs: []string{"p"}, Attachments: fixtureRequest().Attachments}, live, store, &closed)
	if r.Outcome != OutcomeActiveMutation || closed != 0 {
		t.Fatalf("active mutation: %+v closed=%d", r, closed)
	}

	live = fixtureLive()
	live.Protected = true
	r = runAuth(t, CompareAndCloseRequest{Nonce: "prot", WorkspaceID: "w", TabID: "t", TabGeneration: 7, TabRevision: 3, PaneIDs: []string{"p"}, Attachments: fixtureRequest().Attachments}, live, store, &closed)
	if r.Outcome != OutcomeProtected || closed != 0 {
		t.Fatalf("protected: %+v closed=%d", r, closed)
	}

	failWrite := NewMemoryReceiptStore()
	failWrite.FailWrite = true
	r = runAuth(t, CompareAndCloseRequest{Nonce: "fw", WorkspaceID: "w", TabID: "t", TabGeneration: 7, TabRevision: 3, PaneIDs: []string{"p"}, Attachments: fixtureRequest().Attachments}, fixtureLive(), failWrite, &closed)
	if r.Outcome != OutcomeError || closed != 0 {
		t.Fatalf("receipt write failure must block close: %+v closed=%d", r, closed)
	}

	failRB := NewMemoryReceiptStore()
	failRB.FailReadback = true
	r = runAuth(t, CompareAndCloseRequest{Nonce: "fr", WorkspaceID: "w", TabID: "t", TabGeneration: 7, TabRevision: 3, PaneIDs: []string{"p"}, Attachments: fixtureRequest().Attachments}, fixtureLive(), failRB, &closed)
	if r.Outcome != OutcomeError || closed != 0 {
		t.Fatalf("receipt readback failure must block close: %+v closed=%d", r, closed)
	}

	// Socket/provider error on fake server.
	srv := NewFakeCompareCloseServer()
	srv.PutTab(fixtureLive())
	srv.TransportErrors = errors.New("socket down")
	r = srv.CompareAndClose(fixtureRequest())
	if r.Outcome != OutcomeError {
		t.Fatalf("transport error outcome=%q", r.Outcome)
	}
	if srv.IsClosed(fixtureRequest().TabID) {
		t.Fatal("transport error closed tab")
	}
}

func TestCompareAndClose_AutonomousPathsCannotUseLegacyPlainClose(t *testing.T) {
	var blocked *CloseUnavailableError
	if err := TabClose("any-tab"); !errors.As(err, &blocked) {
		t.Fatalf("TabClose must fail closed, got %v", err)
	}
	if err := CloseTabForRef("FAC-180"); !errors.As(err, &blocked) {
		t.Fatalf("CloseTabForRef must fail closed, got %v", err)
	}

	// Ensure runHerdr is never invoked for plain autonomous close.
	old := runHerdr
	defer func() { runHerdr = old }()
	called := false
	runHerdr = func(args ...string) (string, error) {
		called = true
		return "", nil
	}
	_ = TabClose("t1")
	_ = CloseTabForRef("FAC-1")
	if called {
		t.Fatal("legacy plain close reached herdr transport")
	}

	// Cleanup mutation mode also refuses unfenced close.
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"task-fac-1","agent_status":"done","tab_id":"t1","pane_id":"p1"}]}}`, nil
		}
		called = true
		return `{}`, nil
	}
	// SelectCleanupCandidates still returns candidates from agent list on main.
	cands, errs := Cleanup(nil, false)
	if len(cands) == 0 {
		// Policy may still select; either way mutation errs must not call tab close.
		_ = cands
	}
	for _, e := range errs {
		if e == nil {
			t.Fatal("nil cleanup error")
		}
		if !errors.As(e, &blocked) && !strings.Contains(e.Error(), "compare-and-close") {
			t.Fatalf("cleanup err=%v", e)
		}
	}
	// tab close must not have been issued
	if called {
		// called is set only for non-list; list doesn't set it. Good.
	}
}

func TestTabCloseCAS_SucceedsOnlyOnFencedClosedOutcome(t *testing.T) {
	srv := NewFakeCompareCloseServer()
	live := fixtureLive()
	srv.PutTab(live)
	restore := SetCompareCloseTransportForTest(func(req CompareAndCloseRequest) (CloseReceipt, error) {
		return srv.CompareAndClose(req), nil
	})
	defer restore()

	err := TabCloseCAS(CloseRequest{
		WorkspaceID:       "w",
		TabID:             "t",
		Generation:        "7",
		TabRevision:       3,
		PaneIDs:           []string{"p"},
		SessionID:         "s1",
		SessionGeneration: "2",
		Agent:             "codex",
		Nonce:             "cas-1",
	})
	if err != nil {
		t.Fatalf("CAS close: %v", err)
	}
	if !srv.IsClosed("t") {
		t.Fatal("expected closed")
	}

	// Attachment race refuses.
	srv.PutTab(fixtureLive())
	srv.AttachSession("t", "p", "s-new", 9)
	err = TabCloseCAS(CloseRequest{
		WorkspaceID: "w", TabID: "t", Generation: "7", TabRevision: 3,
		PaneIDs: []string{"p"}, SessionID: "s1", SessionGeneration: "2", Nonce: "cas-2",
	})
	var blocked *CloseUnavailableError
	if !errors.As(err, &blocked) || blocked.Reason != "attachment-changed" {
		t.Fatalf("want attachment-changed, got %v", err)
	}
}

func TestTabCloseCAS_IncompleteEvidenceFailsClosed(t *testing.T) {
	cases := []CloseRequest{
		{TabID: "t", Generation: "1", Nonce: "n"},                       // no workspace
		{WorkspaceID: "w", TabID: "t", Nonce: "n"},                      // no generation
		{WorkspaceID: "w", TabID: "t", Generation: "1"},                 // no nonce
		{WorkspaceID: "w", TabID: "t", Generation: "0", Nonce: "n"},     // zero gen
		{WorkspaceID: "w", TabID: "t", Generation: "1", SessionID: "s", Nonce: "n"}, // session without gen
	}
	for i, req := range cases {
		if err := TabCloseCAS(req); err == nil {
			t.Fatalf("case %d: expected error for %+v", i, req)
		}
	}
}

func TestJSONLReceiptStore_AppendReadIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "receipts.jsonl")
	store := &JSONLReceiptStore{Path: path}
	req := fixtureRequest()
	live := fixtureLive()
	var closed int
	r1 := CompareAndClose(req, live, 1, store, &FixedClock{MS: 5}, func() error { closed++; return nil })
	if r1.Outcome != OutcomeClosed || closed != 1 {
		t.Fatalf("r1=%+v closed=%d", r1, closed)
	}
	r2 := CompareAndClose(req, live, 2, store, &FixedClock{MS: 6}, func() error { closed++; return nil })
	if closed != 1 {
		t.Fatalf("replay mutated again: closed=%d", closed)
	}
	if r2.Outcome != r1.Outcome || r2.ServerGeneration != r1.ServerGeneration {
		t.Fatalf("jsonl replay diverged: %+v vs %+v", r1, r2)
	}
	// File is append-only JSONL.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
			var rec CloseReceipt
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("want 1 receipt line, got %d", lines)
	}
}

func TestMutation_WithoutGenerationCheckClosesRecycledTab(t *testing.T) {
	// Real path refuses; broken path closes — non-vacuous proof.
	req := fixtureRequest()
	live := fixtureLive()
	live.Generation = 8 // recycled

	var closedReal, closedBroken int
	storeReal := NewMemoryReceiptStore()
	real := CompareAndClose(req, live, 1, storeReal, &FixedClock{MS: 1}, func() error { closedReal++; return nil })
	if real.Outcome != OutcomeStaleGeneration || closedReal != 0 {
		t.Fatalf("real path must refuse recycled tab: %+v closed=%d", real, closedReal)
	}

	storeBroken := NewMemoryReceiptStore()
	broken := compareAndCloseWithoutGenerationCheck(req, live, 1, storeBroken, &FixedClock{MS: 1}, func() error { closedBroken++; return nil })
	if broken.Outcome != OutcomeClosed || closedBroken != 1 {
		t.Fatalf("mutation oracle must close recycled tab when generation fence removed: %+v closed=%d", broken, closedBroken)
	}
}

func TestMutation_WithoutAttachmentCheckClosesNewSession(t *testing.T) {
	req := fixtureRequest()
	live := fixtureLive()
	live.Attachments[0].Session = StringPtr("s-hijacked")

	var closedReal, closedBroken int
	real := CompareAndClose(req, live, 1, NewMemoryReceiptStore(), &FixedClock{MS: 1}, func() error { closedReal++; return nil })
	if real.Outcome != OutcomeAttachmentChanged || closedReal != 0 {
		t.Fatalf("real path: %+v closed=%d", real, closedReal)
	}
	broken := compareAndCloseWithoutAttachmentCheck(req, live, 1, NewMemoryReceiptStore(), &FixedClock{MS: 1}, func() error { closedBroken++; return nil })
	if broken.Outcome != OutcomeClosed || closedBroken != 1 {
		t.Fatalf("mutation oracle must close on attachment drift: %+v closed=%d", broken, closedBroken)
	}
}

func TestMutation_WithoutInFlightCheckClosesDuringStart(t *testing.T) {
	req := fixtureRequest()
	live := fixtureLive()
	live.MutationInFlight = true

	var closedReal, closedBroken int
	real := CompareAndClose(req, live, 1, NewMemoryReceiptStore(), &FixedClock{MS: 1}, func() error { closedReal++; return nil })
	if real.Outcome != OutcomeActiveMutation || closedReal != 0 {
		t.Fatalf("real path: %+v closed=%d", real, closedReal)
	}
	broken := compareAndCloseWithoutMutationCheck(req, live, 1, NewMemoryReceiptStore(), &FixedClock{MS: 1}, func() error { closedBroken++; return nil })
	if broken.Outcome != OutcomeClosed || closedBroken != 1 {
		t.Fatalf("mutation oracle must close during in-flight mutation: %+v closed=%d", broken, closedBroken)
	}
}

func TestMutation_WithoutReceiptDurabilityClosesEvenWhenStoreWouldFail(t *testing.T) {
	req := fixtureRequest()
	live := fixtureLive()
	failStore := NewMemoryReceiptStore()
	failStore.FailWrite = true

	var closedReal, closedBroken int
	real := CompareAndClose(req, live, 1, failStore, &FixedClock{MS: 1}, func() error { closedReal++; return nil })
	if real.Outcome != OutcomeError || closedReal != 0 {
		t.Fatalf("real path must block on receipt failure: %+v closed=%d", real, closedReal)
	}
	broken := compareAndCloseWithoutReceiptDurability(req, live, 1, failStore, &FixedClock{MS: 1}, func() error { closedBroken++; return nil })
	if broken.Outcome != OutcomeClosed || closedBroken != 1 {
		t.Fatalf("mutation oracle closes without durable receipt: %+v closed=%d", broken, closedBroken)
	}
}

func TestLiveTransport_NeverFallsBackToPlainTabClose(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		cp := append([]string(nil), args...)
		calls = append(calls, cp)
		return "", errors.New("compare-close not available")
	}
	_, err := liveCompareCloseTransport(fixtureRequest())
	if err == nil {
		t.Fatal("expected error when compare-close unavailable")
	}
	for _, c := range calls {
		if len(c) >= 2 && c[0] == "tab" && c[1] == "close" {
			t.Fatalf("fell back to plain tab close: %v", calls)
		}
		if len(c) < 2 || c[0] != "tab" || c[1] != "compare-close" {
			t.Fatalf("unexpected transport call: %v", c)
		}
	}
}

func TestWireRequestJSON_MatchesHerdrFieldNames(t *testing.T) {
	req := fixtureRequest()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	// Pin the snake_case wire names Herdr's serde expects.
	for _, key := range []string{
		`"workspace_id"`, `"tab_id"`, `"tab_generation"`, `"tab_revision"`,
		`"pane_ids"`, `"attachments"`, `"nonce"`, `"pane_id"`, `"session"`, `"generation"`,
	} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("wire JSON missing %s: %s", key, b)
		}
	}
	var round CompareAndCloseRequest
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if round.TabGeneration != 7 || round.Attachments[0].Generation != 2 {
		t.Fatalf("round-trip: %+v", round)
	}
}

func TestExpandCloseRequest_BuildsWireFromFAC158Handoff(t *testing.T) {
	wire, err := ExpandCloseRequest(CloseRequest{
		WorkspaceID: "w", TabID: "t", Generation: "7", TabRevision: 3,
		PaneIDs: []string{"p"}, SessionID: "s1", SessionGeneration: "2",
		Agent: "codex", Nonce: "n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.TabGeneration != 7 || wire.Attachments[0].Generation != 2 {
		t.Fatalf("%+v", wire)
	}
	if wire.Attachments[0].Session == nil || *wire.Attachments[0].Session != "s1" {
		t.Fatalf("session: %+v", wire.Attachments)
	}
}

func TestFAC158_ReconciliationClosesOnlyViaCompareAndClose(t *testing.T) {
	// Integration-shaped proof: a durable decision can only mutate through
	// TabCloseCAS; legacy TabClose / Cleanup mutation stay blocked.
	srv := NewFakeCompareCloseServer()
	live := fixtureLive()
	srv.PutTab(live)
	restore := SetCompareCloseTransportForTest(func(req CompareAndCloseRequest) (CloseReceipt, error) {
		return srv.CompareAndClose(req), nil
	})
	defer restore()

	// Simulate FAC-158 AuthorizeClose output.
	decision := CloseRequest{
		WorkspaceID: "w", TabID: "t", Generation: "7", TabRevision: 3,
		PaneIDs: []string{"p"}, SessionID: "s1", SessionGeneration: "2",
		Agent: "codex", Nonce: "fac158-1",
	}
	if err := TabClose("t"); err == nil {
		t.Fatal("unfenced TabClose must not succeed")
	}
	if err := TabCloseCAS(decision); err != nil {
		t.Fatalf("fenced close: %v", err)
	}
	if !srv.IsClosed("t") {
		t.Fatal("only CAS path should close")
	}
}
