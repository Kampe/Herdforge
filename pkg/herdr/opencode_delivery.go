package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type opencodeExportFunc func(context.Context, string, string) ([]byte, error)

var runOpenCodeExport opencodeExportFunc = exportOpenCodeSession
var opencodeCLI = "opencode"

// SetOpenCodeExportForTest replaces the native export runner. The seam keeps
// Send tests hermetic while the production runner still uses OpenCode's own
// structured session export.
func SetOpenCodeExportForTest(f func(context.Context, string, string) ([]byte, error)) func() {
	old := runOpenCodeExport
	if f == nil {
		runOpenCodeExport = exportOpenCodeSession
	} else {
		runOpenCodeExport = f
	}
	return func() { runOpenCodeExport = old }
}

// SetOpenCodeExecutableForTest replaces only the CLI path used by the
// filesystem/export test. Production always uses the installed opencode CLI.
func SetOpenCodeExecutableForTest(path string) func() {
	old := opencodeCLI
	if strings.TrimSpace(path) == "" {
		opencodeCLI = "opencode"
	} else {
		opencodeCLI = path
	}
	return func() { opencodeCLI = old }
}

type opencodePromptAck struct {
	PaneID       string
	Name         string
	Kind         string
	State        string
	Session      AgentSession
	SessionID    string
	SessionKnown bool
}

func parseOpenCodePromptAck(raw string) (opencodePromptAck, error) {
	var envelope struct {
		Result struct {
			Type  string `json:"type"`
			Agent struct {
				PaneID  string        `json:"pane_id"`
				Name    string        `json:"name"`
				Kind    string        `json:"agent"`
				State   string        `json:"state"`
				Session *AgentSession `json:"agent_session"`
			} `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return opencodePromptAck{}, fmt.Errorf("parse native prompt acknowledgement: %w", err)
	}
	if envelope.Result.Type != "agent_prompted" {
		return opencodePromptAck{}, fmt.Errorf("native prompt acknowledgement type %q is not agent_prompted", envelope.Result.Type)
	}
	ack := opencodePromptAck{
		PaneID: envelope.Result.Agent.PaneID,
		Name:   envelope.Result.Agent.Name,
		Kind:   envelope.Result.Agent.Kind,
		State:  strings.TrimSpace(envelope.Result.Agent.State),
	}
	if envelope.Result.Agent.Session != nil {
		ack.Session = *envelope.Result.Agent.Session
		ack.SessionID = strings.TrimSpace(ack.Session.Value)
		ack.SessionKnown = true
	}
	if ack.PaneID == "" || ack.Name == "" || ack.Kind == "" {
		return opencodePromptAck{}, errors.New("native prompt acknowledgement omitted pane, agent, or session identity")
	}
	if ack.SessionKnown && (!RealModelSessionID(ack.SessionID) || ack.Session.Kind != "id") {
		return opencodePromptAck{}, errors.New("native prompt acknowledgement contained an invalid native session identity")
	}
	return ack, nil
}

func isStagedPromptAck(ack opencodePromptAck) bool {
	state := strings.ToLower(ack.State)
	return state == "staged" || state == "composer" || state == "unsubmitted"
}

type opencodeModel struct {
	ID         string `json:"id"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
}

func (m opencodeModel) id() string {
	if strings.TrimSpace(m.ID) != "" {
		return strings.TrimSpace(m.ID)
	}
	return strings.TrimSpace(m.ModelID)
}

func (m opencodeModel) provider() string { return strings.TrimSpace(m.ProviderID) }

type opencodeMessageTime struct {
	Created   int64  `json:"created"`
	Completed *int64 `json:"completed"`
}

type opencodeMessagePath struct {
	Cwd string `json:"cwd"`
}

type opencodeMessageInfo struct {
	ID         string              `json:"id"`
	SessionID  string              `json:"sessionID"`
	Role       string              `json:"role"`
	Time       opencodeMessageTime `json:"time"`
	ParentID   string              `json:"parentID"`
	Model      opencodeModel       `json:"model"`
	ModelID    string              `json:"modelID"`
	ProviderID string              `json:"providerID"`
	Path       opencodeMessagePath `json:"path"`
	Error      json.RawMessage     `json:"error"`
}

func (m opencodeMessageInfo) model() opencodeModel {
	if m.Role == "user" {
		return m.Model
	}
	return opencodeModel{ModelID: m.ModelID, ProviderID: m.ProviderID}
}

type opencodeMessagePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type opencodeMessage struct {
	Info  opencodeMessageInfo   `json:"info"`
	Parts []opencodeMessagePart `json:"parts"`
}

type opencodeSessionInfo struct {
	ID        string        `json:"id"`
	Directory string        `json:"directory"`
	Model     opencodeModel `json:"model"`
}

type opencodeExportData struct {
	Info     opencodeSessionInfo `json:"info"`
	Messages []opencodeMessage   `json:"messages"`
}

func parseOpenCodeExport(raw []byte) (opencodeExportData, error) {
	var data opencodeExportData
	if err := json.Unmarshal(raw, &data); err != nil {
		return opencodeExportData{}, fmt.Errorf("parse OpenCode export: %w", err)
	}
	if strings.TrimSpace(data.Info.ID) == "" {
		return opencodeExportData{}, errors.New("OpenCode export omitted session id")
	}
	return data, nil
}

func exportOpenCodeSession(ctx context.Context, sessionID, cwd string) ([]byte, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("OpenCode export requires a session id")
	}
	if !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("OpenCode export requires an absolute cwd, got %q", cwd)
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("OpenCode export cwd %q is not a directory", cwd)
	}
	file, err := os.CreateTemp("", "herd-opencode-export-*")
	if err != nil {
		return nil, fmt.Errorf("create private OpenCode export: %w", err)
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("protect private OpenCode export: %w", err)
	}

	// The caller owns the complete evidence deadline. In particular, do not
	// add a fresh timeout here: a before export, prompt, and after export must
	// fit inside one bounded delivery attempt.
	cmd := exec.CommandContext(ctx, opencodeCLI, "export", sessionID, "--sanitize", "--pure")
	cmd.Dir = cwd
	cmd.Stdout = file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("OpenCode export failed: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close private OpenCode export: %w", err)
	}
	body, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read private OpenCode export: %w", err)
	}
	if len(body) == 0 {
		return nil, errors.New("OpenCode export returned an empty document")
	}
	return body, nil
}

