package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/kick"
)

// generateLargeExportJSON generates a realistic opencode export JSON payload larger than 64KiB (>65536 bytes).
func generateLargeExportJSON(sessionID, userMsgID, assistantMsgID, finishReason, model string, completedAt time.Time) []byte {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree","title":"task turn"},"messages":[`, sessionID))

	// User message
	sb.WriteString(fmt.Sprintf(`{"info":{"id":%q,"sessionID":%q,"role":"user","parentID":"","providerID":"","modelID":"","time":{"created":%d}},"parts":[{"type":"text"}]},`,
		userMsgID, sessionID, completedAt.Add(-2*time.Minute).UnixMilli()))

	// Many intermediate messages/parts to exceed 64KiB
	for i := 0; i < 600; i++ {
		padding := strings.Repeat("x", 120)
		sb.WriteString(fmt.Sprintf(`{"info":{"id":"mid-%d","sessionID":%q,"role":"assistant","parentID":%q,"providerID":"litellm","modelID":%q,"finish":"tool-calls","time":{"created":%d,"completed":%d}},"parts":[{"type":"tool"},{"type":"text","content":%q}]},`,
			i, sessionID, userMsgID, model, completedAt.Add(-1*time.Minute).UnixMilli(), completedAt.Add(-50*time.Second).UnixMilli(), padding))
	}

	// Final assistant message
	finishField := "null"
	if finishReason != "" {
		finishField = fmt.Sprintf("%q", finishReason)
	}
	sb.WriteString(fmt.Sprintf(`{"info":{"id":%q,"sessionID":%q,"role":"assistant","parentID":%q,"providerID":"litellm","modelID":%q,"finish":%s,"time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}`,
		assistantMsgID, sessionID, userMsgID, model, finishField, completedAt.Add(-30*time.Second).UnixMilli(), completedAt.UnixMilli()))

	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func TestExtractTerminalEvidenceFromExport_LargePayloadAndFinishLength(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	userMsgID := "msg-user-101"
	asstMsgID := "msg-asst-102"
	model := "lazer/gemini-3.7-flash"

	// 1. Generate >64KiB payload with finish=length
	largePayload := generateLargeExportJSON(sessionID, userMsgID, asstMsgID, "length", model, now)
	if len(largePayload) <= 65536 {
		t.Fatalf("test payload must exceed 64KiB (65536 bytes), got %d bytes", len(largePayload))
	}

	ev, err := ExtractTerminalEvidenceFromExport(largePayload, sessionID, "/path/to/worktree", now, 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to extract terminal evidence from >64KiB payload: %v", err)
	}

	if ev.SessionID != sessionID {
		t.Errorf("expected session %s, got %s", sessionID, ev.SessionID)
	}
	if ev.TurnID != userMsgID {
		t.Errorf("expected turnID %s (latest user message id), got %s", userMsgID, ev.TurnID)
	}
	if ev.Model != model {
		t.Errorf("expected model %s, got %s", model, ev.Model)
	}
	if ev.FinishReason != "length" {
		t.Errorf("expected finish_reason 'length', got %s", ev.FinishReason)
	}
	if ev.Account != "" {
		t.Errorf("account must not be manufactured from export, got %q", ev.Account)
	}

	// 2. EvaluateEvidence on this extracted evidence: MUST BE Unknown / read_pane (cannot produce Pass/Done)
	ctx := SessionContext{
		SessionID: sessionID,
		TurnID:    userMsgID,
		Provider:  ev.Provider,
		Model:     ev.Model,
		Now:       now,
		MaxAge:    5 * time.Minute,
	}

	res := EvaluateEvidence(ev, ctx, "Verdict: PASS\nStatus: COMPLETE")
	if res.Class != Unknown {
		t.Errorf("extracted finish=length evidence must evaluate to Unknown, got %s", res.Class)
	}
	if res.Action != "read_pane" {
		t.Errorf("action must be read_pane, got %s", res.Action)
	}
}

func TestExtractTerminalEvidenceFromExport_SecurityMatrix(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"

	// Case 1: Wrong session ID
	payload := generateLargeExportJSON(sessionID, "u1", "a1", "stop", model, now)
	_, err := ExtractTerminalEvidenceFromExport(payload, "different-session-id", "/path/to/worktree", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "session_id mismatch") {
		t.Errorf("expected session mismatch error, got %v", err)
	}

	// Case 2: Broken parent link (assistant does not point to latest user turn)
	brokenParentPayload := []byte(fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[{"info":{"id":"u-old","sessionID":%q,"role":"user","time":{"created":%d}}},{"info":{"id":"a-old","sessionID":%q,"role":"assistant","parentID":"u-old","providerID":"litellm","modelID":%q,"time":{"created":%d,"completed":%d}}},{"info":{"id":"u-new","sessionID":%q,"role":"user","time":{"created":%d}}},{"info":{"id":"a-orphan","sessionID":%q,"role":"assistant","parentID":"u-old","providerID":"litellm","modelID":%q,"time":{"created":%d,"completed":%d}}}]}`,
		sessionID, sessionID, now.Add(-3*time.Minute).UnixMilli(), sessionID, model, now.Add(-3*time.Minute).UnixMilli(), now.Add(-2*time.Minute).UnixMilli(), sessionID, now.Add(-1*time.Minute).UnixMilli(), sessionID, model, now.Add(-1*time.Minute).UnixMilli(), now.UnixMilli()))
	_, err = ExtractTerminalEvidenceFromExport(brokenParentPayload, sessionID, "/path/to/worktree", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "turn binding broken") {
		t.Errorf("expected turn binding broken error, got %v", err)
	}

	// Case 3: Empty parent ID
	emptyParentPayload := []byte(fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}}},{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"","providerID":"litellm","modelID":%q,"time":{"created":%d,"completed":%d}}}]}`,
		sessionID, sessionID, now.Add(-2*time.Minute).UnixMilli(), sessionID, model, now.Add(-1*time.Minute).UnixMilli(), now.UnixMilli()))
	_, err = ExtractTerminalEvidenceFromExport(emptyParentPayload, sessionID, "/path/to/worktree", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "missing parentID") {
		t.Errorf("expected missing parentID error, got %v", err)
	}

	// Case 4: Foreign message session ID
	foreignSessionPayload := []byte(fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[{"info":{"id":"u-1","sessionID":"other-sess","role":"user","time":{"created":%d}}},{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":%q,"time":{"created":%d,"completed":%d}}}]}`,
		sessionID, now.Add(-2*time.Minute).UnixMilli(), sessionID, model, now.Add(-1*time.Minute).UnixMilli(), now.UnixMilli()))
	_, err = ExtractTerminalEvidenceFromExport(foreignSessionPayload, sessionID, "/path/to/worktree", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "does not match export sessionID") {
		t.Errorf("expected message session mismatch error, got %v", err)
	}

	// Case 5: Directory mismatch (suffix match attempt must be rejected)
	_, err = ExtractTerminalEvidenceFromExport(payload, sessionID, "/other/prefix/path/to/worktree", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "directory mismatch") {
		t.Errorf("expected directory mismatch error on suffix match, got %v", err)
	}

	// Case 6: Missing directory metadata in export
	noDirPayload := []byte(fmt.Sprintf(`{"info":{"id":%q},"messages":[{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}}},{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":%q,"time":{"created":%d,"completed":%d}}}]}`,
		sessionID, sessionID, now.Add(-2*time.Minute).UnixMilli(), sessionID, model, now.Add(-1*time.Minute).UnixMilli(), now.UnixMilli()))
	_, err = ExtractTerminalEvidenceFromExport(noDirPayload, sessionID, "/path/to/worktree", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "missing directory metadata") {
		t.Errorf("expected missing directory metadata error, got %v", err)
	}

	// Case 7: Stale completed timestamp (15 minutes old)
	stalePayload := generateLargeExportJSON(sessionID, "u1", "a1", "stop", model, now.Add(-15*time.Minute))
	_, err = ExtractTerminalEvidenceFromExport(stalePayload, sessionID, "/path/to/worktree", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("expected stale evidence error, got %v", err)
	}

	// Case 8: Old in-flight turn (started 15 minutes ago, but still in-flight with no completion time)
	// Must be accepted as valid active in-flight turn!
	inFlightPayload := []byte(fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}}},{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"litellm","modelID":%q,"finish":null,"time":{"created":%d}}}]}`,
		sessionID, sessionID, now.Add(-15*time.Minute).UnixMilli(), sessionID, model, now.Add(-15*time.Minute).UnixMilli()))
	inFlightEv, err := ExtractTerminalEvidenceFromExport(inFlightPayload, sessionID, "/path/to/worktree", now, 5*time.Minute)
	if err != nil {
		t.Fatalf("expected old in-flight turn to be accepted, got error: %v", err)
	}
	if inFlightEv.FinishReason != "" {
		t.Errorf("expected empty in-flight finish reason, got %q", inFlightEv.FinishReason)
	}
	if inFlightEv.CapturedAt.IsZero() {
		t.Errorf("expected CapturedAt to be set to current snapshot time")
	}

	// Validate against SessionContext: must not be rejected as stale
	sctx := SessionContext{
		SessionID:  sessionID,
		TurnID:     "u-1",
		Provider:   "litellm",
		Model:      model,
		Now:        now,
		MaxAge:     5 * time.Minute,
		CapturedAt: now,
	}
	if err := inFlightEv.Validate(sctx); err != nil {
		t.Fatalf("in-flight turn with fresh CapturedAt failed Validate: %v", err)
	}

	evalRes := EvaluateEvidence(inFlightEv, sctx, "")
	if evalRes.Class != Unknown || evalRes.Action != "read_pane" {
		t.Errorf("expected in-flight turn to evaluate to Unknown / read_pane, got class=%s action=%s", evalRes.Class, evalRes.Action)
	}
	if evalRes.Reason != "generation in progress" {
		t.Errorf("expected reason 'generation in progress', got %q", evalRes.Reason)
	}

	target := ClassifyTargetWithEvidence("pane-1", "forge-worker-1", "working", "", inFlightEv, sctx)
	if target.Class != Unknown || target.Action != "read_pane" {
		t.Errorf("expected target to be Unknown / read_pane, got class=%s action=%s", target.Class, target.Action)
	}

	// Case 9: Missing provider ID (no fallback to fabricated provider)
	noProviderPayload := []byte(fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree"},"messages":[{"info":{"id":"u-1","sessionID":%q,"role":"user","time":{"created":%d}}},{"info":{"id":"a-1","sessionID":%q,"role":"assistant","parentID":"u-1","providerID":"","modelID":%q,"finish":"stop","time":{"created":%d,"completed":%d}}}]}`,
		sessionID, sessionID, now.Add(-2*time.Minute).UnixMilli(), sessionID, model, now.Add(-1*time.Minute).UnixMilli(), now.UnixMilli()))
	_, err = ExtractTerminalEvidenceFromExport(noProviderPayload, sessionID, "/path/to/worktree", now, 5*time.Minute)
	if err == nil || !errors.Is(err, ErrMissingProvider) {
		t.Errorf("expected ErrMissingProvider error, got %v", err)
	}

	// Case 10: Loose prose with embedded JSON must be rejected
	looseProse := append([]byte("Log prefix:\n"), payload...)
	_, err = ExtractTerminalEvidenceFromExport(looseProse, sessionID, "/path/to/worktree", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "must be a structured JSON object") {
		t.Errorf("expected structured JSON requirement error on loose prose, got %v", err)
	}
}

