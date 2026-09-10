package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/procsignal"
)

// OpencodeExport represents the top-level structured JSON emitted by `opencode export <session> --sanitize --pure`.
type OpencodeExport struct {
	Info     OpencodeExportInfo      `json:"info"`
	Messages []OpencodeExportMessage `json:"messages"`
}

// OpencodeExportInfo captures session metadata from opencode export.
type OpencodeExportInfo struct {
	ID        string `json:"id"`
	Directory string `json:"directory,omitempty"`
	Title     string `json:"title,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

// OpencodeExportMessage captures a single message in the exported session.
type OpencodeExportMessage struct {
	Info  OpencodeExportMessageInfo `json:"info"`
	Parts []OpencodeExportPart      `json:"parts,omitempty"`
}

// OpencodeExportMessageInfo holds metadata for a message.
type OpencodeExportMessageInfo struct {
	ID         string                    `json:"id"`
	SessionID  string                    `json:"sessionID"`
	Role       string                    `json:"role"`
	ParentID   string                    `json:"parentID"`
	ProviderID string                    `json:"providerID"`
	ModelID    string                    `json:"modelID"`
	Finish     *string                   `json:"finish"`
	Time       OpencodeExportMessageTime `json:"time"`
}

// OpencodeExportMessageTime holds message creation and completion timestamps (epoch milliseconds).
type OpencodeExportMessageTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed,omitempty"`
}

// OpencodeExportPart holds message part information.
type OpencodeExportPart struct {
	Type string `json:"type"`
}

// IdentityFence holds trustworthy Herdr agent metadata captured before export.
type IdentityFence struct {
	Name             string
	Kind             string
	SessionID        string
	SessionKind      string
	SessionSource    string
	PaneID           string
	TabID            string
	TerminalID       string
	Workspace        string
	Cwd              string
	Revision         uint64
	StateChangeSeq   uint64
	TabGeneration    uint64
	ExpectedModel    string
	ExpectedProvider string
	ExpectedAccount  string
}

// Verify compares the after snapshot against the before fence.
func (f IdentityFence) Verify(after kick.AgentEntry) error {
	if after.Name != f.Name {
		return fmt.Errorf("identity fence mismatch: name changed from %q to %q", f.Name, after.Name)
	}
	if after.Kind != f.Kind {
		return fmt.Errorf("identity fence mismatch: kind changed from %q to %q", f.Kind, after.Kind)
	}
	if after.Session.Value != f.SessionID {
		return fmt.Errorf("identity fence mismatch: session_id changed from %q to %q", f.SessionID, after.Session.Value)
	}
	if f.SessionKind != "" && after.Session.Kind != f.SessionKind {
		return fmt.Errorf("identity fence mismatch: session_kind changed from %q to %q", f.SessionKind, after.Session.Kind)
	}
	if f.SessionSource != "" && after.Session.Source != f.SessionSource {
		return fmt.Errorf("identity fence mismatch: session_source changed from %q to %q", f.SessionSource, after.Session.Source)
	}
	if after.PaneID != f.PaneID {
		return fmt.Errorf("identity fence mismatch: pane_id changed from %q to %q", f.PaneID, after.PaneID)
	}
	if after.TabID != f.TabID {
		return fmt.Errorf("identity fence mismatch: tab_id changed from %q to %q", f.TabID, after.TabID)
	}
	if after.TerminalID != f.TerminalID {
		return fmt.Errorf("identity fence mismatch: terminal_id changed from %q to %q", f.TerminalID, after.TerminalID)
	}
	if after.Workspace != f.Workspace {
		return fmt.Errorf("identity fence mismatch: workspace_id changed from %q to %q", f.Workspace, after.Workspace)
	}
	if NormalizePath(after.Cwd) != NormalizePath(f.Cwd) {
		return fmt.Errorf("identity fence mismatch: cwd changed from %q to %q", f.Cwd, after.Cwd)
	}
	if after.Revision != f.Revision {
		return fmt.Errorf("identity fence mismatch: revision changed from %d to %d", f.Revision, after.Revision)
	}
	if after.StateChangeSeq != f.StateChangeSeq {
		return fmt.Errorf("identity fence mismatch: state_change_seq changed from %d to %d", f.StateChangeSeq, after.StateChangeSeq)
	}
	if f.TabGeneration != 0 && after.TabGeneration != f.TabGeneration {
		return fmt.Errorf("identity fence mismatch: tab_generation changed from %d to %d", f.TabGeneration, after.TabGeneration)
	}
	if f.ExpectedModel != "" && after.ExpectedModel != f.ExpectedModel {
		return fmt.Errorf("identity fence mismatch: expected_model changed from %q to %q", f.ExpectedModel, after.ExpectedModel)
	}
	if f.ExpectedProvider != "" && after.ExpectedProvider != f.ExpectedProvider {
		return fmt.Errorf("identity fence mismatch: expected_provider changed from %q to %q", f.ExpectedProvider, after.ExpectedProvider)
	}
	return nil
}

