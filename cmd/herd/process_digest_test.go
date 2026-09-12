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
func stubProcessSources(t *testing.T, available bool, agents []herdr.AgentEntry, listErr error, read func(pane string, lines int) (string, error)) *int32 {
	t.Helper()
	var reads int32
	prevAvail, prevList, prevRead := processHerdrAvailable, processAgentList, processPaneRead
	processHerdrAvailable = func() bool { return available }
	processAgentList = func(context.Context) ([]herdr.AgentEntry, error) { return agents, listErr }
	processPaneRead = func(pane string, lines int) (string, error) {
		atomic.AddInt32(&reads, 1)
		if read == nil {
			return "", nil
		}
		return read(pane, lines)
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
	reads := stubProcessSources(t, true, agents, nil, func(pane string, _ int) (string, error) {
		return "tail for " + pane, nil
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
	stubProcessSources(t, true, agents, nil, func(pane string, _ int) (string, error) {
		if pane == "wT:p2" {
			return "", fmt.Errorf("herdr pane read %s: exit 1", pane)
		}
		return "ok", nil
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

// A tool's JSON error envelope is not agent output. Classifying it would let a
// TOOL failure be read as an AGENT verdict.
func TestProcessDigestRejectsToolErrorEnvelopes(t *testing.T) {
	for _, body := range []string{
		`{"error":"pane not found"}`,
		`{"ok":false,"result":null}`,
		`  {"error":{"code":7,"message":"herdr failed"}}  `,
	} {
		t.Run(body, func(t *testing.T) {
			if !isToolErrorEnvelope(body) {
				t.Fatalf("envelope not recognised: %s", body)
			}
			stubProcessSources(t, true, []herdr.AgentEntry{agentIn("a", "wT:p1", "idle")}, nil,
				func(string, int) (string, error) { return body, nil })
			result, err := scan(t, defaultProcessScanLimits())
			if err != nil {
				t.Fatal(err)
			}
			if !result.Partial {
				t.Fatal("a tool error envelope was accepted as agent output")
			}
			if item := result.Digest.Items[0]; item.Tail != "" {
				t.Fatalf("the envelope text reached classification as a tail: %+v", item)
			}
		})
	}
	// The converse: ordinary agent output, and a plain JSON result envelope,
	// must NOT be mistaken for an error.
	for _, body := range []string{"build passed", `{"result":{"text":"done"}}`, `{"ok":true}`, ""} {
		if isToolErrorEnvelope(body) {
			t.Fatalf("ordinary output misread as a tool error: %q", body)
		}
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
		func(string, int) (string, error) { return huge, nil })
	result, err := scan(t, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(result.Unknowns, "; "), "truncated to 64 bytes") {
		t.Fatalf("the sweep did not report applying its cap: %v", result.Unknowns)
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
	reads := stubProcessSources(t, true, agents, nil, func(string, int) (string, error) { return "ok", nil })
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
	reads := stubProcessSources(t, true, agents, nil, func(string, int) (string, error) { return "ok", nil })
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
	reads := stubProcessSources(t, true, agents, nil, func(string, int) (string, error) { return "ok", nil })
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
	reads := stubProcessSources(t, true, agents, nil, func(string, int) (string, error) { return "ok", nil })
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
