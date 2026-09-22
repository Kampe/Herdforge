package herdr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAgyConversationIndex(t *testing.T, home, cwd, id string, withDB bool) {
	t.Helper()
	cache := filepath.Join(home, ".gemini", "antigravity-cli", "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	index := map[string]string{cwd: id}
	body, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "last_conversations.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if !withDB {
		return
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".db"), []byte("agy"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgyWorkspaceConversationIDUsesOfficialIndexAndDB(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	id := "a5866738-16eb-4815-b811-b090b16dc0ee"
	writeAgyConversationIndex(t, home, cwd, id, true)
	got, err := AgyWorkspaceConversationID(home, cwd)
	if err != nil || got != id {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestAgyWorkspaceConversationIDRefusesMissingDBAndProvisionalIDs(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	if _, err := AgyWorkspaceConversationID(home, cwd); err == nil {
		t.Fatal("missing index was treated as a session")
	}
	writeAgyConversationIndex(t, home, cwd, "pending-agy", true)
	if _, err := AgyWorkspaceConversationID(home, cwd); err == nil {
		t.Fatal("provisional id was accepted")
	}
	real := "a5866738-16eb-4815-b811-b090b16dc0ee"
	writeAgyConversationIndex(t, home, cwd, real, false)
	if _, err := AgyWorkspaceConversationID(home, cwd); err == nil {
		t.Fatal("index without conversation db was accepted")
	}
}

func TestBindAgyWorkspaceSessionReportsOfficialConversation(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	id := "a5866738-16eb-4815-b811-b090b16dc0ee"
	writeAgyConversationIndex(t, home, cwd, id, true)
	t.Setenv("HOME", home)
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	logPath := filepath.Join(dir, "calls")
	listed, err := json.Marshal(map[string]any{
		"result": map[string]any{
			"agents": []map[string]any{{
				"name":          "review-agy",
				"agent":         "agy",
				"agent_status":  "done",
				"workspace_id":  "wK",
				"tab_id":        "t1",
				"pane_id":       "p1",
				"terminal_id":   "term1",
				"cwd":           cwd,
				"agent_session": map[string]string{"source": agySessionSource, "agent": "agy", "kind": "id", "value": id},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> \"$FAC650_HERDR_LOG\"\ncase \" $* \" in\n  *\" pane report-agent-session \"*) printf '%%s\\n' '{\"result\":{\"ok\":true}}' ;;\n  *) printf '%%s\\n' '%s' ;;\nesac\n", string(listed))
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BinaryEnv, bin)
	t.Setenv(NoLiveEnv, "1")
	t.Setenv("FAC650_HERDR_LOG", logPath)
	t.Setenv("FAC650_AGY_CWD", cwd)
	agent := AgentEntry{Name: "review-agy", Kind: "agy", Status: "done", Workspace: "wK", TabID: "t1", PaneID: "p1", TerminalID: "term1", Cwd: cwd}
	got, err := BindAgyWorkspaceSession(agent)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got.Session.Value != id || got.Session.Source != agySessionSource {
		t.Fatalf("bound session = %+v", got.Session)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := string(logged)
	if !strings.Contains(line, "pane report-agent-session") || !strings.Contains(line, id) || !strings.Contains(line, "herdr:antigravity_cli") {
		t.Fatalf("did not report official session: %s", line)
	}
	if strings.Contains(line, "herdr-pane:") || strings.Contains(line, "herdr-term:") {
		t.Fatal("reported a pane/terminal fallback")
	}
}

func TestBindAgyWorkspaceSessionDoesNotInventFromPaneIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '{\"result\":{\"agents\":[]}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BinaryEnv, bin)
	t.Setenv(NoLiveEnv, "1")
	agent := AgentEntry{Name: "review-agy", Kind: "agy", PaneID: "wK:p1E7", TabID: "wK:t1E7", TerminalID: "term_x", Cwd: t.TempDir()}
	if _, err := BindAgyWorkspaceSession(agent); err == nil {
		t.Fatal("empty workspace conversation invented a session")
	}
}
