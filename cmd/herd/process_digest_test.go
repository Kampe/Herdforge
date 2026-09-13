package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/process"
)

// These fixtures exercise the CLI adapter, not the classifiers. pkg/process
// already has table tests for every bucket; what was never proven is that the
// command feeds them a LIVE input, which is why `herd process --json` shipped a
// hard-coded pane-demo record that exited 0.

const testWorkspace = "wT"

func agentIn(name, pane, status string) herdr.AgentEntry {
	return herdr.AgentEntry{Name: name, PaneID: pane, Status: status, Workspace: testWorkspace}
}

// stubProcessSources replaces the three read-only seams for one test.
func stubProcessSources(t *testing.T, available bool, agents []herdr.AgentEntry, listErr error, read func(ctx context.Context, pane string, lines int) (herdr.PaneObservation, error)) *int32 {
	t.Helper()
	var reads int32
	prevAvail, prevList, prevRead := processHerdrAvailable, processAgentList, processPaneRead
	processHerdrAvailable = func() bool { return available }
	// Existing fixtures describe a roster that DID prove its identity; the
	// unverified case is exercised explicitly by its own tests below.
	processAgentList = func(context.Context) ([]herdr.AgentEntry, bool, error) { return agents, true, listErr }
	processPaneRead = func(ctx context.Context, pane string, lines int) (herdr.PaneObservation, error) {
		atomic.AddInt32(&reads, 1)
		if read == nil {
			return herdr.PaneObservation{}, nil
		}
		return read(ctx, pane, lines)
	}
	t.Cleanup(func() {
		processHerdrAvailable, processAgentList, processPaneRead = prevAvail, prevList, prevRead
	})
	return &reads
}

func scan(t *testing.T, limits processScanLimits) (processScanResult, error) {
	t.Helper()
	return collectProcessDigest(context.Background(), testWorkspace, limits)
}

// Multiple real pane identities must each appear, carrying their own pane id
// and name rather than a placeholder.
func TestProcessDigestReportsEveryRealPaneIdentity(t *testing.T) {
	agents := []herdr.AgentEntry{
		agentIn("builder-a", "wT:p1", "working"),
		agentIn("reviewer-b", "wT:p2", "idle"),
		agentIn("scout-c", "wT:p3", "done"),
	}
	reads := stubProcessSources(t, true, agents, nil, func(_ context.Context, pane string, _ int) (herdr.PaneObservation, error) {
		return herdr.PaneObservation{Text: "tail for " + pane}, nil
	})
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(result.Digest.Items) != 3 {
		t.Fatalf("want 3 items, got %d: %+v", len(result.Digest.Items), result.Digest.Items)
	}
	seen := map[string]string{}
	for _, item := range result.Digest.Items {
		seen[item.Name] = item.PaneID
		if item.PaneID == "pane-demo" || item.Name == "agent-demo" {
			t.Fatal("the placeholder record is still being emitted")
		}
	}
	for name, pane := range map[string]string{"builder-a": "wT:p1", "reviewer-b": "wT:p2", "scout-c": "wT:p3"} {
		if seen[name] != pane {
			t.Fatalf("%s reported pane %q, want %q", name, seen[name], pane)
		}
	}
	if result.Partial {
		t.Fatalf("a complete sweep reported partial: %v", result.Unknowns)
	}
	if got := atomic.LoadInt32(reads); got != 3 {
		t.Fatalf("issued %d pane reads for 3 agents; the sweep is not one read per pane", got)
	}
}

// An actually empty fleet is a success with zero entries.
func TestProcessDigestEmptyFleetSucceedsWithZeroItems(t *testing.T) {
	reads := stubProcessSources(t, true, []herdr.AgentEntry{}, nil, nil)
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatalf("an empty fleet must succeed: %v", err)
	}
	if len(result.Digest.Items) != 0 || result.Partial {
		t.Fatalf("empty fleet produced %d items (partial=%v)", len(result.Digest.Items), result.Partial)
	}
	if got := atomic.LoadInt32(reads); got != 0 {
		t.Fatalf("empty fleet still issued %d pane reads", got)
	}
	data, err := process.DigestJSON(result.Digest.WorkspaceID, result.Digest.Items, nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded process.Digest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("digest JSON did not round-trip: %v", err)
	}
	if decoded.WorkspaceID != testWorkspace {
		t.Fatalf("digest lost its workspace scope: %q", decoded.WorkspaceID)
	}
}