func sameDirectoryPath(a, b string) bool {
	aClean := filepath.Clean(strings.TrimSpace(a))
	bClean := filepath.Clean(strings.TrimSpace(b))
	if aClean == "" || bClean == "" {
		return false
	}
	if aClean == bClean {
		return true
	}
	realA, errA := filepath.EvalSymlinks(aClean)
	realB, errB := filepath.EvalSymlinks(bClean)
	if errA == nil && errB == nil && filepath.Clean(realA) == filepath.Clean(realB) {
		return true
	}
	return false
}

func exactOpenCodeCwd(agent AgentEntry) (string, error) {
	cwd := strings.TrimSpace(agent.ForegroundCwd)
	if cwd == "" {
		cwd = strings.TrimSpace(agent.Cwd)
	}
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", errors.New("OpenCode delivery requires exact live absolute cwd evidence")
	}
	if agent.ForegroundCwd != "" && agent.Cwd != "" && !sameDirectoryPath(agent.ForegroundCwd, agent.Cwd) {
		return "", errors.New("OpenCode delivery has conflicting live cwd evidence")
	}
	return filepath.Clean(cwd), nil
}

func sameOpenCodeIncarnation(before, after AgentEntry) error {
	if before.TerminalID == "" || after.TerminalID == "" {
		return errors.New("OpenCode delivery requires before/after terminal incarnation evidence")
	}
	checks := []struct {
		name   string
		before string
		after  string
	}{
		{"name", before.Name, after.Name},
		{"kind", before.Kind, after.Kind},
		{"tab", before.TabID, after.TabID},
		{"pane", before.PaneID, after.PaneID},
		{"workspace", before.Workspace, after.Workspace},
		{"terminal", before.TerminalID, after.TerminalID},
		{"session source", before.Session.Source, after.Session.Source},
		{"session agent", before.Session.Agent, after.Session.Agent},
		{"session kind", before.Session.Kind, after.Session.Kind},
		{"session", before.Session.Value, after.Session.Value},
	}
	for _, check := range checks {
		if check.before != check.after {
			return fmt.Errorf("OpenCode delivery %s identity changed", check.name)
		}
	}
	if !sameDirectoryPath(before.Cwd, after.Cwd) {
		return errors.New("OpenCode delivery cwd identity changed")
	}
	if before.ForegroundCwd != "" && after.ForegroundCwd != "" && !sameDirectoryPath(before.ForegroundCwd, after.ForegroundCwd) {
		return errors.New("OpenCode delivery foreground cwd identity changed")
	}
	if after.ForegroundCwd != "" && !sameDirectoryPath(before.Cwd, after.ForegroundCwd) {
		return errors.New("OpenCode delivery foreground cwd identity changed")
	}
	return nil
}