func TestResolveNativeAgentEvidenceWithFence_BeforeAfterFencing(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"
	worktreeDir := filepath.Clean("/path/to/worktree")

	fakePayload := generateLargeExportJSON(sessionID, "u-42", "a-42", "length", model, now)

	restore := SetDefaultExportRunner(func(ctx context.Context, sid string, dir string) ([]byte, error) {
		if sid != sessionID {
			return nil, fmt.Errorf("unknown session: %s", sid)
		}
		return fakePayload, nil
	})
	defer restore()

	baseFence := IdentityFence{
		Name:           "forge-worker-1",
		Kind:           "opencode",
		SessionID:      sessionID,
		PaneID:         "p-100",
		TabID:          "t-100",
		TerminalID:     "term-100",
		Workspace:      "ws-main",
		Cwd:            worktreeDir,
		StateChangeSeq:   10,
		ExpectedModel:    model,
		ExpectedProvider: "litellm",
	}

	// 1. Success case: after matches before fence exactly
	fetchMatching := func(name string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:           "forge-worker-1",
			Kind:           "opencode",
			PaneID:         "p-100",
			TabID:          "t-100",
			TerminalID:     "term-100",
			Workspace:      "ws-main",
			Cwd:            worktreeDir,
			StateChangeSeq: 10,
			Session:        kick.AgentSession{Value: sessionID},
		}, nil
	}

	ev, sctx, _, err := ResolveNativeAgentEvidenceWithFence(context.Background(), baseFence, fetchMatching, now, 5*time.Minute)
	if err != nil {
		t.Fatalf("expected successful resolution with valid fence, got: %v", err)
	}
	if ev.TurnID != "u-42" || sctx.TurnID != "u-42" {
		t.Errorf("turn ID mismatch: %s / %s", ev.TurnID, sctx.TurnID)
	}

	// 2. StateChangeSeq changed during export (agent mutated/reused)
	fetchMutated := func(name string) (*kick.AgentEntry, error) {
		entry, _ := fetchMatching(name)
		entry.StateChangeSeq = 11
		return entry, nil
	}
	_, _, _, err = ResolveNativeAgentEvidenceWithFence(context.Background(), baseFence, fetchMutated, now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "state_change_seq changed") {
		t.Errorf("expected state_change_seq fence mismatch error, got: %v", err)
	}

	// 3. Pane ID changed during export (moved pane)
	fetchMoved := func(name string) (*kick.AgentEntry, error) {
		entry, _ := fetchMatching(name)
		entry.PaneID = "p-999"
		return entry, nil
	}
	_, _, _, err = ResolveNativeAgentEvidenceWithFence(context.Background(), baseFence, fetchMoved, now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "pane_id changed") {
		t.Errorf("expected pane_id fence mismatch error, got: %v", err)
	}

	// 4. Session ID replaced
	fetchReplaced := func(name string) (*kick.AgentEntry, error) {
		entry, _ := fetchMatching(name)
		entry.Session = kick.AgentSession{Value: "new-session-id"}
		return entry, nil
	}
	_, _, _, err = ResolveNativeAgentEvidenceWithFence(context.Background(), baseFence, fetchReplaced, now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "session_id changed") {
		t.Errorf("expected session_id fence mismatch error, got: %v", err)
	}
}

