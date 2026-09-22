package herdr

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAgyLogPinnedModelReadyIgnoresStaleAndFailedOverride(t *testing.T) {
	pinned := "gemini-3.1-pro-high"
	stale := "Resolving model other-model\nPropagating selected model override to backend: label=\"Other\"\n"
	if agyLogPinnedModelReady(stale, pinned) {
		t.Fatal("stale other-model propagate must not satisfy the pin")
	}
	failed := "Resolving model gemini-3.1-pro-high\nfailed to apply model override: model gemini-3.1-pro-high is not recognized as a known model or custom model in settings\n"
	if agyLogPinnedModelReady(failed, pinned) {
		t.Fatal("failed override without later propagate must not be ready")
	}
	ready := failed + "Resolving model gemini-3.1-pro-high\nPropagating selected model override to backend: label=\"Gemini 3.1 Pro (High)\"\n"
	if !agyLogPinnedModelReady(ready, pinned) {
		t.Fatal("propagate after auth must be ready for the pinned model")
	}
}

func TestAwaitAgyPinnedModelReadyBindsLaunchLogAndUUID(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	start := time.Now().Add(-time.Second)
	logDir := filepath.Join(home, ".gemini", "antigravity-cli", "log")
	cache := filepath.Join(home, ".gemini", "antigravity-cli", "cache")
	conv := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(conv, 0o700); err != nil {
		t.Fatal(err)
	}
	old := start.Add(-time.Hour).In(time.Local).Format("20060102_150405")
	if err := os.WriteFile(filepath.Join(logDir, "cli-"+old+".log"), []byte("Resolving model gemini-3.1-pro-high\nPropagating selected model override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := start.In(time.Local).Format("20060102_150405")
	launchLog := filepath.Join(logDir, "cli-"+stamp+".log")
	if err := os.WriteFile(launchLog, []byte("Starting language server process with pid 4242\nResolving model gemini-3.1-pro-high\nfailed to apply model override: model gemini-3.1-pro-high is not recognized\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	idx, err := json.Marshal(map[string]string{cwd: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "last_conversations.json"), idx, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conv, id+".db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := AgyPinnedModelReadyRequest{
		Home: home, Cwd: cwd, PinnedModel: "gemini-3.1-pro-high",
		StartedAt: start, PID: 4242, Budget: 50 * time.Millisecond,
		LogDir: logDir, IndexPath: filepath.Join(cache, "last_conversations.json"), Conversations: conv,
	}
	if _, err := inspectAgyPinnedModelReady(req); err == nil {
		t.Fatal("failed override must not be ready")
	}
	if err := os.WriteFile(launchLog, []byte("Starting language server process with pid 4242\nResolving model gemini-3.1-pro-high\nfailed to apply model override: model gemini-3.1-pro-high is not recognized\nResolving model gemini-3.1-pro-high\nPropagating selected model override to backend: label=\"Gemini 3.1 Pro (High)\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ev, err := inspectAgyPinnedModelReady(req)
	if err != nil {
		t.Fatal(err)
	}
	if ev.ConversationID != id || ev.PinnedModel != "gemini-3.1-pro-high" || ev.LogFile != launchLog {
		t.Fatalf("evidence %+v", ev)
	}
}

func TestAwaitAgyPinnedModelReadyLiveTUI(t *testing.T) {
	if os.Getenv("HERD_AGY_LIVE_TUI") != "1" {
		t.Skip("set HERD_AGY_LIVE_TUI=1 to run the disposable AGY TUI readiness fixture")
	}
	if _, err := exec.LookPath("agy"); err != nil {
		t.Skip("agy not on PATH")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) required for a disposable TUI pty")
	}
	cmd := exec.Command(script, "-q", "/dev/null", "agy", "--model", "gemini-3.1-pro-high")
	cmd.Dir = cwd
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agy TUI: %v", err)
	}
	started := time.Now()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	ev, err := AwaitAgyPinnedModelReady(AgyPinnedModelReadyRequest{
		Home: home, Cwd: cwd, PinnedModel: "gemini-3.1-pro-high",
		StartedAt: started, Budget: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("pinned model never settled for this launch: %v", err)
	}
	if ev.PinnedModel != "gemini-3.1-pro-high" || ev.LogFile == "" {
		t.Fatalf("missing launch-specific model evidence %+v", ev)
	}
	if ev.ConversationID != "" && !agyConversation.MatchString(ev.ConversationID) {
		t.Fatalf("conversation id %q is not a genuine UUID", ev.ConversationID)
	}
}