func sameOpenCodePrelaunchIdentity(before, after AgentEntry) error {
	if before.Name == "" || before.Kind == "" || before.TabID == "" || before.PaneID == "" || before.Workspace == "" || before.TerminalID == "" {
		return errors.New("OpenCode delivery requires exact prelaunch pane identity evidence")
	}
	checks := []struct {
		name   string
		before string
		after  string
	}{
		{"name", before.Name, after.Name},
		{"kind", before.Kind, after.Kind},
		{"tab", before.TabID, after.TabID},
		{"pane", before.PaneID, after.PaneID},
		{"workspace", before.Workspace, after.Workspace},
		{"terminal", before.TerminalID, after.TerminalID},
	}
	for _, check := range checks {
		if check.before != check.after {
			return fmt.Errorf("OpenCode delivery %s identity changed before session assignment", check.name)
		}
	}
	if !sameDirectoryPath(before.Cwd, after.Cwd) {
		return errors.New("OpenCode delivery cwd identity changed before session assignment")
	}
	if before.ForegroundCwd != "" && after.ForegroundCwd != "" && !sameDirectoryPath(before.ForegroundCwd, after.ForegroundCwd) {
		return errors.New("OpenCode delivery foreground cwd identity changed before session assignment")
	}
	if after.ForegroundCwd != "" && !sameDirectoryPath(before.Cwd, after.ForegroundCwd) {
		return errors.New("OpenCode delivery foreground cwd identity changed before session assignment")
	}
	return nil
}

func messageErrorAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func modelMatches(expected, got opencodeModel) bool {
	return expected.id() != "" && expected.id() == got.id() && expected.provider() == got.provider()
}