func TestCaptureOpencodeExportLive_RegularFDCapturer_SubprocessExecution(t *testing.T) {
	// Create a fake "opencode" binary in a temp directory
	tmpDir, err := os.MkdirTemp("", "fake-opencode-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	largePayload := generateLargeExportJSON(sessionID, "u1", "a1", "length", "litellm/model", now)

	payloadFile := filepath.Join(tmpDir, "payload.json")
	if err := os.WriteFile(payloadFile, largePayload, 0644); err != nil {
		t.Fatalf("write payload file: %v", err)
	}

	// Script outputs the payload file to stdout
	fakeBin := filepath.Join(tmpDir, "opencode")
	scriptContent := fmt.Sprintf("#!/bin/sh\ncat %s\n", payloadFile)
	if err := os.WriteFile(fakeBin, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("write fake opencode script: %v", err)
	}

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+":"+oldPath)
	defer os.Setenv("PATH", oldPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	captured, err := captureOpencodeExportLive(ctx, sessionID, tmpDir)
	if err != nil {
		t.Fatalf("captureOpencodeExportLive failed: %v", err)
	}

	if len(captured) != len(largePayload) {
		t.Errorf("expected %d bytes captured, got %d bytes", len(largePayload), len(captured))
	}
	if len(captured) <= 65536 {
		t.Errorf("expected payload to exceed 64KiB (65536 bytes), got %d", len(captured))
	}
}

func TestCaptureOpencodeExportLive_HangingCommand(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fake-opencode-hang-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	fakeBin := filepath.Join(tmpDir, "opencode")
	scriptContent := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(fakeBin, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("write fake opencode script: %v", err)
	}

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+":"+oldPath)
	defer os.Setenv("PATH", oldPath)

	// Explicit short context
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = captureOpencodeExportLive(ctx, "session-hang", "")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error for hanging command, got: %v", err)
	}
}