// A roster that cannot be read is an ERROR. This is the distinction the card
// turns on: "nothing needs attention" and "I could not look" must never produce
// the same output.
func TestProcessDigestRosterFailureIsNotEmptySuccess(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available bool
		listErr   error
	}{
		{"herdr missing from PATH", false, nil},
		{"roster read failed", true, errors.New("herdr agent list: exit 1")},
		{"roster malformed", true, errors.New("parsing agent list: unexpected end of JSON input")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := stubProcessSources(t, tc.available, nil, tc.listErr, nil)
			result, err := scan(t, defaultProcessScanLimits())
			if err == nil {
				t.Fatalf("a broken roster reported success with %d items", len(result.Digest.Items))
			}
			if len(result.Digest.Items) != 0 {
				t.Fatalf("a failed scan still produced items: %+v", result.Digest.Items)
			}
			if got := atomic.LoadInt32(reads); got != 0 {
				t.Fatalf("a failed roster still issued %d pane reads", got)
			}
		})
	}
}

// A pane that cannot be read is reported as unknown and marks the digest
// partial; it is not silently classified from empty text as though it were
// quiet.
func TestProcessDigestPaneReadFailureIsReportedNotSwallowed(t *testing.T) {
	agents := []herdr.AgentEntry{agentIn("a", "wT:p1", "idle"), agentIn("b", "wT:p2", "idle")}
	stubProcessSources(t, true, agents, nil, func(_ context.Context, pane string, _ int) (herdr.PaneObservation, error) {
		if pane == "wT:p2" {
			return herdr.PaneObservation{}, fmt.Errorf("herdr pane read %s: exit 1", pane)
		}
		return herdr.PaneObservation{Text: "ok"}, nil
	})
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatalf("one unreadable pane must not fail the whole sweep: %v", err)
	}
	if !result.Partial {
		t.Fatal("a failed pane read did not mark the digest partial")
	}
	if !strings.Contains(strings.Join(result.Unknowns, "; "), "b: pane read failed") {
		t.Fatalf("the failure was not reported: %v", result.Unknowns)
	}
	if len(result.Digest.Items) != 2 {
		t.Fatalf("want both agents still listed, got %d", len(result.Digest.Items))
	}
}

// A TRANSPORT failure is not agent output, and the distinction is made where
// the envelope is decoded (pkg/herdr), not by sniffing decoded agent text here.
//
// The earlier version of this adapter inspected pane text for an "error" key.
// That was wrong in both directions, so this fixture asserts both: a typed
// transport error is reported and never classified, while a healthy pane whose
// literal output IS {"error":...} stays ordinary agent content.
func TestProcessDigestReportsTransportFailuresDistinctly(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"envelope", fmt.Errorf("herdr pane read wT:p1: %w: pane not found", herdr.ErrReadTransportEnvelope),
			"pane read returned a transport error envelope"},
		{"contradiction", fmt.Errorf("herdr pane read wT:p1: %w: boom", herdr.ErrReadTransportContradiction),
			"pane read returned an error and a result together"},
		{"oversized", fmt.Errorf("herdr pane read wT:p1: %w: stopped", herdr.ErrReadTransportOversized),
			"pane read exceeded the transport byte bound and was stopped"},
		{"ordinary", errors.New("exit status 1"), "pane read failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "done")}, nil,
				func(context.Context, string, int) (herdr.PaneObservation, error) {
					return herdr.PaneObservation{}, tc.err
				})
			result, err := scan(t, defaultProcessScanLimits())
			if err != nil {
				t.Fatalf("one failed pane must not fail the sweep: %v", err)
			}
			if !result.Partial {
				t.Fatal("a transport failure was not reported as partial")
			}
			if got := strings.Join(result.Unknowns, "; "); !strings.Contains(got, tc.want) {
				t.Fatalf("reason = %q, want it to contain %q", got, tc.want)
			}
			// Irrespective of the roster still claiming "done", an unreadable
			// pane classifies UNKNOWN: "I could not look" is not a verdict.
			item := result.Digest.Items[0]
			if item.Class != process.Unknown {
				t.Fatalf("unreadable pane classified %s, want UNKNOWN", item.Class)
			}
			if item.Tail != "" {
				t.Fatalf("transport failure text reached classification: %+v", item)
			}
		})
	}
}

