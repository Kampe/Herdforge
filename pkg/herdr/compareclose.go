package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Compare-and-close (FAC-180) is the only close path autonomous Herdforge
// callers may use. The wire format matches Herdr's tab.compare_and_close /
// `herdr tab compare-close` request and receipt. Hermetic tests exercise the
// pure authority below; the production transport is the herdr CLI/socket.

// CompareAndCloseRequest is the exact generation-fenced close request.
type CompareAndCloseRequest struct {
	WorkspaceID   string       `json:"workspace_id"`
	TabID         string       `json:"tab_id"`
	TabGeneration uint64       `json:"tab_generation"`
	TabRevision   uint64       `json:"tab_revision"`
	PaneIDs       []string     `json:"pane_ids"`
	Attachments   []Attachment `json:"attachments"`
	Nonce         string       `json:"nonce"`
}

// Attachment is one pane's expected agent/session identity at close time.
type Attachment struct {
	PaneID     string  `json:"pane_id"`
	Agent      *string `json:"agent"`
	Session    *string `json:"session"`
	Generation uint64  `json:"generation"`
}

// LiveTab is the server-side re-read under the lifecycle lock.
type LiveTab struct {
	WorkspaceID      string       `json:"workspace_id"`
	TabID            string       `json:"tab_id"`
	Generation       uint64       `json:"generation"`
	Revision         uint64       `json:"revision"`
	PaneIDs          []string     `json:"pane_ids"`
	Attachments      []Attachment `json:"attachments"`
	MutationInFlight bool         `json:"mutation_in_flight"`
	Protected        bool         `json:"protected"`
}

// CompareCloseOutcome is a typed close result.
type CompareCloseOutcome string

const (
	// OutcomeIntent is a durable reservation written before mutate. It is NOT
	// final: a nonce whose latest record is only intent must re-read live state
	// and must never be replayed as a successful close.
	OutcomeIntent CompareCloseOutcome = "intent"
	// OutcomeClosed is final: mutate succeeded and resulting absence is claimed.
	OutcomeClosed CompareCloseOutcome = "closed"
	// OutcomeReplayed is reserved for wire compatibility; clients treat a
	// final Closed receipt on nonce retry as success without re-mutating.
	OutcomeReplayed CompareCloseOutcome = "replayed"
	// OutcomeAlreadyClosed is returned when the tab is absent under the server
	// lock. Callers must still prove absence via durable final receipt or a
	// live re-read — see TabCloseCAS.
	OutcomeAlreadyClosed     CompareCloseOutcome = "already_closed"
	OutcomeStaleGeneration   CompareCloseOutcome = "stale_generation"
	OutcomeAttachmentChanged CompareCloseOutcome = "attachment_changed"
	OutcomeActiveMutation    CompareCloseOutcome = "active_mutation"
	OutcomeProtected         CompareCloseOutcome = "protected"
	OutcomeUnknown           CompareCloseOutcome = "unknown"
	OutcomeError             CompareCloseOutcome = "error"
)

// isFinalOutcome reports whether a durable receipt may be replayed as the
// definitive result for its nonce. Intent is never final.
func isFinalOutcome(o CompareCloseOutcome) bool {
	switch o {
	case OutcomeIntent, "":
		return false
	default:
		return true
	}
}

// CloseReceipt binds request, pre-close identity, server generation, outcome,
// timestamp, and resulting absence. Append-only. A close attempt writes an
// intent record first, then a final outcome after mutate (or a final refusal
// without mutate). Nonce replay returns only final outcomes.
type CloseReceipt struct {
	Request          CompareAndCloseRequest `json:"request"`
	PreClose         LiveTab                `json:"pre_close"`
	ServerGeneration uint64                 `json:"server_generation"`
	Outcome          CompareCloseOutcome    `json:"outcome"`
	TimestampMS      uint64                 `json:"timestamp_ms"`
	ResultingAbsence bool                   `json:"resulting_absence"`
}