func TestCaptureOpencodeExportLive_HangingDescendant_Cleanup(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fake-opencode-descendant-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Script spawns a background child process that holds open inherited handles, then hangs
	fakeBin := filepath.Join(tmpDir, "opencode")
	scriptContent := "#!/bin/sh\n(sleep 30) & sleep 30\n"
	if err := os.WriteFile(fakeBin, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("write fake opencode script: %v", err)
	}

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+":"+oldPath)
	defer os.Setenv("PATH", oldPath)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = captureOpencodeExportLive(ctx, "session-descendant", "")
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error for hanging descendant command, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("hanging descendant process held wait too long: %v (expected <= 2s)", elapsed)
	}
}

func TestCaptureOpencodeExportLive_OversizeLimit(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fake-opencode-oversize-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Script generates infinite output
	fakeBin := filepath.Join(tmpDir, "opencode")
	scriptContent := "#!/bin/sh\nwhile true; do printf '%01024d' 0; done\n"
	if err := os.WriteFile(fakeBin, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("write fake opencode script: %v", err)
	}

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+":"+oldPath)
	defer os.Setenv("PATH", oldPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = captureOpencodeExportLive(ctx, "session-oversize", "")
	if err == nil || !strings.Contains(err.Error(), "maximum size limit") {
		t.Errorf("expected maximum size limit error on infinite output, got: %v", err)
	}
}