// The converse, at this layer: a pane whose own output is literally an error
// object is agent content. It is classified, not reported as a tool failure.
func TestProcessDigestLiteralErrorJSONFromAPaneIsAgentContent(t *testing.T) {
	const body = `{"error":"example from the agent's own output"}`
	stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "idle")}, nil,
		func(context.Context, string, int) (herdr.PaneObservation, error) {
			return herdr.PaneObservation{Text: body}, nil
		})
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Partial {
		t.Fatalf("literal agent JSON was misread as a transport failure: %v", result.Unknowns)
	}
	if len(result.Digest.Items) != 1 || result.Digest.Items[0].Tail == "" {
		t.Fatalf("agent content did not reach classification: %+v", result.Digest.Items)
	}
}

// transportFailureReason must classify by typed error, never by message text.
func TestTransportFailureReasonIgnoresUntypedErrors(t *testing.T) {
	if _, ok := transportFailureReason(nil); ok {
		t.Fatal("nil reported as a transport failure")
	}
	if _, ok := transportFailureReason(errors.New("herdr read: transport reported an error")); ok {
		t.Fatal("an untyped error with a matching MESSAGE was accepted as typed")
	}
}

// Missing output is different from failed output: an agent with no pane id
// still appears, marked partial, without a pane read being attempted.
func TestProcessDigestAgentWithoutPaneIsReportedWithoutReading(t *testing.T) {
	agents := []herdr.AgentEntry{{Name: "no-pane", Status: "idle", Workspace: testWorkspace}}
	reads := stubProcessSources(t, true, agents, nil, nil)
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(reads); got != 0 {
		t.Fatalf("read a pane for an agent with no pane id (%d reads)", got)
	}
	if !result.Partial || len(result.Digest.Items) != 1 {
		t.Fatalf("want one partial item, got partial=%v items=%d", result.Partial, len(result.Digest.Items))
	}
}

// Pane text is capped BEFORE classification so one runaway pane cannot make
// the sweep unbounded.
//
// The assertion is on truncateTail and on the reported note, not on
// Target.Tail: pkg/process applies its own 220-char display cap, so a Tail
// length check would pass on that cap whether or not this one ran.
func TestProcessDigestCapsPaneTextBeforeClassification(t *testing.T) {
	huge := strings.Repeat("x", 4096) + "TAIL-END"

	got, truncated := truncateTail(huge, 64)
	if !truncated {
		t.Fatal("oversized text was not reported as truncated")
	}
	if len(got) != 64 {
		t.Fatalf("truncated to %d bytes, want 64", len(got))
	}
	// The END is kept, because that is where a verdict appears.
	if !strings.HasSuffix(got, "TAIL-END") {
		t.Fatalf("truncation dropped the end of the pane: %q", got)
	}
	if text, wasTruncated := truncateTail("short", 64); wasTruncated || text != "short" {
		t.Fatalf("text within the cap must pass through unchanged: %q %v", text, wasTruncated)
	}

	limits := defaultProcessScanLimits()
	limits.MaxTailBytes = 64
	stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "idle")}, nil,
		func(context.Context, string, int) (herdr.PaneObservation, error) {
			return herdr.PaneObservation{Text: huge}, nil
		})
	result, err := scan(t, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(result.Unknowns, "; "), "truncated to 64 bytes by this sweep") {
		t.Fatalf("the sweep did not report applying its cap: %v", result.Unknowns)
	}
	// A cut tail is incomplete evidence, so the digest must not call itself
	// complete. Reporting the truncation in Unknowns while still exiting 0 is
	// how a sweep that dropped the verdict reads as "nothing to see".
	if !result.Partial {
		t.Fatal("a sweep-truncated tail left the digest claiming to be complete")
	}
}