// ReceiptStore is the durable append-only close-receipt seam.
// Read returns the latest record for nonce (intent or final).
// ReadFinal returns the latest final outcome, or nil when only intent exists.
type ReceiptStore interface {
	Read(nonce string) (*CloseReceipt, error)
	ReadFinal(nonce string) (*CloseReceipt, error)
	Append(receipt *CloseReceipt) error
	Readback(nonce string) (*CloseReceipt, error)
}

// Clock supplies receipt timestamps (ms since epoch).
type Clock interface {
	NowMS() uint64
}

// FixedClock is a deterministic clock for tests.
type FixedClock struct{ MS uint64 }

func (c *FixedClock) NowMS() uint64 { return c.MS }

// SystemClock uses wall time.
type SystemClock struct{}

func (SystemClock) NowMS() uint64 {
	return uint64(time.Now().UnixMilli())
}

// MemoryReceiptStore is the hermetic receipt store used by unit tests and the
// in-process fake server. Records are append-only per nonce (intent then outcome).
type MemoryReceiptStore struct {
	mu           sync.Mutex
	receipts     map[string][]CloseReceipt
	FailWrite    bool
	FailReadback bool
}

func NewMemoryReceiptStore() *MemoryReceiptStore {
	return &MemoryReceiptStore{receipts: map[string][]CloseReceipt{}}
}

func (s *MemoryReceiptStore) Read(nonce string) (*CloseReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.receipts[nonce]
	if len(list) == 0 {
		return nil, nil
	}
	cp := list[len(list)-1]
	return &cp, nil
}