func TestIdentityFence_RevisionMismatch_Rejects(t *testing.T) {
	fence := IdentityFence{
		Name:      "worker",
		Kind:      "opencode",
		SessionID: "sess-1",
		Revision:  3,
	}
	after := kick.AgentEntry{
		Name:     "worker",
		Kind:     "opencode",
		Revision: 4,
		Session:  kick.AgentSession{Value: "sess-1"},
	}
	err := fence.Verify(after)
	if err == nil || !strings.Contains(err.Error(), "revision changed from 3 to 4") {
		t.Fatalf("expected revision mismatch error, got: %v", err)
	}
}

func TestIdentityFence_BlankAfterSessionKind_RejectsWhenExpectedNonEmpty(t *testing.T) {
	fence := IdentityFence{
		Name:        "worker",
		Kind:        "opencode",
		SessionID:   "sess-1",
		SessionKind: "id",
	}
	after := kick.AgentEntry{
		Name:    "worker",
		Kind:    "opencode",
		Session: kick.AgentSession{Value: "sess-1", Kind: ""},
	}
	err := fence.Verify(after)
	if err == nil || !strings.Contains(err.Error(), "session_kind changed") {
		t.Fatalf("expected blank after session_kind to be rejected, got: %v", err)
	}
}

func TestIdentityFence_BlankAfterSessionSource_RejectsWhenExpectedNonEmpty(t *testing.T) {
	fence := IdentityFence{
		Name:          "worker",
		Kind:          "opencode",
		SessionID:     "sess-1",
		SessionSource: "herdr:opencode",
	}
	after := kick.AgentEntry{
		Name:    "worker",
		Kind:    "opencode",
		Session: kick.AgentSession{Value: "sess-1", Source: ""},
	}
	err := fence.Verify(after)
	if err == nil || !strings.Contains(err.Error(), "session_source changed") {
		t.Fatalf("expected blank after session_source to be rejected, got: %v", err)
	}
}

func TestResolveNativeAgentEvidenceWithFence_ExpectedModelAndProviderMismatch(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	exportJSON := generateLargeExportJSON(sessionID, "u1", "a1", "length", "litellm/actual-model", now)

	prevRunner := defaultExportRunner
	defaultExportRunner = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return exportJSON, nil
	}
	defer func() { defaultExportRunner = prevRunner }()

	fence := IdentityFence{
		Name:             "worker",
		Kind:             "opencode",
		SessionID:        sessionID,
		Cwd:              "/path/to/worktree",
		ExpectedModel:    "litellm/expected-model",
		ExpectedProvider: "litellm",
	}

	fetchAfter := func(_ string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:    "worker",
			Kind:    "opencode",
			Cwd:     "/path/to/worktree",
			Session: kick.AgentSession{Value: sessionID},
		}, nil
	}

	_, _, _, err := ResolveNativeAgentEvidenceWithFence(context.Background(), fence, fetchAfter, now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "model mismatch") {
		t.Fatalf("expected model mismatch error, got: %v", err)
	}
}