// The pane cap is a hard ceiling on fan-out, and the shortfall is reported
// rather than presented as a complete picture.
func TestProcessDigestBoundsFanoutByPaneCap(t *testing.T) {
	var agents []herdr.AgentEntry
	for i := 0; i < 25; i++ {
		agents = append(agents, agentIn(fmt.Sprintf("agent-%02d", i), fmt.Sprintf("wT:p%02d", i), "idle"))
	}
	limits := defaultProcessScanLimits()
	limits.MaxPanes = 5
	reads := stubProcessSources(t, true, agents, nil, func(context.Context, string, int) (herdr.PaneObservation, error) {
		return herdr.PaneObservation{Text: "ok"}, nil
	})
	result, err := scan(t, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(reads); got != 5 {
		t.Fatalf("issued %d pane reads against a cap of 5", got)
	}
	if !result.Partial || result.Skipped != 20 {
		t.Fatalf("cap shortfall not reported: partial=%v skipped=%d", result.Partial, result.Skipped)
	}
}

// A canceled sweep stops issuing reads and says so.
func TestProcessDigestCanceledSweepStopsAndReportsPartial(t *testing.T) {
	var agents []herdr.AgentEntry
	for i := 0; i < 5; i++ {
		agents = append(agents, agentIn(fmt.Sprintf("agent-%d", i), fmt.Sprintf("wT:p%d", i), "idle"))
	}
	reads := stubProcessSources(t, true, agents, nil, func(context.Context, string, int) (herdr.PaneObservation, error) {
		return herdr.PaneObservation{Text: "ok"}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := collectProcessDigest(ctx, testWorkspace, defaultProcessScanLimits())
	if err != nil {
		t.Fatalf("cancellation is a partial result, not a roster error: %v", err)
	}
	if got := atomic.LoadInt32(reads); got != 0 {
		t.Fatalf("a canceled sweep still issued %d pane reads", got)
	}
	if !result.Partial || result.Skipped != 5 {
		t.Fatalf("cancellation not reported: partial=%v skipped=%d", result.Partial, result.Skipped)
	}
	if !strings.Contains(strings.Join(result.Unknowns, "; "), "canceled") {
		t.Fatalf("cancellation reason missing: %v", result.Unknowns)
	}
}

// An expired deadline behaves the same way, and is distinguishable in the
// reported reason.
func TestProcessDigestExpiredDeadlineStopsAndReportsPartial(t *testing.T) {
	agents := []herdr.AgentEntry{agentIn("a", "wT:p1", "idle"), agentIn("b", "wT:p2", "idle")}
	reads := stubProcessSources(t, true, agents, nil, func(context.Context, string, int) (herdr.PaneObservation, error) {
		return herdr.PaneObservation{Text: "ok"}, nil
	})
	limits := defaultProcessScanLimits()
	limits.Deadline = -time.Second // already expired before the first read
	result, err := scan(t, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(reads); got != 0 {
		t.Fatalf("an expired deadline still issued %d pane reads", got)
	}
	if !result.Partial || !strings.Contains(strings.Join(result.Unknowns, "; "), "deadline") {
		t.Fatalf("deadline not reported: partial=%v unknowns=%v", result.Partial, result.Unknowns)
	}
}

// Scope is explicit. Agents outside the workspace, and agents whose workspace
// is unreported, are not swept: guessing would silently widen the read.
func TestProcessDigestScopeIsExplicitAndNarrow(t *testing.T) {
	agents := []herdr.AgentEntry{
		agentIn("mine", "wT:p1", "idle"),
		{Name: "other-workspace", PaneID: "wB:p1", Status: "idle", Workspace: "wB"},
		{Name: "unreported", PaneID: "wX:p1", Status: "idle"},
	}
	reads := stubProcessSources(t, true, agents, nil, func(context.Context, string, int) (herdr.PaneObservation, error) {
		return herdr.PaneObservation{Text: "ok"}, nil
	})
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(reads); got != 1 {
		t.Fatalf("swept %d panes; only the in-scope one may be read", got)
	}
	if len(result.Digest.Items) != 1 || result.Digest.Items[0].Name != "mine" {
		t.Fatalf("scope leaked: %+v", result.Digest.Items)
	}
}

func TestResolveProcessWorkspaceRequiresAnExplicitScope(t *testing.T) {
	if _, err := resolveProcessWorkspace("", ""); !errors.Is(err, errProcessNoWorkspace) {
		t.Fatalf("an unscoped sweep must refuse, got %v", err)
	}
	if ws, err := resolveProcessWorkspace("  wF ", "wK"); err != nil || ws != "wF" {
		t.Fatalf("flag must win and be trimmed: %q %v", ws, err)
	}
	if ws, err := resolveProcessWorkspace("", "wK"); err != nil || ws != "wK" {
		t.Fatalf("configured scope must be used: %q %v", ws, err)
	}
}

// The human format must not read like a complete picture when it is not.
func TestRenderProcessDigestTextStatesPartial(t *testing.T) {
	result := processScanResult{
		Digest:   process.Digest{WorkspaceID: testWorkspace, Items: []process.Target{{Name: "a", Class: process.Unknown, Action: "read_pane"}}},
		Unknowns: []string{"b: pane read failed"},
		Partial:  true,
	}
	out := renderProcessDigestText(result)
	for _, want := range []string{"PARTIAL", "b: pane read failed", testWorkspace} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output missing %q:\n%s", want, out)
		}
	}
	complete := renderProcessDigestText(processScanResult{Digest: process.Digest{WorkspaceID: testWorkspace}})
	if strings.Contains(complete, "PARTIAL") {
		t.Fatalf("a complete sweep was labelled partial:\n%s", complete)
	}
	if !strings.Contains(complete, "no agents in scope") {
		t.Fatalf("an empty digest should say so:\n%s", complete)
	}
}

// Truncation must not corrupt text. Slicing bytes mid-rune yields invalid
// UTF-8, which json.Marshal silently rewrites to U+FFFD — the operator would
// see mangled evidence with nothing saying so.
func TestTruncateTailPreservesUTF8OrReportsIncomplete(t *testing.T) {
	const multibyte = "αβγ日本語" // 2+2+2+3+3+3 = 15 bytes
	if len(multibyte) != 15 {
		t.Fatalf("fixture is %d bytes, expected 15", len(multibyte))
	}

	// A cap landing mid-rune moves the cut FORWARD to a rune boundary, so the
	// result is shorter than the cap but always valid.
	for _, capBytes := range []int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14} {
		got, truncated := truncateTail(multibyte, capBytes)
		if !truncated {
			t.Fatalf("cap %d: oversized text not reported truncated", capBytes)
		}
		if got == "" {
			t.Fatalf("cap %d: a boundary exists within the cap but text was refused", capBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("cap %d: truncation produced invalid UTF-8 %q", capBytes, got)
		}
		if len(got) > capBytes {
			t.Fatalf("cap %d: kept %d bytes", capBytes, len(got))
		}
		if !strings.HasSuffix(multibyte, got) {
			t.Fatalf("cap %d: %q is not a suffix of the pane text", capBytes, got)
		}
	}

	// No boundary within the cap: report incomplete rather than emit a
	// fragment of a rune.
	for _, capBytes := range []int{1, 2} {
		got, truncated := truncateTail(multibyte, capBytes)
		if !truncated || got != "" {
			t.Fatalf("cap %d: want incomplete (\"\", true), got (%q, %v)", capBytes, got, truncated)
		}
	}

	// The sweep reports that case as partial and as an explicit incomplete
	// value, not as a quietly short tail.
	limits := defaultProcessScanLimits()
	limits.MaxTailBytes = 1
	stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "idle")}, nil,
		func(context.Context, string, int) (herdr.PaneObservation, error) {
			return herdr.PaneObservation{Text: multibyte}, nil
		})
	result, err := scan(t, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial {
		t.Fatal("an unusable truncation did not mark the digest partial")
	}
	if got := strings.Join(result.Unknowns, "; "); !strings.Contains(got, "no UTF-8 boundary within 1 bytes") {
		t.Fatalf("incomplete tail not reported explicitly: %q", got)
	}
}