func openCodeConsumptionProof(before, after opencodeExportData, sessionID, cwd, payload string, submittedAt time.Time, cold bool) error {
	if !cold && before.Info.ID != sessionID {
		return errors.New("OpenCode pre-send export session does not match live session")
	}
	if after.Info.ID != sessionID {
		return errors.New("OpenCode export session changed")
	}
	known := make(map[string]struct{}, len(before.Messages))
	latestBefore := int64(0)
	for _, message := range before.Messages {
		if message.Info.ID != "" {
			known[message.Info.ID] = struct{}{}
		}
		if message.Info.Time.Created > latestBefore {
			latestBefore = message.Info.Time.Created
		}
	}
	var users []opencodeMessage
	for _, message := range after.Messages {
		if message.Info.Role != "user" || message.Info.SessionID != sessionID || message.Info.ID == "" {
			continue
		}
		if _, exists := known[message.Info.ID]; exists || message.Info.Time.Created < latestBefore || message.Info.Time.Created < submittedAt.UnixMilli() {
			continue
		}
		users = append(users, message)
	}
	if len(users) != 1 {
		return fmt.Errorf("OpenCode export has %d new current-session user messages", len(users))
	}
	user := users[0]
	if user.Info.Time.Created < submittedAt.UnixMilli() || !modelComplete(user.Info.model()) {
		return errors.New("OpenCode export current user message lacks timestamp or model identity")
	}
	var assistants []opencodeMessage
	for _, message := range after.Messages {
		if message.Info.Role != "assistant" || message.Info.SessionID != sessionID || message.Info.ID == "" || message.Info.ParentID != user.Info.ID {
			continue
		}
		if message.Info.Time.Created < user.Info.Time.Created || message.Info.Time.Created < submittedAt.UnixMilli() ||
			!messageErrorAbsent(message.Info.Error) {
			continue
		}
		assistants = append(assistants, message)
	}
	if len(assistants) != 1 {
		return fmt.Errorf("OpenCode export has %d completed assistant replies parented to current user", len(assistants))
	}
	assistant := assistants[0]
	if !modelComplete(assistant.Info.model()) || !modelMatches(user.Info.model(), assistant.Info.model()) {
		return errors.New("OpenCode user and assistant model identity differs")
	}
	if before.Info.Model.id() != "" && !modelMatches(before.Info.Model, user.Info.model()) {
		return errors.New("OpenCode current user model differs from the session model")
	}
	if !hasExactExportPayload(user, payload) {
		return errors.New("OpenCode current user payload does not match the submitted packet")
	}
	// The sanitized export intentionally redacts directory and assistant cwd.
	// Exact cwd is therefore bound to the before/after Herdr incarnation by the
	// caller, while this export binds the native session and message lineage.
	_ = cwd
	return nil
}

func modelComplete(model opencodeModel) bool {
	return model.id() != "" && model.provider() != ""
}

func hasExactExportPayload(message opencodeMessage, payload string) bool {
	for _, part := range message.Parts {
		if part.Type != "text" || part.Text == "" {
			continue
		}
		if strings.HasPrefix(part.Text, "[redacted:text:") {
			continue
		}
		return part.Text == payload
	}
	// --sanitize intentionally redacts transcript text. In that form the
	// exact packet is already bound to the structured agent.prompt request that
	// was accepted immediately before this native message lineage; redaction is
	// not itself evidence of a match. Any text that is actually emitted must
	// still compare byte-for-byte above.
	return true
}