func TestResolveNativeAgentEvidenceWithFence_PerLanePostCaptureTimestamp(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	// Create an in-flight turn that started 15 minutes ago
	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree","title":"in-flight"},"messages":[
		{"info":{"id":"u1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a1","sessionID":%q,"role":"assistant","parentID":"u1","providerID":"litellm","modelID":"model","finish":"","time":{"created":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.Add(-15*time.Minute).UnixMilli(), sessionID, now.Add(-15*time.Minute).UnixMilli())

	prevRunner := defaultExportRunner
	defaultExportRunner = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(exportJSON), nil
	}
	defer func() { defaultExportRunner = prevRunner }()

	fence := IdentityFence{
		Name:             "worker",
		Kind:             "opencode",
		SessionID:        sessionID,
		Cwd:              "/path/to/worktree",
		ExpectedModel:    "model",
		ExpectedProvider: "litellm",
	}

	fetchAfter := func(_ string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:             "worker",
			Kind:             "opencode",
			Cwd:              "/path/to/worktree",
			Session:          kick.AgentSession{Value: sessionID},
			ExpectedModel:    "model",
			ExpectedProvider: "litellm",
		}, nil
	}

	// Per-lane capture timestamp taken post-capture
	lanePostCaptureTime := time.Now().UTC()
	ev, sctx, _, err := ResolveNativeAgentEvidenceWithFence(context.Background(), fence, fetchAfter, lanePostCaptureTime, 5*time.Minute)
	if err != nil {
		t.Fatalf("ResolveNativeAgentEvidenceWithFence failed: %v", err)
	}

	// In-flight turn must evaluate fresh against post-capture timestamp
	if err := ev.Validate(sctx); err != nil {
		t.Errorf("expected in-flight turn to pass freshness validation with post-capture CapturedAt: %v", err)
	}

	// If CapturedAt exceeds maxAge (e.g. stale scan start time), Validate must reject it
	staleCtx := sctx
	staleCtx.CapturedAt = now.Add(-10 * time.Minute)
	staleEv := *ev
	staleEv.CapturedAt = now.Add(-10 * time.Minute)
	if err := staleEv.Validate(staleCtx); err == nil {
		t.Errorf("expected stale CapturedAt to be rejected by Validate")
	}
}

func TestResolveNativeAgentEvidenceWithFence_UnboundRoute_Rejected(t *testing.T) {
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	now := time.Now().UTC()
	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree","title":"test"},"messages":[
		{"info":{"id":"u1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a1","sessionID":%q,"role":"assistant","parentID":"u1","providerID":"litellm","modelID":"model","finish":"stop","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.Add(-1*time.Minute).UnixMilli(), sessionID, now.Add(-30*time.Second).UnixMilli(), now.UnixMilli())

	prevRunner := defaultExportRunner
	defaultExportRunner = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(exportJSON), nil
	}
	defer func() { defaultExportRunner = prevRunner }()

	fence := IdentityFence{
		Name:      "worker",
		Kind:      "opencode",
		SessionID: sessionID,
		Cwd:       "/path/to/worktree",
		// Omit ExpectedModel and ExpectedProvider -> Unbound route
	}

	fetchAfter := func(_ string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:    "worker",
			Kind:    "opencode",
			Cwd:     "/path/to/worktree",
			Session: kick.AgentSession{Value: sessionID},
		}, nil
	}

	_, _, _, err := ResolveNativeAgentEvidenceWithFence(context.Background(), fence, fetchAfter, time.Now().UTC(), 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "unbound model route") {
		t.Fatalf("expected unbound model route error, got: %v", err)
	}
}