// --lines is user input. An arbitrary positive integer would walk past the
// bound the sweep exists to hold, and a negative one is a mistake, not a hint.
func TestResolveProcessLinesRejectsInvalidAndCapsDepth(t *testing.T) {
	if _, _, err := resolveProcessLines(-1, 50); !errors.Is(err, errProcessBadLines) {
		t.Fatalf("negative --lines accepted: %v", err)
	}
	got, clamped, err := resolveProcessLines(0, 50)
	if err != nil || clamped || got != 50 {
		t.Fatalf("unset --lines = (%d, %v, %v), want the default", got, clamped, err)
	}
	got, clamped, err = resolveProcessLines(10, 50)
	if err != nil || clamped || got != 10 {
		t.Fatalf("narrowing --lines = (%d, %v, %v)", got, clamped, err)
	}
	got, clamped, err = resolveProcessLines(100000, 50)
	if err != nil || !clamped || got != maxProcessPaneLines {
		t.Fatalf("oversized --lines = (%d, %v, %v), want a clamp to %d", got, clamped, err, maxProcessPaneLines)
	}
}

// The sweep bound must be finite on every path through the flag.
func TestResolveProcessDeadlineIsAlwaysFiniteAndBounded(t *testing.T) {
	if _, _, err := resolveProcessDeadline(-time.Second, 30*time.Second); !errors.Is(err, errProcessBadDeadline) {
		t.Fatalf("negative --deadline accepted: %v", err)
	}
	for _, requested := range []time.Duration{0, time.Millisecond, time.Second, 30 * time.Second, time.Hour, 1 << 62} {
		got, _, err := resolveProcessDeadline(requested, 30*time.Second)
		if err != nil {
			t.Fatalf("--deadline %s: %v", requested, err)
		}
		if got <= 0 || got > maxProcessDeadline {
			t.Fatalf("--deadline %s resolved to %s, outside (0, %s]", requested, got, maxProcessDeadline)
		}
	}
}