// OpencodeExportRunner abstracts the capture of `opencode export` payloads for hermetic testing.
type OpencodeExportRunner func(ctx context.Context, sessionID string, targetDir string) ([]byte, error)

var defaultExportRunner OpencodeExportRunner = captureOpencodeExportLive

// SetDefaultExportRunner overrides the export runner for testing. Returns a restore function.
func SetDefaultExportRunner(runner OpencodeExportRunner) func() {
	prev := defaultExportRunner
	defaultExportRunner = runner
	return func() {
		defaultExportRunner = prev
	}
}

// NormalizePath canonicalizes platform paths without suffix/case ambiguity.
func NormalizePath(p string) string {
	if p == "" {
		return ""
	}
	cleaned := filepath.Clean(p)
	if abs, err := filepath.Abs(cleaned); err == nil {
		cleaned = abs
	}
	if realPath, err := filepath.EvalSymlinks(cleaned); err == nil {
		cleaned = realPath
	}
	return filepath.Clean(cleaned)
}

// captureOpencodeExportLive captures opencode export using a private temporary file regular FD.
// Capturing stdout via os/exec PIPE truncates at 64KiB (65536 bytes) with exit code 0.
// Writing to a regular file FD preserves full payloads without truncation.
// Real-time size monitoring enforces a hard 16 MiB limit to prevent unbounded disk growth.
func captureOpencodeExportLive(ctx context.Context, sessionID string, targetDir string) ([]byte, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("capture opencode export: empty session id")
	}

	// Bounded per-export deadline
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tmp, err := os.CreateTemp("", "opencode-export-*.json")
	if err != nil {
		return nil, fmt.Errorf("create tempfile for export capture: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	cmd := procsignal.CommandContext(ctx, "opencode", "export", sessionID, "--sanitize", "--pure")
	if targetDir != "" {
		cmd.Dir = targetDir
	}
	cmd.Stdout = tmp
	devNull, errNull := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if errNull == nil {
		defer devNull.Close()
		cmd.Stderr = devNull
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start opencode export: %w", err)
	}

	const maxExportBytes = 16 * 1024 * 1024
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = procsignal.CancelSpawnedProcess(cmd.Process)
			}
			<-done
			return nil, fmt.Errorf("opencode export timed out: %w", ctx.Err())
		case err := <-done:
			if err != nil {
				return nil, fmt.Errorf("opencode export failed: %w", err)
			}
			goto finished
		case <-ticker.C:
			fi, err := tmp.Stat()
			if err == nil && fi.Size() > maxExportBytes {
				if cmd.Process != nil {
					_ = procsignal.CancelSpawnedProcess(cmd.Process)
				}
				<-done
				return nil, fmt.Errorf("opencode export output exceeded maximum size limit of %d bytes", maxExportBytes)
			}
		}
	}

finished:
	// Read full payload
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek tempfile: %w", err)
	}

	lr := io.LimitReader(tmp, maxExportBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("read export tempfile: %w", err)
	}
	if int64(len(data)) > maxExportBytes {
		return nil, fmt.Errorf("opencode export output exceeded maximum size limit of %d bytes", maxExportBytes)
	}

	return data, nil
}