func TestResolveNativeAgentEvidenceWithFence_DelayedExport_CapturedAtAfterFileRead(t *testing.T) {
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	now := time.Now().UTC()
	exportJSON := fmt.Sprintf(`{"info":{"id":%q,"directory":"/path/to/worktree","title":"delayed"},"messages":[
		{"info":{"id":"u1","sessionID":%q,"role":"user","time":{"created":%d}},"parts":[{"type":"text"}]},
		{"info":{"id":"a1","sessionID":%q,"role":"assistant","parentID":"u1","providerID":"litellm","modelID":"model","finish":"stop","time":{"created":%d,"completed":%d}},"parts":[{"type":"text"}]}
	]}`, sessionID, sessionID, now.UnixMilli(), sessionID, now.UnixMilli(), now.Add(100*time.Millisecond).UnixMilli())

	startTime := time.Now().UTC()
	prevRunner := defaultExportRunner
	defaultExportRunner = func(_ context.Context, _ string, _ string) ([]byte, error) {
		time.Sleep(150 * time.Millisecond)
		return []byte(exportJSON), nil
	}
	defer func() { defaultExportRunner = prevRunner }()

	fence := IdentityFence{
		Name:             "worker",
		Kind:             "opencode",
		SessionID:        sessionID,
		Cwd:              "/path/to/worktree",
		ExpectedModel:    "model",
		ExpectedProvider: "litellm",
	}

	fetchAfter := func(_ string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Name:             "worker",
			Kind:             "opencode",
			Cwd:              "/path/to/worktree",
			Session:          kick.AgentSession{Value: sessionID},
			ExpectedModel:    "model",
			ExpectedProvider: "litellm",
		}, nil
	}

	ev, sctx, _, err := ResolveNativeAgentEvidenceWithFence(context.Background(), fence, fetchAfter, time.Time{}, 5*time.Minute)
	if err != nil {
		t.Fatalf("ResolveNativeAgentEvidenceWithFence failed: %v", err)
	}

	// CapturedAt must be after the start time plus delay
	if ev.CapturedAt.Before(startTime.Add(100 * time.Millisecond)) {
		t.Errorf("CapturedAt %v was before expected post-export timestamp (startTime %v + 100ms)", ev.CapturedAt, startTime)
	}
	if sctx.CapturedAt.Before(startTime.Add(100 * time.Millisecond)) {
		t.Errorf("sctx.CapturedAt %v was before expected post-export timestamp", sctx.CapturedAt)
	}
}

func TestCaptureOpencodeExportLive_OwnedProcessGroupTeardown_NoOrphanDescendants(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fake-opencode-owned-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	childPIDFile := filepath.Join(tmpDir, "child.pid")
	grandchildPIDFile := filepath.Join(tmpDir, "grandchild.pid")
	fakeBin := filepath.Join(tmpDir, "opencode")

	// Script spawns a background child which itself spawns a background grandchild (sleep 60 &)
	scriptContent := fmt.Sprintf("#!/bin/sh\n(sleep 60 & echo $! > %s; wait) &\nCHILD_PID=$!\necho $CHILD_PID > %s\nwait\n", grandchildPIDFile, childPIDFile)
	if err := os.WriteFile(fakeBin, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("write fake opencode script: %v", err)
	}

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+":"+oldPath)
	defer os.Setenv("PATH", oldPath)

	ctx, cancel := context.WithTimeout(context.Background(), 1000*time.Millisecond)
	defer cancel()

	_, err = captureOpencodeExportLive(ctx, "session-owned", "")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error for hanging script, got: %v", err)
	}

	// Read child PID
	pidBytes, err := os.ReadFile(childPIDFile)
	if err != nil {
		t.Fatalf("failed to read child PID file: %v", err)
	}
	childPIDStr := strings.TrimSpace(string(pidBytes))
	childPID, err := strconv.Atoi(childPIDStr)
	if err != nil {
		t.Fatalf("invalid child PID %q: %v", childPIDStr, err)
	}

	// Read grandchild PID
	gcPIDBytes, err := os.ReadFile(grandchildPIDFile)
	if err != nil {
		t.Fatalf("failed to read grandchild PID file: %v", err)
	}
	gcPIDStr := strings.TrimSpace(string(gcPIDBytes))
	gcPID, err := strconv.Atoi(gcPIDStr)
	if err != nil {
		t.Fatalf("invalid grandchild PID %q: %v", gcPIDStr, err)
	}

	// Give a brief window (up to 500ms) to ensure process group SIGKILL completes
	deadline := time.Now().Add(500 * time.Millisecond)
	childAlive := true
	gcAlive := true
	for time.Now().Before(deadline) {
		if childAlive && syscall.Kill(childPID, 0) != nil {
			childAlive = false
		}
		if gcAlive && syscall.Kill(gcPID, 0) != nil {
			gcAlive = false
		}
		if !childAlive && !gcAlive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if childAlive {
		t.Errorf("child process PID %d is still alive after process group cancellation (orphaned)", childPID)
		_ = syscall.Kill(childPID, syscall.SIGKILL)
	}
	if gcAlive {
		t.Errorf("grandchild process PID %d is still alive after process group cancellation (orphaned)", gcPID)
		_ = syscall.Kill(gcPID, syscall.SIGKILL)
	}
}