// The failure REASON names the class; the detail is herdr's own message and is
// the operator's only clue about why. It is carried, bounded and on one line.
func TestProcessDigestCarriesTheTransportDetail(t *testing.T) {
	readErr := fmt.Errorf("herdr pane read wT:p1: %w: pane wT:p1 is gone\nexit status 3",
		herdr.ErrReadTransportEnvelope)
	stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "idle")}, nil,
		func(context.Context, string, int) (herdr.PaneObservation, error) {
			return herdr.PaneObservation{}, readErr
		})
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(result.Unknowns, "; ")
	if !strings.Contains(got, "pane wT:p1 is gone") {
		t.Fatalf("the transport message was dropped: %q", got)
	}
	if !strings.Contains(got, "exit status 3") {
		t.Fatalf("the exit status was dropped: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("a multi-line reason would break the one-unknown-per-line report: %q", got)
	}
}

// errDetail bounds an arbitrarily long message and never emits invalid UTF-8.
func TestErrDetailIsBoundedSingleLineAndValid(t *testing.T) {
	if got := errDetail(nil); got != "" {
		t.Fatalf("errDetail(nil) = %q", got)
	}
	if got := errDetail(errors.New("a\nb\tc   d")); got != "a b c d" {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
	long := errors.New(strings.Repeat("日", 400)) // 1200 bytes
	got := errDetail(long)
	if len(got) > maxUnknownDetailBytes+len("...") {
		t.Fatalf("detail is %d bytes, past the %d bound", len(got), maxUnknownDetailBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("detail is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("a truncated detail must say so: %q", got)
	}
}

// Legacy raw text is classified — dropping real evidence would be worse — but
// the digest says the classification is unverified and refuses to call itself
// complete. A silent healthy verdict from an unverified payload is exactly the
// failure the envelope validation exists to prevent.
func TestProcessDigestMarksUnverifiedPaneTextPartial(t *testing.T) {
	stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "idle")}, nil,
		func(context.Context, string, int) (herdr.PaneObservation, error) {
			return herdr.PaneObservation{Text: "PASS: 12 tests", Unverified: true}, nil
		})
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial {
		t.Fatal("unverified pane text produced a digest that called itself complete")
	}
	if got := strings.Join(result.Unknowns, "; "); !strings.Contains(got, "classification is unverified") {
		t.Fatalf("the ambiguity was not made explicit: %q", got)
	}
	// The text is still classified: refusing it would discard real evidence.
	if len(result.Digest.Items) != 1 || result.Digest.Items[0].Tail == "" {
		t.Fatalf("unverified text was dropped instead of flagged: %+v", result.Digest.Items)
	}
}

// herdr's OWN truncation flag is separate from this sweep's byte cap, and is
// reported rather than dropped: a verdict may be missing for that reason alone.
func TestProcessDigestReportsHerdrsOwnTruncation(t *testing.T) {
	stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "idle")}, nil,
		func(context.Context, string, int) (herdr.PaneObservation, error) {
			return herdr.PaneObservation{Text: "...tail", Truncated: true}, nil
		})
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial {
		t.Fatal("a herdr-truncated tail produced a complete digest")
	}
	if got := strings.Join(result.Unknowns, "; "); !strings.Contains(got, "herdr reported the pane tail as truncated") {
		t.Fatalf("herdr's truncation flag was dropped: %q", got)
	}
}

// A verified, untruncated read stays clean — the flags above must not make
// every sweep partial.
func TestProcessDigestVerifiedReadStaysComplete(t *testing.T) {
	stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "idle")}, nil,
		func(context.Context, string, int) (herdr.PaneObservation, error) {
			return herdr.PaneObservation{Text: "PASS: 12 tests"}, nil
		})
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Partial || len(result.Unknowns) != 0 {
		t.Fatalf("a clean read was reported partial: %v", result.Unknowns)
	}
}

