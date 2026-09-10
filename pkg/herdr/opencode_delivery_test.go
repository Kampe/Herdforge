package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	nativeOpenCodeTarget  = "reviewer"
	nativeOpenCodeSession = "ses_native"
	nativeOpenCodeCwd     = "/repo"
	nativeOpenCodePacket  = "PACKET"
)

type openCodeSendFixture struct {
	listCalls     int
	exportCalls   int
	mode          string
	customCwd     string
	customFgCwd   func(calls int) string
	promptCalls   int
	keys          int
	read          int
	baseline      []byte
	promptPayload string
}

func (f *openCodeSendFixture) run(args ...string) (string, error) {
	if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
		f.listCalls++
		terminalID, cwd := "term-1", nativeOpenCodeCwd
		if f.customCwd != "" {
			cwd = f.customCwd
		}
		fgCwd := cwd
		if f.customFgCwd != nil {
			fgCwd = f.customFgCwd(f.listCalls)
		} else if f.mode == "cold-transient-fg" && f.listCalls == 1 {
			fgCwd = ""
		}
		if (f.mode == "reused-pane" || f.mode == "cold-reused-pane") && f.listCalls > 1 {
			terminalID = "term-2"
		}
		if (f.mode == "wrong-cwd" || f.mode == "cold-wrong-cwd") && f.listCalls > 1 {
			cwd = "/other-repo"
			fgCwd = "/other-repo"
		}
		if f.mode == "cold-wrong-fg-after" && f.listCalls > 1 {
			fgCwd = "/other-repo"
		}
		session := ""
		if !strings.HasPrefix(f.mode, "cold") || (f.mode != "cold-never-session" && f.listCalls > 1) {
			session = nativeOpenCodeSession
			if strings.HasPrefix(f.mode, "cold") {
				session = "ses_cold"
			}
		}
		sessionJSON := ""
		if session != "" {
			sessionJSON = fmt.Sprintf(`,"agent_session":{"source":"herdr:opencode","agent":"opencode","kind":"id","value":%q}`, session)
		}
		return fmt.Sprintf(`{"result":{"type":"agents","agents":[{"name":%q,"agent":"opencode","agent_status":"idle","tab_id":"wK:t1","pane_id":"wK:p1","workspace_id":"wK","terminal_id":%q,"cwd":%q,"foreground_cwd":%q,"revision":1,"state_change_seq":%d%s}]}}`,
			nativeOpenCodeTarget, terminalID, cwd, fgCwd, f.listCalls, sessionJSON), nil
	}
	if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
		f.promptCalls++
		f.promptPayload = args[3]
		if f.mode == "provider-error" {
			return "provider unavailable", errors.New("provider unavailable")
		}
		if f.mode == "cold-missing-ack" {
			return `{"result":{"type":"agent_prompted","agent":{"name":"reviewer"}}}`, nil
		}
		ackSession := ""
		if f.mode == "cold-wrong-ack-session" {
			ackSession = `,"agent_session":{"source":"herdr:opencode","agent":"opencode","kind":"id","value":"ses_other"}`
		} else if !strings.HasPrefix(f.mode, "cold") {
			ackSession = fmt.Sprintf(`,"agent_session":{"source":"herdr:opencode","agent":"opencode","kind":"id","value":%q}`, nativeOpenCodeSession)
		}
		return fmt.Sprintf(`{"result":{"type":"agent_prompted","agent":{"name":%q,"agent":"opencode","pane_id":"wK:p1"%s,"state":"working"}}}`,
			nativeOpenCodeTarget, ackSession), nil
	}
	if len(args) >= 2 && args[0] == "agent" && args[1] == "send-keys" {
		f.keys++
		return `{"result":{"type":"ok"}}`, nil
	}
	if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
		f.read++
		return `{"result":{"type":"pane","content":"decorative pane text"}}`, nil
	}
	return `{"result":{"type":"ok"}}`, nil
}

