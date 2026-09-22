package herdr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	agySessionSource = "herdr:antigravity_cli"
	agySessionAgent  = "agy"
)

// AgyWorkspaceConversationID returns the Antigravity conversation UUID that
// AGY itself recorded for this workspace cwd. That UUID is the same identity
// --conversation would resume. Missing files, a missing conversation db, or a
// provisional id are unavailable sessions, never a pane/terminal fallback.
func AgyWorkspaceConversationID(home, cwd string) (string, error) {
	home = strings.TrimSpace(home)
	cwd = strings.TrimSpace(cwd)
	if home == "" || cwd == "" {
		return "", fmt.Errorf("agy workspace conversation requires home and cwd")
	}
	body, err := os.ReadFile(filepath.Join(home, ".gemini", "antigravity-cli", "cache", "last_conversations.json"))
	if err != nil {
		return "", fmt.Errorf("agy workspace conversation index: %w", err)
	}
	var index map[string]string
	if err := json.Unmarshal(body, &index); err != nil || index == nil {
		return "", fmt.Errorf("agy workspace conversation index is invalid")
	}
	want := canonicalPath(cwd)
	id := ""
	for recorded, value := range index {
		if canonicalPath(recorded) == want {
			id = strings.TrimSpace(value)
			break
		}
	}
	if !RealModelSessionID(id) {
		return "", fmt.Errorf("agy workspace conversation is not a real model session")
	}
	if _, err := os.Stat(agyConversationDB(home, id)); err != nil {
		return "", fmt.Errorf("agy conversation db for %s: %w", id, err)
	}
	return id, nil
}

func agyConversationDB(home, id string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "conversations", id+".db")
}

func agyConversationFreshness(home, id string, notBefore time.Time) error {
	if notBefore.IsZero() {
		return nil
	}
	db := agyConversationDB(home, id)
	info, err := os.Stat(db)
	if err != nil {
		return fmt.Errorf("agy conversation db for %s: %w", id, err)
	}
	mtime := info.ModTime()
	if wal, err := os.Stat(db + "-wal"); err == nil && wal.ModTime().After(mtime) {
		mtime = wal.ModTime()
	}
	if mtime.Before(notBefore) {
		return fmt.Errorf("agy workspace conversation is older than this launch")
	}
	return nil
}

// SelectAgyLaunchConversation returns the workspace conversation created by
// this launch. A reused pool slot that still maps to the previous conversation
// is stale, even if the db still exists.
func SelectAgyLaunchConversation(home, cwd, priorID string, notBefore time.Time) (string, error) {
	id, err := AgyWorkspaceConversationID(home, cwd)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(priorID) != "" && id == strings.TrimSpace(priorID) {
		return "", fmt.Errorf("agy workspace conversation is the previous launch mapping")
	}
	if err := agyConversationFreshness(home, id, notBefore); err != nil {
		return "", err
	}
	return id, nil
}

func canonicalPath(path string) string {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved
	}
	return cleaned
}

// ReportPaneAgentSession publishes one official herdr session identity for a
// pane. The id must already be a real model session; this does not invent one.
func ReportPaneAgentSession(paneID, source, agent, sessionID string) error {
	paneID = strings.TrimSpace(paneID)
	source = strings.TrimSpace(source)
	agent = strings.TrimSpace(agent)
	sessionID = strings.TrimSpace(sessionID)
	if paneID == "" || source == "" || agent == "" || !RealModelSessionID(sessionID) {
		return fmt.Errorf("pane session report requires pane, source, agent, and a real model session")
	}
	_, err := runHerdr("pane", "report-agent-session", paneID, "--source", source, "--agent", agent, "--agent-session-id", sessionID)
	return err
}

// BindAgyWorkspaceSession reports AGY's own workspace conversation through
// herdr's session API, then re-reads the live agent. priorID is the mapping
// present before this launch; notBefore is the launch clock. A reused slot
// that still names the previous conversation is stale. It never uses pane,
// terminal, timestamp, or revision as a session id.
func BindAgyWorkspaceSession(agent AgentEntry, priorID string, notBefore time.Time) (*AgentEntry, error) {
	if RealModelSessionID(agent.Session.Value) {
		return &agent, nil
	}
	if !strings.EqualFold(strings.TrimSpace(agent.Kind), agySessionAgent) {
		return nil, fmt.Errorf("agent session remains unavailable after delivery")
	}
	cwd := strings.TrimSpace(agent.ForegroundCwd)
	if cwd == "" {
		cwd = strings.TrimSpace(agent.Cwd)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("agy workspace conversation home: %w", err)
	}
	id, err := SelectAgyLaunchConversation(home, cwd, priorID, notBefore)
	if err != nil {
		return nil, fmt.Errorf("agent session remains unavailable after delivery: %w", err)
	}
	if err := ReportPaneAgentSession(agent.PaneID, agySessionSource, agySessionAgent, id); err != nil {
		return nil, fmt.Errorf("report agy workspace conversation: %w", err)
	}
	live, err := LookupAgent(agent.Name)
	if err != nil {
		return nil, err
	}
	if live.PaneID != agent.PaneID || live.TabID != agent.TabID || live.TerminalID != agent.TerminalID || live.Workspace != agent.Workspace {
		return nil, fmt.Errorf("authoritative reviewer identity changed: name=%q tab=%q pane=%q terminal=%q workspace=%q", live.Name, live.TabID, live.PaneID, live.TerminalID, live.Workspace)
	}
	if live.Session.Value != id || !RealModelSessionID(live.Session.Value) {
		return nil, fmt.Errorf("agent session remains unavailable after delivery")
	}
	return live, nil
}