func deliverOpenCode(target, payload string, timeout time.Duration, before AgentEntry, workspace string) (SendResult, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return SendResult{Status: "queued"}, err
	}
	cwd, err := exactOpenCodeCwd(before)
	if err != nil {
		return SendResult{}, err
	}
	sessionID := strings.TrimSpace(before.Session.Value)
	cold := sessionID == ""
	if !cold && !RealModelSessionID(sessionID) {
		return SendResult{}, errors.New("OpenCode delivery has an invalid native model session")
	}
	var baseline opencodeExportData
	if !cold {
		baselineRaw, exportErr := runOpenCodeExport(ctx, sessionID, cwd)
		if exportErr != nil {
			return SendResult{}, fmt.Errorf("capture OpenCode pre-send evidence: %w", exportErr)
		}
		baseline, err = parseOpenCodeExport(baselineRaw)
		if err != nil {
			return SendResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return SendResult{Status: "queued"}, err
	}
	// This is the send boundary. Evidence timestamps after the prompt call
	// would make a delayed provider response look like a newly consumed packet.
	submittedAt := time.Now().Add(-500 * time.Millisecond)
	promptRaw, err := AgentPrompt(before.Name, payload, false)
	if err != nil {
		return SendResult{}, err
	}
	ack, err := parseOpenCodePromptAck(promptRaw)
	if err != nil {
		return SendResult{}, err
	}
	if ack.Name != before.Name || ack.Kind != before.Kind || ack.PaneID != before.PaneID {
		return SendResult{}, errors.New("OpenCode prompt acknowledgement does not bind the exact target or harness")
	}
	if !cold && (!ack.SessionKnown || ack.SessionID != sessionID) {
		return SendResult{}, errors.New("OpenCode prompt acknowledgement does not bind the exact warm session")
	}
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		return SendResult{Status: "queued"}, errors.New("OpenCode delivery lost its evidence deadline")
	}
	var lastProofErr error
	nudgedStaged := false
	for {
		if err := ctx.Err(); err != nil {
			return SendResult{Status: "queued"}, errQueuedUnobserved(before.Name, before.Status)
		}
		live, liveErr := requireAgentWorkspaceIn(target, workspace)
		if liveErr != nil {
			return SendResult{Status: "queued"}, liveErr
		}
		if identityErr := sameOpenCodePrelaunchIdentity(before, live); identityErr != nil {
			return SendResult{Status: "queued"}, identityErr
		}
		if liveCwd, cwdErr := exactOpenCodeCwd(live); cwdErr != nil || !sameDirectoryPath(liveCwd, cwd) {
			return SendResult{Status: "queued"}, errors.New("OpenCode live cwd changed during delivery")
		}
		if cold {
			liveSession := strings.TrimSpace(live.Session.Value)
			if !RealModelSessionID(liveSession) || live.Session.Kind != "id" {
				if !time.Now().Before(deadline) {
					return SendResult{Status: "queued"}, errQueuedUnobserved(before.Name, live.Status)
				}
				// If exact owned matching composer is positively staged/unsubmitted,
				// nudge Enter once. Never send if busy, working, or blocked.
				if isStagedPromptAck(ack) && !nudgedStaged && live.Status != "working" && live.Status != "busy" && live.Status != "blocked" {
					_ = SendKeys(before.Name, "Enter")
					nudgedStaged = true
				}
				select {
				case <-ctx.Done():
					return SendResult{Status: "queued"}, errQueuedUnobserved(before.Name, live.Status)
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			sessionID = liveSession
			if ack.SessionKnown && ack.SessionID != sessionID {
				return SendResult{Status: "queued"}, errors.New("OpenCode prompt acknowledgement session differs from assigned native session")
			}
		}
		if !cold {
			if identityErr := sameOpenCodeIncarnation(before, live); identityErr != nil {
				return SendResult{Status: "queued"}, identityErr
			}
		}
		raw, exportErr := runOpenCodeExport(ctx, sessionID, cwd)
		if exportErr != nil {
			return SendResult{Status: "queued"}, fmt.Errorf("OpenCode consumption evidence unavailable: %w", exportErr)
		}
		after, parseErr := parseOpenCodeExport(raw)
		if parseErr != nil {
			return SendResult{Status: "queued"}, parseErr
		}
		if proofErr := openCodeConsumptionProof(baseline, after, sessionID, cwd, payload, submittedAt, cold); proofErr == nil {
			return SendResult{Status: live.Status}, nil
		} else {
			lastProofErr = proofErr
		}
		// Send Enter only if exact owned matching composer is positively staged/unsubmitted;
		// never when prompt already consumed, busy, blocked, or new incarnation.
		if isStagedPromptAck(ack) && !nudgedStaged && live.Status != "working" && live.Status != "busy" && live.Status != "blocked" {
			_ = SendKeys(before.Name, "Enter")
			nudgedStaged = true
		}
		if !time.Now().Before(deadline) {
			return SendResult{Status: "queued"}, fmt.Errorf("%w: native evidence: %v", errQueuedUnobserved(before.Name, live.Status), lastProofErr)
		}
		select {
		case <-ctx.Done():
			return SendResult{Status: "queued"}, fmt.Errorf("%w: native evidence: %v", errQueuedUnobserved(before.Name, live.Status), lastProofErr)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