func (f *openCodeSendFixture) export(_ context.Context, sessionID, cwd string) ([]byte, error) {
	expectedSession := nativeOpenCodeSession
	if strings.HasPrefix(f.mode, "cold") {
		expectedSession = "ses_cold"
	}
	if sessionID != expectedSession {
		return nil, fmt.Errorf("unexpected export session %q", sessionID)
	}
	expectedCwd := nativeOpenCodeCwd
	if f.customCwd != "" {
		expectedCwd = f.customCwd
	}
	if !sameDirectoryPath(cwd, expectedCwd) {
		return nil, fmt.Errorf("unexpected export cwd %q (want %q)", cwd, expectedCwd)
	}
	f.exportCalls++
	if strings.HasPrefix(f.mode, "cold-") {
		return nativeOpenCodeCurrentExport(f.mode), nil
	}
	if f.exportCalls == 1 || f.mode == "timeout" || f.mode == "unchanged" || f.mode == "stale" {
		return f.baseline, nil
	}
	// Build the post-submit document at the export boundary so its native
	// created timestamp is necessarily after the Send prompt boundary.
	return nativeOpenCodeCurrentExport(f.mode), nil
}

func newOpenCodeSendFixture(t *testing.T, mode string) (*openCodeSendFixture, func()) {
	t.Helper()
	f := &openCodeSendFixture{mode: mode}
	f.baseline = nativeOpenCodeExport(false, "", "")
	restoreHerdr := SetRunHerdrForTest(f.run)
	restoreExport := SetOpenCodeExportForTest(func(_ context.Context, sessionID, cwd string) ([]byte, error) {
		return f.export(nil, sessionID, cwd)
	})
	return f, func() {
		restoreExport()
		restoreHerdr()
	}
}