// ExtractTerminalEvidenceFromExport strictly parses and validates an opencode export payload,
// extracting minimal TerminalEvidence bound to the exact expected session, turn, and directory.
func ExtractTerminalEvidenceFromExport(data []byte, expectedSessionID string, expectedDirectory string, now time.Time, maxAge time.Duration) (*TerminalEvidence, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, errors.New("empty opencode export payload")
	}
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return nil, errors.New("opencode export must be a structured JSON object, not loose prose")
	}

	var exp OpencodeExport
	if err := json.Unmarshal([]byte(trimmed), &exp); err != nil {
		return nil, fmt.Errorf("decode opencode export JSON: %w", err)
	}

	// 1. Exact session ID binding
	if exp.Info.ID == "" {
		return nil, ErrMissingSessionID
	}
	if expectedSessionID != "" && exp.Info.ID != expectedSessionID {
		return nil, fmt.Errorf("%w: expected %s, export info has %s", ErrSessionMismatch, expectedSessionID, exp.Info.ID)
	}

	// 2. Strict directory binding (exact canonical path equality, no suffix/substring matches)
	if expectedDirectory != "" {
		if exp.Info.Directory == "" {
			return nil, errors.New("opencode export missing directory metadata")
		}
		normExp := NormalizePath(exp.Info.Directory)
		normTarget := NormalizePath(expectedDirectory)
		if normExp != normTarget {
			return nil, fmt.Errorf("opencode export directory mismatch: expected %s, got %s", expectedDirectory, exp.Info.Directory)
		}
	}

	// 3. Find latest user message (current turn boundary) and latest assistant message
	var latestUser *OpencodeExportMessage
	var latestAssistant *OpencodeExportMessage

	for i := range exp.Messages {
		msg := &exp.Messages[i]
		if strings.EqualFold(msg.Info.Role, "user") {
			latestUser = msg
		} else if strings.EqualFold(msg.Info.Role, "assistant") {
			latestAssistant = msg
		}
	}

	if latestUser == nil {
		return nil, errors.New("opencode export contains no user messages")
	}
	if latestAssistant == nil {
		return nil, errors.New("opencode export contains no assistant messages")
	}

	// Non-empty message IDs
	if latestUser.Info.ID == "" {
		return nil, errors.New("opencode export user message missing id")
	}
	if latestAssistant.Info.ID == "" {
		return nil, errors.New("opencode export assistant message missing id")
	}
	if latestAssistant.Info.ParentID == "" {
		return nil, errors.New("opencode export assistant message missing parentID")
	}

	// 4. Exact parent turn binding
	if latestAssistant.Info.ParentID != latestUser.Info.ID {
		return nil, fmt.Errorf("turn binding broken: assistant parentID %q does not match latest user message ID %q", latestAssistant.Info.ParentID, latestUser.Info.ID)
	}

	// Verify both message session IDs match export info session ID
	if latestUser.Info.SessionID != exp.Info.ID {
		return nil, fmt.Errorf("user message sessionID %q does not match export sessionID %q", latestUser.Info.SessionID, exp.Info.ID)
	}
	if latestAssistant.Info.SessionID != exp.Info.ID {
		return nil, fmt.Errorf("assistant message sessionID %q does not match export sessionID %q", latestAssistant.Info.SessionID, exp.Info.ID)
	}

	provider := latestAssistant.Info.ProviderID
	if provider == "" {
		return nil, ErrMissingProvider
	}
	model := latestAssistant.Info.ModelID
	if model == "" {
		return nil, ErrMissingModel
	}

	// 5. Determine finish reason and status
	finishReason := ""
	if latestAssistant.Info.Finish != nil {
		finishReason = *latestAssistant.Info.Finish
	}

	// In-flight check: null finish, tool-calls finish, or 0 completion time
	if finishReason == "" || finishReason == "null" || strings.EqualFold(finishReason, "tool-calls") || strings.EqualFold(finishReason, "tool_calls") {
		if strings.EqualFold(finishReason, "tool-calls") || strings.EqualFold(finishReason, "tool_calls") {
			finishReason = "tool_use"
		} else {
			finishReason = ""
		}
	}

	// 6. Timestamp extraction and freshness check
	if latestAssistant.Info.Time.Created <= 0 {
		return nil, errors.New("opencode export assistant message missing created timestamp")
	}

	createdTime := time.UnixMilli(latestAssistant.Info.Time.Created).UTC()
	inFlight := (finishReason == "" || finishReason == "tool_use") && latestAssistant.Info.Time.Completed <= 0

	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}

	var ts time.Time
	if inFlight {
		// Active ongoing turn: creation time must not be in the future
		if !now.IsZero() && createdTime.After(now.Add(1*time.Minute)) {
			return nil, fmt.Errorf("turn created timestamp in future: %s", createdTime.Format(time.RFC3339))
		}
		ts = createdTime
	} else {
		// Completed turn: completed timestamp is required and must be within maxAge lookback
		if latestAssistant.Info.Time.Completed <= 0 {
			return nil, errors.New("opencode export finished message missing completed timestamp")
		}
		completedTime := time.UnixMilli(latestAssistant.Info.Time.Completed).UTC()
		if !now.IsZero() {
			if now.Sub(completedTime) > maxAge || completedTime.After(now.Add(1*time.Minute)) {
				return nil, fmt.Errorf("%w: completed timestamp %s outside %s lookback", ErrStaleEvidence, completedTime.Format(time.RFC3339), maxAge)
			}
		}
		ts = completedTime
	}

	ev := &TerminalEvidence{
		SessionID:    exp.Info.ID,
		TurnID:       latestUser.Info.ID,
		Provider:     provider,
		Account:      "", // Account identity is not manufactured from export
		Model:        model,
		FinishReason: finishReason,
		Timestamp:    ts,
		CapturedAt:   now,
	}

	return ev, nil
}

