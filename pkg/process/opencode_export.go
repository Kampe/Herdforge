package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
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

// captureOpencodeExportLive captures opencode export using a private temporary file regular FD.
// Capturing stdout via os/exec PIPE truncates at 64KiB (65536 bytes) with exit code 0.
// Writing to a regular file FD preserves full payloads without truncation.
func captureOpencodeExportLive(ctx context.Context, sessionID string, targetDir string) ([]byte, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("capture opencode export: empty session id")
	}

	tmp, err := os.CreateTemp("", "opencode-export-*.json")
	if err != nil {
		return nil, fmt.Errorf("create tempfile for export capture: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	cmd := exec.CommandContext(ctx, "opencode", "export", sessionID, "--sanitize", "--pure")
	if targetDir != "" {
		cmd.Dir = targetDir
	}
	cmd.Stdout = tmp
	cmd.Stderr = io.Discard

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("opencode export failed: %w", err)
	}

	// Read full payload with a bounded 16 MiB ceiling
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek tempfile: %w", err)
	}

	const maxExportBytes = 16 * 1024 * 1024
	lr := io.LimitReader(tmp, maxExportBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("read export tempfile: %w", err)
	}
	if len(data) > maxExportBytes {
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
	if expectedSessionID != "" && exp.Info.ID != expectedSessionID {
		return nil, fmt.Errorf("%w: expected %s, export info has %s", ErrSessionMismatch, expectedSessionID, exp.Info.ID)
	}
	if exp.Info.ID == "" {
		return nil, ErrMissingSessionID
	}

	// 2. Directory binding if expected
	if expectedDirectory != "" && exp.Info.Directory != "" {
		expDir := strings.TrimRight(exp.Info.Directory, "/")
		targetDir := strings.TrimRight(expectedDirectory, "/")
		if !strings.EqualFold(expDir, targetDir) && !strings.HasSuffix(targetDir, expDir) && !strings.HasSuffix(expDir, targetDir) {
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

	// 4. Link assistant turn to the latest user message
	if latestAssistant.Info.ParentID != "" && latestAssistant.Info.ParentID != latestUser.Info.ID {
		// Assistant parent does not link to latest user message; old turn cannot drive actions
		return nil, fmt.Errorf("turn binding broken: assistant parentID %q does not match latest user message ID %q", latestAssistant.Info.ParentID, latestUser.Info.ID)
	}

	turnID := latestUser.Info.ID
	if turnID == "" {
		turnID = latestAssistant.Info.ID
	}

	provider := latestAssistant.Info.ProviderID
	if provider == "" {
		provider = "opencode"
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
	var ts time.Time
	if latestAssistant.Info.Time.Completed > 0 {
		ts = time.UnixMilli(latestAssistant.Info.Time.Completed).UTC()
	} else if latestAssistant.Info.Time.Created > 0 {
		ts = time.UnixMilli(latestAssistant.Info.Time.Created).UTC()
	} else if exp.Info.CreatedAt > 0 {
		ts = time.UnixMilli(exp.Info.CreatedAt).UTC()
	} else {
		ts = now
	}

	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}
	if !now.IsZero() && !ts.IsZero() {
		if now.Sub(ts) > maxAge || ts.After(now.Add(1*time.Minute)) {
			return nil, fmt.Errorf("%w: observation timestamp %s outside %s lookback", ErrStaleEvidence, ts.Format(time.RFC3339), maxAge)
		}
	}

	ev := &TerminalEvidence{
		SessionID:    exp.Info.ID,
		TurnID:       turnID,
		Provider:     provider,
		Account:      "", // Account identity is not manufactured from export
		Model:        model,
		FinishReason: finishReason,
		Timestamp:    ts,
	}

	return ev, nil
}

// ResolveNativeAgentEvidence fetches and parses authoritative structured evidence for an agent
// if supported by its native runtime, otherwise returning nil evidence for fallback handling.
func ResolveNativeAgentEvidence(ctx context.Context, sessionID string, kind string, cwd string, now time.Time, maxAge time.Duration) (*TerminalEvidence, SessionContext, string, error) {
	sctx := SessionContext{
		SessionID: sessionID,
		Provider:  kind,
		Now:       now,
		MaxAge:    maxAge,
	}

	if !strings.EqualFold(kind, "opencode") || strings.TrimSpace(sessionID) == "" {
		return nil, sctx, "", nil
	}

	runner := defaultExportRunner
	if runner == nil {
		runner = captureOpencodeExportLive
	}

	data, err := runner(ctx, sessionID, cwd)
	if err != nil {
		return nil, sctx, "", fmt.Errorf("native export capture: %w", err)
	}

	ev, err := ExtractTerminalEvidenceFromExport(data, sessionID, cwd, now, maxAge)
	if err != nil {
		return nil, sctx, "", fmt.Errorf("native evidence extraction: %w", err)
	}

	sctx.TurnID = ev.TurnID
	sctx.Model = ev.Model
	sctx.Provider = ev.Provider

	return ev, sctx, "", nil
}