// The new transport failure kinds each get a distinct reason, and each leaves
// the pane UNKNOWN rather than letting stale roster status stand in for one.
func TestProcessDigestNamesStructuredTransportFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"malformed", herdr.ErrReadTransportMalformed, "structured response that could not be decoded"},
		{"unsupported body", herdr.ErrReadTransportUnsupportedResult, "unsupported result body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readErr := fmt.Errorf("herdr pane read wT:p1: %w: detail", tc.err)
			stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "done")}, nil,
				func(context.Context, string, int) (herdr.PaneObservation, error) {
					return herdr.PaneObservation{}, readErr
				})
			result, err := scan(t, defaultProcessScanLimits())
			if err != nil {
				t.Fatal(err)
			}
			if !result.Partial {
				t.Fatal("a structured transport failure was not reported as partial")
			}
			if got := strings.Join(result.Unknowns, "; "); !strings.Contains(got, tc.want) {
				t.Fatalf("reason = %q, want it to contain %q", got, tc.want)
			}
			if item := result.Digest.Items[0]; item.Class != process.Unknown {
				t.Fatalf("classified %s despite a failed read, want UNKNOWN", item.Class)
			}
		})
	}
}

// REGRESSION (FAC-36 / PR839): a roster that did not prove it came from herdr
// can never produce a clean digest, and an EMPTY one least of all.
//
// The decoder used to accept any non-null result, so an id-less
// {"result":{"agents":[]}} reached this adapter as a perfectly ordinary empty
// fleet: zero items, partial false, exit 0. "I could not establish that I was
// talking to herdr" and "the fleet is empty" are opposite operational facts.
func TestProcessDigestUnverifiedRosterIsNeverACleanFleet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agents []herdr.AgentEntry
	}{
		{"empty", nil},
		{"populated", []herdr.AgentEntry{agentIn("a", "wT:p1", "idle")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prevList := processAgentList
			prevAvail := processHerdrAvailable
			prevRead := processPaneRead
			processHerdrAvailable = func() bool { return true }
			processAgentList = func(context.Context) ([]herdr.AgentEntry, bool, error) {
				return tc.agents, false, nil // decoded, but identity was absent
			}
			processPaneRead = func(context.Context, string, int) (herdr.PaneObservation, error) {
				return herdr.PaneObservation{Text: "ok"}, nil
			}
			t.Cleanup(func() {
				processAgentList, processHerdrAvailable, processPaneRead = prevList, prevAvail, prevRead
			})

			result, err := scan(t, defaultProcessScanLimits())
			if err != nil {
				t.Fatalf("an unverified roster is a partial result, not a hard error: %v", err)
			}
			if !result.Partial {
				t.Fatal("an unverified roster produced a digest that called itself complete")
			}
			if got := strings.Join(result.Unknowns, "; "); !strings.Contains(got, "no valid herdr identity") {
				t.Fatalf("the unverified roster was not reported: %q", got)
			}
		})
	}
}

// The positive half: a verified roster with zero in-scope agents is still a
// clean, complete, empty digest, so the rule above cannot be satisfied by
// marking every sweep partial.
func TestProcessDigestVerifiedEmptyRosterStaysClean(t *testing.T) {
	stubProcessSources(t, true, nil, nil, nil)
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Partial || len(result.Unknowns) != 0 {
		t.Fatalf("a verified empty fleet was reported partial: %v", result.Unknowns)
	}
	if len(result.Digest.Items) != 0 || result.PaneReads != 0 {
		t.Fatalf("items=%d reads=%d, want a clean zero digest", len(result.Digest.Items), result.PaneReads)
	}
}

// An unverified PANE read is reported too, and its text still reaches the
// classifier rather than being discarded.
func TestProcessDigestUnverifiedPaneReadIsReportedNotTrusted(t *testing.T) {
	stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "idle")}, nil,
		func(context.Context, string, int) (herdr.PaneObservation, error) {
			return herdr.PaneObservation{Text: "PASS: 12 tests", Unverified: true}, nil
		})
	result, err := scan(t, defaultProcessScanLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial {
		t.Fatal("an unverified pane read produced a complete digest")
	}
	if len(result.Digest.Items) != 1 || result.Digest.Items[0].Tail == "" {
		t.Fatalf("unverified text was dropped instead of flagged: %+v", result.Digest.Items)
	}
}