// ResolveNativeAgentEvidenceWithFence fetches and parses authoritative structured evidence for an agent
// while enforcing a Herdr before/after identity fence to reject replaced or moved sessions.
func ResolveNativeAgentEvidenceWithFence(ctx context.Context, fence IdentityFence, fetchAfter func(name string) (*kick.AgentEntry, error), now time.Time, maxAge time.Duration) (*TerminalEvidence, SessionContext, string, error) {
	sctx := SessionContext{
		SessionID:  fence.SessionID,
		Provider:   fence.Kind,
		Now:        now,
		MaxAge:     maxAge,
		CapturedAt: now,
	}

	if fetchAfter == nil {
		return nil, sctx, "", errors.New("identity fence: fetchAfter callback is required for authoritative capture")
	}

	if !strings.EqualFold(fence.Kind, "opencode") || strings.TrimSpace(fence.SessionID) == "" {
		return nil, sctx, "", nil
	}

	runner := defaultExportRunner
	if runner == nil {
		runner = captureOpencodeExportLive
	}

	data, err := runner(ctx, fence.SessionID, fence.Cwd)
	if err != nil {
		return nil, sctx, "", fmt.Errorf("native export capture: %w", err)
	}

	// Check Herdr after-state identity fence
	after, err := fetchAfter(fence.Name)
	if err != nil {
		return nil, sctx, "", fmt.Errorf("fetch agent after export: %w", err)
	}
	if after == nil {
		return nil, sctx, "", errors.New("agent missing from fleet after export")
	}
	if err := fence.Verify(*after); err != nil {
		return nil, sctx, "", fmt.Errorf("identity fence rejected: %w", err)
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	ev, err := ExtractTerminalEvidenceFromExport(data, fence.SessionID, fence.Cwd, now, maxAge)
	if err != nil {
		return nil, sctx, "", fmt.Errorf("native evidence extraction: %w", err)
	}

	ev.CapturedAt = now
	sctx.CapturedAt = now
	sctx.Now = now

	// Verify model against expected model and provider from trusted authority.
	// If trusted census omits model/provider route, route is unbound and cannot be accepted.
	if strings.TrimSpace(fence.ExpectedModel) == "" || strings.TrimSpace(fence.ExpectedProvider) == "" {
		return nil, sctx, "", errors.New("unbound model route: expected model and provider required from trusted authority")
	}
	if ev.Model != fence.ExpectedModel {
		return nil, sctx, "", fmt.Errorf("model mismatch: expected %q, export has %q", fence.ExpectedModel, ev.Model)
	}
	if ev.Provider != fence.ExpectedProvider {
		return nil, sctx, "", fmt.Errorf("provider mismatch: expected %q, export has %q", fence.ExpectedProvider, ev.Provider)
	}

	if strings.TrimSpace(fence.ExpectedAccount) != "" {
		ev.Account = fence.ExpectedAccount
		sctx.Account = fence.ExpectedAccount
	}

	sctx.TurnID = ev.TurnID
	sctx.Model = ev.Model
	sctx.Provider = ev.Provider

	return ev, sctx, "", nil
}

// ResolveNativeAgentEvidence is the legacy compatibility wrapper around ResolveNativeAgentEvidenceWithFence.
func ResolveNativeAgentEvidence(ctx context.Context, sessionID string, kind string, cwd string, now time.Time, maxAge time.Duration) (*TerminalEvidence, SessionContext, string, error) {
	fence := IdentityFence{
		Kind:      kind,
		SessionID: sessionID,
		Cwd:       cwd,
	}
	mockFetch := func(name string) (*kick.AgentEntry, error) {
		return &kick.AgentEntry{
			Kind:    kind,
			Cwd:     cwd,
			Session: kick.AgentSession{Value: sessionID},
		}, nil
	}
	return ResolveNativeAgentEvidenceWithFence(ctx, fence, mockFetch, now, maxAge)
}
