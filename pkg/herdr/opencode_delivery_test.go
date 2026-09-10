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
		if f.mode == "reused-pane" && f.listCalls > 1 {
			terminalID = "term-2"
		}
		if f.mode == "wrong-cwd" && f.listCalls > 1 {
			cwd = "/other-repo"
		}
		return fmt.Sprintf(`{"result":{"type":"agents","agents":[{"name":%q,"agent":"opencode","agent_status":"idle","tab_id":"wK:t1","pane_id":"wK:p1","workspace_id":"wK","terminal_id":%q,"cwd":%q,"foreground_cwd":%q,"agent_session":{"value":%q}}]}}`,
			nativeOpenCodeTarget, terminalID, cwd, cwd, nativeOpenCodeSession), nil
	}
	if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
		f.promptCalls++
		f.promptPayload = args[3]
		if f.mode == "provider-error" {
			return "provider unavailable", errors.New("provider unavailable")
		}
		return fmt.Sprintf(`{"result":{"type":"agent_prompted","agent":{"pane_id":"wK:p1","agent":%q,"agent_session":{"source":"herdr","agent":%q,"kind":"id","value":%q},"state":"working"}}}`,
			nativeOpenCodeTarget, nativeOpenCodeTarget, nativeOpenCodeSession), nil
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
	if sessionID != nativeOpenCodeSession {
		return nil, fmt.Errorf("unexpected export session %q", sessionID)
	}
	if cwd != nativeOpenCodeCwd {
		return nil, fmt.Errorf("unexpected export cwd %q", cwd)
	}
	f.exportCalls++
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
			"id": "user-old", "sessionID": nativeOpenCodeSession, "role": "user",
			"time":  map[string]interface{}{"created": oldCreated},
			"model": map[string]interface{}{"providerID": providerID, "modelID": "gpt-5.6-luna"},
		}, "parts": []map[string]string{{"type": "text", "text": "old"}}},
		{"info": map[string]interface{}{
			"id": "assistant-old", "sessionID": nativeOpenCodeSession, "role": "assistant", "parentID": "user-old",
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
				"id": "user-current", "sessionID": nativeOpenCodeSession, "role": "user",
				"time":  map[string]interface{}{"created": created},
				"model": map[string]interface{}{"providerID": providerID, "modelID": "gpt-5.6-luna"},
			}, "parts": []map[string]string{{"type": "text", "text": packetText}}},
			map[string]interface{}{"info": map[string]interface{}{
				"id": "assistant-current", "sessionID": nativeOpenCodeSession, "role": "assistant", "parentID": parentID,
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
	if f.keys != 0 || f.read != 0 {
		t.Fatalf("native proof used unsafe/decorative pane operations: keys=%d reads=%d", f.keys, f.read)
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
			if f.keys != 0 || f.read != 0 {
				t.Fatalf("failure used duplicate/decorative pane operations: keys=%d reads=%d", f.keys, f.read)
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
