package process

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
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

func TestExtractTerminalEvidenceFromExport_SecurityValidations(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"

	// Case 1: Wrong session ID
	payload := generateLargeExportJSON(sessionID, "u1", "a1", "stop", model, now)
	_, err := ExtractTerminalEvidenceFromExport(payload, "different-session-id", "", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "session_id mismatch") {
		t.Errorf("expected session mismatch error, got %v", err)
	}

	// Case 2: Broken parent link (assistant does not point to latest user turn)
	brokenParentPayload := []byte(fmt.Sprintf(`{"info":{"id":%q},"messages":[{"info":{"id":"u-old","role":"user"}},{"info":{"id":"a-old","role":"assistant","parentID":"u-old"}},{"info":{"id":"u-new","role":"user"}},{"info":{"id":"a-orphan","role":"assistant","parentID":"u-old","providerID":"litellm","modelID":%q,"time":{"completed":%d}}}]}`,
		sessionID, model, now.UnixMilli()))
	_, err = ExtractTerminalEvidenceFromExport(brokenParentPayload, sessionID, "", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "turn binding broken") {
		t.Errorf("expected turn binding broken error, got %v", err)
	}

	// Case 3: Stale timestamp (15 minutes old)
	stalePayload := generateLargeExportJSON(sessionID, "u1", "a1", "stop", model, now.Add(-15*time.Minute))
	_, err = ExtractTerminalEvidenceFromExport(stalePayload, sessionID, "", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("expected stale evidence error, got %v", err)
	}

	// Case 4: Loose prose with embedded JSON must be rejected
	looseProse := append([]byte("Log prefix:\n"), payload...)
	_, err = ExtractTerminalEvidenceFromExport(looseProse, sessionID, "", now, 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "must be a structured JSON object") {
		t.Errorf("expected structured JSON requirement error on loose prose, got %v", err)
	}
}

func TestResolveNativeAgentEvidence_WithExportRunner(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	sessionID := "019fc450-7ce2-7602-a62c-329f31271c7a"
	model := "lazer/gemini-3.7-flash"

	fakePayload := generateLargeExportJSON(sessionID, "u-42", "a-42", "length", model, now)

	restore := SetDefaultExportRunner(func(ctx context.Context, sid string, dir string) ([]byte, error) {
		if sid != sessionID {
			return nil, fmt.Errorf("unknown session: %s", sid)
		}
		return fakePayload, nil
	})
	defer restore()

	ev, sctx, _, err := ResolveNativeAgentEvidence(context.Background(), sessionID, "opencode", "/path/to/worktree", now, 5*time.Minute)
	if err != nil {
		t.Fatalf("ResolveNativeAgentEvidence failed: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil evidence")
	}
	if ev.FinishReason != "length" {
		t.Errorf("expected finish_reason length, got %s", ev.FinishReason)
	}
	if sctx.TurnID != "u-42" {
		t.Errorf("expected sctx.TurnID to be bound to u-42, got %s", sctx.TurnID)
	}

	// Non-opencode harness returns nil evidence cleanly
	evClaude, _, _, errClaude := ResolveNativeAgentEvidence(context.Background(), "claude-sess", "claude", "", now, 5*time.Minute)
	if errClaude != nil || evClaude != nil {
		t.Errorf("non-opencode harness should return nil evidence with no error, got ev=%+v err=%v", evClaude, errClaude)
	}
}