func (s *MemoryReceiptStore) ReadFinal(nonce string) (*CloseReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.receipts[nonce]
	for i := len(list) - 1; i >= 0; i-- {
		if isFinalOutcome(list[i].Outcome) {
			cp := list[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *MemoryReceiptStore) Append(receipt *CloseReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailWrite {
		return errors.New("receipt write failed")
	}
	if s.receipts == nil {
		s.receipts = map[string][]CloseReceipt{}
	}
	s.receipts[receipt.Request.Nonce] = append(s.receipts[receipt.Request.Nonce], *receipt)
	return nil
}

func (s *MemoryReceiptStore) Readback(nonce string) (*CloseReceipt, error) {
	if s.FailReadback {
		return nil, errors.New("receipt readback failed")
	}
	return s.Read(nonce)
}

// JSONLReceiptStore is an append-only file-backed store (mirrors Herdr's
// session data dir store). Paths must be relative or under a temp root in
// tests — never hard-coded absolute host paths in committed fixtures.
type JSONLReceiptStore struct {
	Path string
	mu   sync.Mutex
}

func (s *JSONLReceiptStore) records() ([]CloseReceipt, error) {
	text, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []CloseReceipt
	for _, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r CloseReceipt
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *JSONLReceiptStore) Read(nonce string) (*CloseReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.records()
	if err != nil {
		return nil, err
	}
	var last *CloseReceipt
	for i := range recs {
		if recs[i].Request.Nonce == nonce {
			cp := recs[i]
			last = &cp
		}
	}
	return last, nil
}

func (s *JSONLReceiptStore) ReadFinal(nonce string) (*CloseReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.records()
	if err != nil {
		return nil, err
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Request.Nonce == nonce && isFinalOutcome(recs[i].Outcome) {
			cp := recs[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *JSONLReceiptStore) Append(receipt *CloseReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (s *JSONLReceiptStore) Readback(nonce string) (*CloseReceipt, error) {
	return s.Read(nonce)
}

// ValidateCompareAndCloseRequest fails closed when fencing evidence is incomplete.
func ValidateCompareAndCloseRequest(req CompareAndCloseRequest) error {
	if req.WorkspaceID == "" || req.TabID == "" {
		return fmt.Errorf("compare-and-close: workspace_id and tab_id are required")
	}
	if req.TabGeneration == 0 {
		return fmt.Errorf("compare-and-close: positive tab_generation is required")
	}
	if req.Nonce == "" {
		return fmt.Errorf("compare-and-close: idempotency nonce is required")
	}
	// Session evidence: when any attachment is present, every attachment that
	// claims a session must also carry a positive generation. Empty pane sets
	// with empty attachments are allowed only for finished/orphan shells that
	// never had an agent — but generation/nonce/workspace/tab still fence.
	for _, a := range req.Attachments {
		if a.PaneID == "" {
			return fmt.Errorf("compare-and-close: attachment pane_id is required")
		}
		if a.Session != nil && *a.Session != "" && a.Generation == 0 {
			return fmt.Errorf("compare-and-close: session attachment requires positive generation")
		}
	}
	return nil
}

// CompareAndClose is the pure server authority.
//
// Protocol (two durable records for a successful close):
//  1. Replay only a *final* receipt for this nonce (never an intent).
//  2. Re-evaluate the live fence under the caller's lifecycle lock.
//  3. Terminal refusals (stale, attachment-changed, …) append one final record.
//  4. A close path appends OutcomeIntent, readbacks it, then mutates, then
//     appends a final OutcomeClosed or OutcomeError. A nonce with only intent
//     forces a live re-read on the next call — it is never success.
//
// Appending Closed only after mutate would reopen the crash-after-close window;
// appending Closed before mutate made failed mutates replay as success. Intent
// then outcome closes both holes.
func CompareAndClose(
	req CompareAndCloseRequest,
	live LiveTab,
	serverGeneration uint64,
	store ReceiptStore,
	clock Clock,
	closeMutate func() error,
) CloseReceipt {
	if store != nil {
		if final, err := store.ReadFinal(req.Nonce); err == nil && final != nil {
			return *final
		}
	}

	var ts uint64
	if clock != nil {
		ts = clock.NowMS()
	}
	base := CloseReceipt{
		Request:          req,
		PreClose:         live,
		ServerGeneration: serverGeneration,
		TimestampMS:      ts,
		ResultingAbsence: false,
	}

	if store == nil {
		base.Outcome = OutcomeError
		return base
	}

	// Fence evaluation against the live snapshot supplied under the server lock.
	outcome := OutcomeClosed
	if err := ValidateCompareAndCloseRequest(req); err != nil {
		outcome = OutcomeError
	} else if live.WorkspaceID != req.WorkspaceID || live.TabID != req.TabID ||
		live.Generation != req.TabGeneration || live.Revision != req.TabRevision {
		outcome = OutcomeStaleGeneration
	} else if !equalStringSlice(live.PaneIDs, req.PaneIDs) || !equalAttachments(live.Attachments, req.Attachments) {
		outcome = OutcomeAttachmentChanged
	} else if live.MutationInFlight {
		outcome = OutcomeActiveMutation
	} else if live.Protected {
		outcome = OutcomeProtected
	}

	// Terminal refusal: one final record, no mutation.
	if outcome != OutcomeClosed {
		base.Outcome = outcome
		if err := store.Append(&base); err != nil {
			base.Outcome = OutcomeError
			return base
		}
		return base
	}

	// Resume path: a prior attempt may have written intent only (crash or
	// mutate failure without a final outcome). Do not re-append intent.
	hasIntent := false
	if latest, err := store.Read(req.Nonce); err == nil && latest != nil && latest.Outcome == OutcomeIntent {
		hasIntent = true
	}
	if !hasIntent {
		intent := base
		intent.Outcome = OutcomeIntent
		intent.ResultingAbsence = false
		if err := store.Append(&intent); err != nil {
			base.Outcome = OutcomeError
			return base
		}
		readback, err := store.Readback(req.Nonce)
		if err != nil || readback == nil || readback.Outcome != OutcomeIntent {
			base.Outcome = OutcomeError
			return base
		}
	}

	if closeMutate == nil {
		fail := base
		fail.Outcome = OutcomeError
		_ = store.Append(&fail)
		return fail
	}
	if err := closeMutate(); err != nil {
		fail := base
		fail.Outcome = OutcomeError
		fail.ResultingAbsence = false
		// Durable final error so a retry does not invent Closed from a stale promise.
		if appendErr := store.Append(&fail); appendErr != nil {
			// Still return error to the caller; durable may only show intent.
			// Next call with unresolved intent re-reads live (must not report closed).
			return fail
		}
		return fail
	}

	closed := base
	closed.Outcome = OutcomeClosed
	closed.ResultingAbsence = true
	if err := store.Append(&closed); err != nil {
		// Mutate already happened. Return closed to this caller; durable may
		// only show intent. A later call re-reads live (tab absent) rather than
		// replaying a false Closed from a pre-mutate promise.
		return closed
	}
	return closed
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalAttachments(a, b []Attachment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PaneID != b[i].PaneID || a[i].Generation != b[i].Generation {
			return false
		}
		if !equalOptString(a[i].Agent, b[i].Agent) || !equalOptString(a[i].Session, b[i].Session) {
			return false
		}
	}
	return true
}

func equalOptString(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func receiptEqual(a, b *CloseReceipt) bool {
	if a == nil || b == nil {
		return a == b
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// ---------------------------------------------------------------------------
// Fake server (hermetic socket stand-in)
// ---------------------------------------------------------------------------

// FakeCompareCloseServer is an in-process lifecycle authority for tests. It
// holds tabs under a mutex, re-reads under that lock, and never touches a
// live herdr socket or host process.
type FakeCompareCloseServer struct {
	mu               sync.Mutex
	tabs             map[string]LiveTab
	closed           map[string]bool
	store            ReceiptStore
	clock            Clock
	serverGeneration atomic.Uint64
	// TransportErrors injects a provider/socket failure before the authority runs.
	TransportErrors error
}

// NewFakeCompareCloseServer constructs a hermetic server with a memory store.
func NewFakeCompareCloseServer() *FakeCompareCloseServer {
	s := &FakeCompareCloseServer{
		tabs:   map[string]LiveTab{},
		closed: map[string]bool{},
		store:  NewMemoryReceiptStore(),
		clock:  &FixedClock{MS: 1},
	}
	s.serverGeneration.Store(1)
	return s
}

// PutTab installs or replaces a live tab snapshot.
func (s *FakeCompareCloseServer) PutTab(tab LiveTab) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tabs[tab.TabID] = tab
	delete(s.closed, tab.TabID)
}

// AttachSession mutates the live attachment after a client readback — the
// classic TOCTOU race FAC-180 must refuse.
func (s *FakeCompareCloseServer) AttachSession(tabID, paneID, session string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tab, ok := s.tabs[tabID]
	if !ok {
		return
	}
	for i := range tab.Attachments {
		if tab.Attachments[i].PaneID == paneID {
			sess := session
			tab.Attachments[i].Session = &sess
			tab.Attachments[i].Generation = generation
		}
	}
	// Bump generation when a new agent attaches so an old decision that only
	// fences generation is also stale if the caller re-reads later.
	s.tabs[tabID] = tab
}

// RecycleTab replaces a tab_id with a new generation (ID reuse).
func (s *FakeCompareCloseServer) RecycleTab(tabID string, newGeneration uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tab, ok := s.tabs[tabID]
	if !ok {
		tab = LiveTab{TabID: tabID, WorkspaceID: "w"}
	}
	tab.Generation = newGeneration
	tab.Revision++
	s.tabs[tabID] = tab
	delete(s.closed, tabID)
}

// SetMutationInFlight marks start/resume/handoff in progress.
func (s *FakeCompareCloseServer) SetMutationInFlight(tabID string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tab := s.tabs[tabID]
	tab.MutationInFlight = v
	s.tabs[tabID] = tab
}

// SetProtected marks a tab as protected (e.g. last tab in a worktree group).
func (s *FakeCompareCloseServer) SetProtected(tabID string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tab := s.tabs[tabID]
	tab.Protected = v
	s.tabs[tabID] = tab
}

// IsClosed reports whether the tab was removed by a successful compare-close.
func (s *FakeCompareCloseServer) IsClosed(tabID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed[tabID]
}

// Live returns a copy of the current tab snapshot, if any.
func (s *FakeCompareCloseServer) Live(tabID string) (LiveTab, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tab, ok := s.tabs[tabID]
	return tab, ok && !s.closed[tabID]
}

// CompareAndClose runs the fenced close under the server lock.
func (s *FakeCompareCloseServer) CompareAndClose(req CompareAndCloseRequest) CloseReceipt {
	if s.TransportErrors != nil {
		return CloseReceipt{Request: req, Outcome: OutcomeError, ResultingAbsence: false}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Replay only final durable outcomes. Unresolved intent falls through to a
	// live re-read so a failed mutate cannot be retried as success.
	if final, err := s.store.ReadFinal(req.Nonce); err == nil && final != nil {
		return *final
	}
	if s.closed[req.TabID] {
		// Tab already removed under this server. Without a final Closed receipt
		// for this nonce, still report already_closed from live absence after
		// re-read — the pure path cannot invent Closed from intent alone.
		return CloseReceipt{
			Request:          req,
			Outcome:          OutcomeAlreadyClosed,
			ServerGeneration: s.serverGeneration.Add(1),
			TimestampMS:      s.clock.NowMS(),
			ResultingAbsence: true,
		}
	}
	live, ok := s.tabs[req.TabID]
	if !ok {
		return CloseReceipt{
			Request:          req,
			Outcome:          OutcomeUnknown,
			ServerGeneration: s.serverGeneration.Add(1),
			TimestampMS:      s.clock.NowMS(),
		}
	}
	gen := s.serverGeneration.Add(1)
	return CompareAndClose(req, live, gen, s.store, s.clock, func() error {
		s.closed[req.TabID] = true
		delete(s.tabs, req.TabID)
		return nil
	})
}

// ---------------------------------------------------------------------------
// Client transport + FAC-158 adapter
// ---------------------------------------------------------------------------

// CloseRequest is the FAC-158 authorization handoff into FAC-180. Incomplete
// fencing fields fail closed before any transport is contacted.
type CloseRequest struct {
	WorkspaceID       string   `json:"workspace_id"`
	TabID             string   `json:"tab_id"`
	Generation        string   `json:"generation"` // tab generation (string form from durable decision)
	TabRevision       uint64   `json:"tab_revision"`
	PaneIDs           []string `json:"pane_ids"`
	SessionID         string   `json:"session_id,omitempty"`
	SessionGeneration string   `json:"session_generation,omitempty"`
	Agent             string   `json:"agent,omitempty"`
	Nonce             string   `json:"nonce"`
}

// CloseUnavailableError is the typed BLOCKED outcome for autonomous close
// when fencing is incomplete or the server capability is not proven.
type CloseUnavailableError struct {
	TabID  string
	Reason string
}

func (e *CloseUnavailableError) Error() string {
	return fmt.Sprintf("tab %s: BLOCKED close unavailable: %s", e.TabID, e.Reason)
}

// ExpandCloseRequest converts a FAC-158 handoff into the wire request.
func ExpandCloseRequest(req CloseRequest) (CompareAndCloseRequest, error) {
	if req.TabID == "" {
		return CompareAndCloseRequest{}, &CloseUnavailableError{TabID: req.TabID, Reason: "tab_id is required"}
	}
	if req.WorkspaceID == "" {
		return CompareAndCloseRequest{}, &CloseUnavailableError{TabID: req.TabID, Reason: "workspace_id is required"}
	}
	if req.Generation == "" {
		return CompareAndCloseRequest{}, &CloseUnavailableError{TabID: req.TabID, Reason: "tab generation is required"}
	}
	if req.Nonce == "" {
		return CompareAndCloseRequest{}, &CloseUnavailableError{TabID: req.TabID, Reason: "idempotency nonce is required"}
	}
	gen, err := strconv.ParseUint(req.Generation, 10, 64)
	if err != nil || gen == 0 {
		return CompareAndCloseRequest{}, &CloseUnavailableError{TabID: req.TabID, Reason: "tab generation must be a positive integer"}
	}
	out := CompareAndCloseRequest{
		WorkspaceID:   req.WorkspaceID,
		TabID:         req.TabID,
		TabGeneration: gen,
		TabRevision:   req.TabRevision,
		PaneIDs:       append([]string(nil), req.PaneIDs...),
		Nonce:         req.Nonce,
	}
	if req.SessionID != "" {
		sessGen, err := strconv.ParseUint(req.SessionGeneration, 10, 64)
		if err != nil || sessGen == 0 {
			return CompareAndCloseRequest{}, &CloseUnavailableError{TabID: req.TabID, Reason: "session generation evidence is required with session_id"}
		}
		if len(req.PaneIDs) == 0 {
			return CompareAndCloseRequest{}, &CloseUnavailableError{TabID: req.TabID, Reason: "expected pane set is required with session attachment"}
		}
		sess := req.SessionID
		var agent *string
		if req.Agent != "" {
			a := req.Agent
			agent = &a
		}
		// One attachment per pane; session binds to the first pane when only
		// one session is provided by the FAC-158 decision.
		out.Attachments = make([]Attachment, 0, len(req.PaneIDs))
		for i, pane := range req.PaneIDs {
			att := Attachment{PaneID: pane, Generation: sessGen}
			if i == 0 {
				att.Session = &sess
				att.Agent = agent
			}
			out.Attachments = append(out.Attachments, att)
		}
	}
	if err := ValidateCompareAndCloseRequest(out); err != nil {
		return CompareAndCloseRequest{}, &CloseUnavailableError{TabID: req.TabID, Reason: err.Error()}
	}
	return out, nil
}

// CompareCloseTransport performs one compare-and-close RPC.
type CompareCloseTransport func(CompareAndCloseRequest) (CloseReceipt, error)

var (
	compareCloseTransportMu sync.Mutex
	compareCloseTransport   CompareCloseTransport = liveCompareCloseTransport
)

// SetCompareCloseTransportForTest installs a hermetic transport. Restore with
// the returned func.
func SetCompareCloseTransportForTest(t CompareCloseTransport) func() {
	compareCloseTransportMu.Lock()
	old := compareCloseTransport
	if t == nil {
		compareCloseTransport = liveCompareCloseTransport
	} else {
		compareCloseTransport = t
	}
	compareCloseTransportMu.Unlock()
	return func() {
		compareCloseTransportMu.Lock()
		compareCloseTransport = old
		compareCloseTransportMu.Unlock()
	}
}

func currentCompareCloseTransport() CompareCloseTransport {
	compareCloseTransportMu.Lock()
	defer compareCloseTransportMu.Unlock()
	return compareCloseTransport
}

// liveCompareCloseTransport calls `herdr tab compare-close '<json>'`. The
// installed binary may not yet expose the subcommand; that is a hard error
// (capability not proven), never a silent unfenced fallback to tab close.
func liveCompareCloseTransport(req CompareAndCloseRequest) (CloseReceipt, error) {
	if err := ValidateCompareAndCloseRequest(req); err != nil {
		return CloseReceipt{}, err
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return CloseReceipt{}, err
	}
	out, err := runHerdr("tab", "compare-close", string(payload))
	if err != nil {
		// Do not fall back to plain tab close — fail closed.
		return CloseReceipt{}, fmt.Errorf("herdr tab compare-close: %s: %w", out, err)
	}
	// Response shapes: either a bare receipt or {"result":{"receipt":...},"type":...}
	var direct CloseReceipt
	if err := json.Unmarshal([]byte(out), &direct); err == nil && direct.Outcome != "" {
		return direct, nil
	}
	var env struct {
		Result struct {
			Receipt CloseReceipt `json:"receipt"`
			Type    string       `json:"type"`
		} `json:"result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return CloseReceipt{}, fmt.Errorf("parse compare-close response: %s: %w", out, err)
	}
	if env.Error != nil {
		return CloseReceipt{}, fmt.Errorf("herdr compare-close %s: %s", env.Error.Code, env.Error.Message)
	}
	if env.Result.Receipt.Outcome == "" {
		return CloseReceipt{}, fmt.Errorf("herdr compare-close returned empty outcome: %s", out)
	}
	return env.Result.Receipt, nil
}

// CompareAndCloseTab is the public Herdforge entry for a fully-fenced close.
func CompareAndCloseTab(req CompareAndCloseRequest) (CloseReceipt, error) {
	if err := ValidateCompareAndCloseRequest(req); err != nil {
		return CloseReceipt{}, err
	}
	return currentCompareCloseTransport()(req)
}

// TabCloseCAS is the FAC-158 adapter: expand the durable decision, call the
// fenced operation, and treat only final Closed (with resulting absence) as
// success. Intent is never success. AlreadyClosed is accepted only when the
// server reported resulting absence after a live re-read (idempotent retry
// after a prior successful close). Every other typed outcome is a hard error
// so reconciliation never confuses "refused" with "gone".
func TabCloseCAS(req CloseRequest) error {
	wire, err := ExpandCloseRequest(req)
	if err != nil {
		return err
	}
	receipt, err := CompareAndCloseTab(wire)
	if err != nil {
		return &CloseUnavailableError{TabID: req.TabID, Reason: err.Error()}
	}
	switch receipt.Outcome {
	case OutcomeClosed:
		if !receipt.ResultingAbsence {
			return &CloseUnavailableError{TabID: req.TabID, Reason: "closed outcome without resulting absence"}
		}
		return nil
	case OutcomeReplayed:
		if !receipt.ResultingAbsence {
			return &CloseUnavailableError{TabID: req.TabID, Reason: "replayed outcome without resulting absence"}
		}
		return nil
	case OutcomeAlreadyClosed:
		if !receipt.ResultingAbsence {
			return &CloseUnavailableError{TabID: req.TabID, Reason: "already_closed without resulting absence"}
		}
		return nil
	case OutcomeIntent:
		return &CloseUnavailableError{TabID: req.TabID, Reason: "unresolved intent is not a close"}
	case OutcomeStaleGeneration:
		return &CloseUnavailableError{TabID: req.TabID, Reason: "stale-generation"}
	case OutcomeAttachmentChanged:
		return &CloseUnavailableError{TabID: req.TabID, Reason: "attachment-changed"}
	case OutcomeActiveMutation:
		return &CloseUnavailableError{TabID: req.TabID, Reason: "active-mutation"}
	case OutcomeProtected:
		return &CloseUnavailableError{TabID: req.TabID, Reason: "protected"}
	default:
		return &CloseUnavailableError{TabID: req.TabID, Reason: fmt.Sprintf("compare-and-close outcome %q", receipt.Outcome)}
	}
}

// StringPtr is a small helper for attachment fixtures.
func StringPtr(s string) *string { return &s }

// SortPaneIDs returns a deterministically ordered pane set copy.
func SortPaneIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Intentionally broken variants — mutation non-vacuity proofs only.
// Production code must never call these.
// ---------------------------------------------------------------------------

// compareAndCloseWithoutGenerationCheck is a mutation oracle: it drops the
// generation/revision fence. Tests prove a recycled tab would be closed.
func compareAndCloseWithoutGenerationCheck(
	req CompareAndCloseRequest,
	live LiveTab,
	serverGeneration uint64,
	store ReceiptStore,
	clock Clock,
	closeMutate func() error,
) CloseReceipt {
	if store != nil {
		if prior, err := store.Read(req.Nonce); err == nil && prior != nil {
			return *prior
		}
	}
	outcome := OutcomeClosed
	// Intentionally skip generation/revision.
	if live.WorkspaceID != req.WorkspaceID || live.TabID != req.TabID {
		outcome = OutcomeStaleGeneration
	} else if !equalStringSlice(live.PaneIDs, req.PaneIDs) || !equalAttachments(live.Attachments, req.Attachments) {
		outcome = OutcomeAttachmentChanged
	} else if live.MutationInFlight {
		outcome = OutcomeActiveMutation
	} else if live.Protected {
		outcome = OutcomeProtected
	}
	var ts uint64
	if clock != nil {
		ts = clock.NowMS()
	}
	receipt := CloseReceipt{
		Request: req, PreClose: live, ServerGeneration: serverGeneration,
		Outcome: outcome, TimestampMS: ts, ResultingAbsence: outcome == OutcomeClosed,
	}
	if store == nil || store.Append(&receipt) != nil {
		receipt.Outcome = OutcomeError
		receipt.ResultingAbsence = false
		return receipt
	}
	if receipt.Outcome == OutcomeClosed && closeMutate != nil {
		_ = closeMutate()
	}
	return receipt
}

// compareAndCloseWithoutAttachmentCheck drops the session/pane fence.
func compareAndCloseWithoutAttachmentCheck(
	req CompareAndCloseRequest,
	live LiveTab,
	serverGeneration uint64,
	store ReceiptStore,
	clock Clock,
	closeMutate func() error,
) CloseReceipt {
	if store != nil {
		if prior, err := store.Read(req.Nonce); err == nil && prior != nil {
			return *prior
		}
	}
	outcome := OutcomeClosed
	if live.WorkspaceID != req.WorkspaceID || live.TabID != req.TabID ||
		live.Generation != req.TabGeneration || live.Revision != req.TabRevision {
		outcome = OutcomeStaleGeneration
	} else if live.MutationInFlight {
		outcome = OutcomeActiveMutation
	} else if live.Protected {
		outcome = OutcomeProtected
	}
	// Intentionally skip attachment/pane equality.
	var ts uint64
	if clock != nil {
		ts = clock.NowMS()
	}
	receipt := CloseReceipt{
		Request: req, PreClose: live, ServerGeneration: serverGeneration,
		Outcome: outcome, TimestampMS: ts, ResultingAbsence: outcome == OutcomeClosed,
	}
	if store == nil || store.Append(&receipt) != nil {
		receipt.Outcome = OutcomeError
		receipt.ResultingAbsence = false
		return receipt
	}
	if receipt.Outcome == OutcomeClosed && closeMutate != nil {
		_ = closeMutate()
	}
	return receipt
}

// compareAndCloseWithoutMutationCheck drops the in-flight mutation fence.
func compareAndCloseWithoutMutationCheck(
	req CompareAndCloseRequest,
	live LiveTab,
	serverGeneration uint64,
	store ReceiptStore,
	clock Clock,
	closeMutate func() error,
) CloseReceipt {
	if store != nil {
		if prior, err := store.Read(req.Nonce); err == nil && prior != nil {
			return *prior
		}
	}
	outcome := OutcomeClosed
	if live.WorkspaceID != req.WorkspaceID || live.TabID != req.TabID ||
		live.Generation != req.TabGeneration || live.Revision != req.TabRevision {
		outcome = OutcomeStaleGeneration
	} else if !equalStringSlice(live.PaneIDs, req.PaneIDs) || !equalAttachments(live.Attachments, req.Attachments) {
		outcome = OutcomeAttachmentChanged
	} else if live.Protected {
		outcome = OutcomeProtected
	}
	// Intentionally skip MutationInFlight.
	var ts uint64
	if clock != nil {
		ts = clock.NowMS()
	}
	receipt := CloseReceipt{
		Request: req, PreClose: live, ServerGeneration: serverGeneration,
		Outcome: outcome, TimestampMS: ts, ResultingAbsence: outcome == OutcomeClosed,
	}
	if store == nil || store.Append(&receipt) != nil {
		receipt.Outcome = OutcomeError
		receipt.ResultingAbsence = false
		return receipt
	}
	if receipt.Outcome == OutcomeClosed && closeMutate != nil {
		_ = closeMutate()
	}
	return receipt
}

// compareAndCloseWithoutReceiptDurability closes before writing a receipt.
func compareAndCloseWithoutReceiptDurability(
	req CompareAndCloseRequest,
	live LiveTab,
	serverGeneration uint64,
	_ ReceiptStore,
	clock Clock,
	closeMutate func() error,
) CloseReceipt {
	// Intentionally skip store append/readback: mutate first.
	if closeMutate != nil {
		_ = closeMutate()
	}
	var ts uint64
	if clock != nil {
		ts = clock.NowMS()
	}
	return CloseReceipt{
		Request: req, PreClose: live, ServerGeneration: serverGeneration,
		Outcome: OutcomeClosed, TimestampMS: ts, ResultingAbsence: true,
	}
}