func nativeOpenCodeExport(consumed bool, sessionID, parentID string, values ...string) []byte {
	if sessionID == "" {
		sessionID = nativeOpenCodeSession
	}
	providerID := "openai"
	if len(values) > 0 {
		providerID = values[0]
	}
	packetText := "[redacted:text:user-current]"
	if len(values) > 1 {
		packetText = values[1]
	}
	oldCreated := time.Now().Add(-2 * time.Second).UnixMilli()
	messages := []map[string]interface{}{
		{"info": map[string]interface{}{
			"id": "user-old", "sessionID": sessionID, "role": "user",
			"time":  map[string]interface{}{"created": oldCreated},
			"model": map[string]interface{}{"providerID": providerID, "modelID": "gpt-5.6-luna"},
		}, "parts": []map[string]string{{"type": "text", "text": "old"}}},
		{"info": map[string]interface{}{
			"id": "assistant-old", "sessionID": sessionID, "role": "assistant", "parentID": "user-old",
			"time":    map[string]interface{}{"created": oldCreated + 100, "completed": oldCreated + 200},
			"modelID": "gpt-5.6-luna", "providerID": providerID,
		}, "parts": []map[string]string{{"type": "text", "text": "old reply"}}},
	}
	if consumed {
		created := time.Now().UnixMilli()
		if parentID == "" {
			parentID = "user-current"
		}
		messages = append(messages,
			map[string]interface{}{"info": map[string]interface{}{
				"id": "user-current", "sessionID": sessionID, "role": "user",
				"time":  map[string]interface{}{"created": created},
				"model": map[string]interface{}{"providerID": providerID, "modelID": "gpt-5.6-luna"},
			}, "parts": []map[string]string{{"type": "text", "text": packetText}}},
			map[string]interface{}{"info": map[string]interface{}{
				"id": "assistant-current", "sessionID": sessionID, "role": "assistant", "parentID": parentID,
				"time":    map[string]interface{}{"created": created + 100, "completed": created + 200},
				"modelID": "gpt-5.6-luna", "providerID": providerID,
			}, "parts": []map[string]string{{"type": "text", "text": "[redacted:text:assistant-current]"}}},
		)
	}
	infoID := nativeOpenCodeSession
	if sessionID != nativeOpenCodeSession {
		infoID = sessionID
	}
	raw, err := json.Marshal(map[string]interface{}{
		"info": map[string]interface{}{
			"id":    infoID,
			"model": map[string]interface{}{"providerID": "openai", "modelID": "gpt-5.6-luna"},
		},
		"messages": messages,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func nativeOpenCodeCurrentExport(mode string) []byte {
	if mode == "cold-reused-session" || mode == "cold-old-user" {
		return nativeOpenCodeExport(false, "ses_cold", "")
	}
	if mode == "cold-no-assistant" {
		raw := nativeOpenCodeExport(true, "ses_cold", "")
		var data map[string]interface{}
		if err := json.Unmarshal(raw, &data); err != nil {
			panic(err)
		}
		messages := data["messages"].([]interface{})
		data["messages"] = messages[:len(messages)-1]
		body, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		return body
	}
	if mode == "cold-incomplete" {
		raw := nativeOpenCodeExport(true, "ses_cold", "")
		var data map[string]interface{}
		if err := json.Unmarshal(raw, &data); err != nil {
			panic(err)
		}
		messages := data["messages"].([]interface{})
		assistantInfo := messages[len(messages)-1].(map[string]interface{})["info"].(map[string]interface{})
		delete(assistantInfo["time"].(map[string]interface{}), "completed")
		body, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		return body
	}
	if mode == "cold-missing-bound-user" {
		raw := nativeOpenCodeExport(true, "ses_cold", "")
		var data map[string]interface{}
		if err := json.Unmarshal(raw, &data); err != nil {
			panic(err)
		}
		messages := data["messages"].([]interface{})
		userInfo := messages[len(messages)-2].(map[string]interface{})["info"].(map[string]interface{})
		userInfo["sessionID"] = ""
		body, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		return body
	}
	if mode == "cold-queued-composer" {
		return nativeOpenCodeExport(false, "ses_cold", "")
	}
	switch mode {
	case "wrong-session":
		return nativeOpenCodeExport(true, "ses_other", "")
	case "wrong-payload":
		return nativeOpenCodeExport(true, "", "", "openai", "OTHER PACKET")
	case "wrong-parent":
		return nativeOpenCodeExport(true, "", "wrong-user")
	case "wrong-model":
		return nativeOpenCodeExport(true, "", "", "anthropic")
	default:
		if strings.HasPrefix(mode, "cold") {
			return nativeOpenCodeExport(true, "ses_cold", "")
		}
		return nativeOpenCodeExport(true, "", "")
	}
}

func TestPublicSendOpenCodeProvesNativeConsumption(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	f, restore := newOpenCodeSendFixture(t, "")
	defer restore()

	status, err := Send(nativeOpenCodeTarget, nativeOpenCodePacket, true, time.Second)
	if err != nil {
		t.Fatalf("native consumed send failed: %v", err)
	}
	if status != "idle" {
		t.Fatalf("status = %q, want the live native status idle", status)
	}
	if f.promptCalls != 1 || f.promptPayload != nativeOpenCodePacket {
		t.Fatalf("prompt calls/payload = %d/%q, want one exact packet", f.promptCalls, f.promptPayload)
	}
	if f.exportCalls < 2 {
		t.Fatalf("export calls = %d, want before and after evidence", f.exportCalls)
	}
	if f.keys != 1 || f.read != 0 {
		t.Fatalf("native proof used unsafe/decorative pane operations: keys=%d reads=%d", f.keys, f.read)
	}
}

func TestPublicSendOpenCodeProvesColdSessionConsumption(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	f, restore := newOpenCodeSendFixture(t, "cold-session")
	defer restore()

	status, err := Send(nativeOpenCodeTarget, nativeOpenCodePacket, true, time.Second)
	if err != nil {
		t.Fatalf("cold native consumed send failed: %v", err)
	}
	if status != "idle" {
		t.Fatalf("status = %q, want idle", status)
	}
	if f.promptCalls != 1 || f.exportCalls == 0 || f.keys != 1 || f.read != 0 {
		t.Fatalf("cold delivery calls = prompts:%d exports:%d keys:%d reads:%d", f.promptCalls, f.exportCalls, f.keys, f.read)
	}
}

func TestPublicSendOpenCodeProvesColdSessionTransientForegroundCwd(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	f, restore := newOpenCodeSendFixture(t, "cold-transient-fg")
	defer restore()

	status, err := SendInWorkspace(nativeOpenCodeTarget, nativeOpenCodePacket, true, time.Second, "wK")
	if err != nil {
		t.Fatalf("cold transient fg cwd send failed: %v", err)
	}
	if status != "idle" {
		t.Fatalf("status = %q, want idle", status)
	}
	if f.promptCalls != 1 || f.exportCalls == 0 {
		t.Fatalf("cold delivery calls = prompts:%d exports:%d", f.promptCalls, f.exportCalls)
	}
}

func TestPublicSendOpenCodeProvesSymlinkAliasCwd(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	realDir := t.TempDir()
	aliasDir := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlinks unsupported in test environment: %v", err)
	}

	f, restore := newOpenCodeSendFixture(t, "cold-session")
	defer restore()
	f.customCwd = aliasDir
	f.customFgCwd = func(calls int) string {
		if calls == 1 {
			return aliasDir
		}
		return realDir
	}

	status, err := SendInWorkspace(nativeOpenCodeTarget, nativeOpenCodePacket, true, time.Second, "wK")
	if err != nil {
		t.Fatalf("symlink alias cwd delivery failed: %v", err)
	}
	if status != "idle" {
		t.Fatalf("status = %q, want idle", status)
	}
}

func TestPublicSendOpenCodeColdStartedAssistantDoesNotNeedCompletion(t *testing.T) {
	f, restore := newOpenCodeSendFixture(t, "cold-incomplete")
	defer restore()

	status, err := SendInWorkspace(nativeOpenCodeTarget, nativeOpenCodePacket, true, time.Second, "wK")
	if err != nil {
		t.Fatalf("started incomplete assistant must prove consumption: %v", err)
	}
	if status != "idle" || f.promptCalls != 1 {
		t.Fatalf("status/prompts = %q/%d, want idle/1", status, f.promptCalls)
	}
}

func TestExportOpenCodeSessionPreservesCompletePrivateJSON(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"info":{"id":"ses_native"},"padding":"` + strings.Repeat("x", 70*1024) + `"}`)
	payloadFile := filepath.Join(dir, "export.json")
	if err := os.WriteFile(payloadFile, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\ncat " + shellQuote(payloadFile) + "\n"
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	restore := SetOpenCodeExecutableForTest(cli)
	defer restore()

	got, err := exportOpenCodeSession(context.Background(), nativeOpenCodeSession, dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("export length = %d, want complete regular-file output length %d", len(got), len(payload))
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestPublicSendOpenCodeFailsClosedForNativeEvidenceGaps(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		want       string
		wantStatus string
	}{
		{name: "staged-or-unchanged", mode: "unchanged", want: "queued-but-not-consumed", wantStatus: "queued"},
		{name: "stale", mode: "stale", want: "queued-but-not-consumed", wantStatus: "queued"},
		{name: "wrong-payload", mode: "wrong-payload", want: "queued-but-not-consumed", wantStatus: "queued"},
		{name: "wrong-session", mode: "wrong-session", want: "queued-but-not-consumed", wantStatus: "queued"},
		{name: "reused-pane", mode: "reused-pane", want: "identity changed", wantStatus: "queued"},
		{name: "wrong-cwd", mode: "wrong-cwd", want: "identity changed", wantStatus: "queued"},
		{name: "wrong-parent", mode: "wrong-parent", want: "queued-but-not-consumed", wantStatus: "queued"},
		{name: "wrong-model", mode: "wrong-model", want: "queued-but-not-consumed", wantStatus: "queued"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, restore := newOpenCodeSendFixture(t, tc.mode)
			defer restore()
			status, err := SendInWorkspace(nativeOpenCodeTarget, nativeOpenCodePacket, true, 350*time.Millisecond, "wK")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("status/error = %q/%v, want error containing %q", status, err, tc.want)
			}
			if status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", status, tc.wantStatus)
			}
			if f.promptCalls != 1 {
				t.Fatalf("prompt calls = %d, want exactly one", f.promptCalls)
			}
			if f.keys != 1 || f.read != 0 {
				t.Fatalf("failure used duplicate/decorative pane operations: keys=%d reads=%d", f.keys, f.read)
			}
		})
	}
}

func TestPublicSendOpenCodeColdSessionFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want string
	}{
		{name: "missing-ack", mode: "cold-missing-ack", want: "pane"},
		{name: "session-never-assigned", mode: "cold-never-session", want: "queued-but-not-consumed"},
		{name: "reused-session-old-transcript", mode: "cold-reused-session", want: "queued-but-not-consumed"},
		{name: "old-user-only", mode: "cold-old-user", want: "queued-but-not-consumed"},
		{name: "no-assistant", mode: "cold-no-assistant", want: "queued-but-not-consumed"},
		{name: "missing-native-bound-user", mode: "cold-missing-bound-user", want: "queued-but-not-consumed"},
		{name: "queued-composer", mode: "cold-queued-composer", want: "queued-but-not-consumed"},
		{name: "conflicting-pane-incarnation", mode: "cold-reused-pane", want: "identity changed"},
		{name: "conflicting-cwd", mode: "cold-wrong-cwd", want: "identity changed"},
		{name: "conflicting-fg-cwd", mode: "cold-wrong-fg-after", want: "identity changed"},
		{name: "ack-session-disagrees", mode: "cold-wrong-ack-session", want: "differs from assigned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, restore := newOpenCodeSendFixture(t, tc.mode)
			defer restore()
			status, err := SendInWorkspace(nativeOpenCodeTarget, nativeOpenCodePacket, true, 350*time.Millisecond, "wK")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("status/error = %q/%v, want error containing %q", status, err, tc.want)
			}
			if tc.mode != "cold-missing-ack" && status != "queued" {
				t.Fatalf("status = %q, want queued", status)
			}
			if f.promptCalls != 1 {
				t.Fatalf("prompt calls = %d, want exactly one", f.promptCalls)
			}
			expectedKeys := 1
			if tc.mode == "cold-missing-ack" {
				expectedKeys = 0
			}
			if f.keys != expectedKeys || f.read != 0 {
				t.Fatalf("cold failure used duplicate/decorative pane operations: keys=%d (want %d) reads=%d", f.keys, expectedKeys, f.read)
			}
		})
	}
}

func TestPublicSendOpenCodeProviderErrorAndTimeoutAreBounded(t *testing.T) {
	t.Run("provider-error", func(t *testing.T) {
		f, restore := newOpenCodeSendFixture(t, "provider-error")
		defer restore()
		status, err := SendInWorkspace(nativeOpenCodeTarget, nativeOpenCodePacket, true, time.Second, "wK")
		if err == nil || !strings.Contains(err.Error(), "provider unavailable") {
			t.Fatalf("status/error = %q/%v, want provider error", status, err)
		}
		if f.exportCalls != 1 || f.promptCalls != 1 {
			t.Fatalf("provider failure performed duplicate work: exports=%d prompts=%d", f.exportCalls, f.promptCalls)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		f, restore := newOpenCodeSendFixture(t, "timeout")
		defer restore()
		started := time.Now()
		status, err := SendInWorkspace(nativeOpenCodeTarget, nativeOpenCodePacket, true, 250*time.Millisecond, "wK")
		if err == nil || status != "queued" {
			t.Fatalf("status/error = %q/%v, want bounded queued failure", status, err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("delivery exceeded one caller deadline by too much: %s", elapsed)
		}
		if f.promptCalls != 1 {
			t.Fatalf("timeout retried prompt %d times", f.promptCalls)
		}
	})
}

func TestPublicSendOpenCodeSubmitsEnterImmediatelyAfterPromptAck(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	f, restore := newOpenCodeSendFixture(t, "cold-session")
	defer restore()

	var order []string
	restoreRun := SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			order = append(order, "prompt")
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "send-keys" {
			order = append(order, fmt.Sprintf("send-keys:%s", args[len(args)-1]))
		}
		return f.run(args...)
	})
	defer restoreRun()

	status, err := SendInWorkspace(nativeOpenCodeTarget, nativeOpenCodePacket, true, time.Second, "wK")
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if status != "idle" {
		t.Fatalf("status = %q, want idle", status)
	}
	if len(order) < 2 || order[0] != "prompt" || order[1] != "send-keys:Enter" {
		t.Fatalf("transport order = %v, want [prompt, send-keys:Enter]", order)
	}
}
