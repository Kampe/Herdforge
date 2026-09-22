package herdr

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

func TestAwaitAgyPinnedModelReadyInterleavedTimestampsBindOwnProcess(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	logDir := filepath.Join(home, ".gemini", "antigravity-cli", "log")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 22, 14, 10, 0, 0, time.Local)
	other := filepath.Join(logDir, "cli-"+start.Add(time.Second).Format("20060102_150405")+".log")
	ours := filepath.Join(logDir, "cli-"+start.Add(2*time.Second).Format("20060102_150405")+".log")
	if err := os.WriteFile(other, []byte("Starting language server process with pid 1111\nResolving model gemini-3.1-pro-high\nPropagating selected model override to backend: label=\"Gemini 3.1 Pro (High)\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ours, []byte("Starting language server process with pid 2222\nResolving model gemini-3.1-pro-high\nfailed to apply model override: model gemini-3.1-pro-high is not recognized\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := AgyPinnedModelReadyRequest{
		Home: home, Cwd: cwd, PinnedModel: "gemini-3.1-pro-high",
		StartedAt: start, Budget: time.Millisecond, LogDir: logDir,
	}
	if _, err := inspectAgyPinnedModelReady(req); err == nil {
		t.Fatal("timestamp-only inspect must fail closed")
	}
	picked := pickAgyLaunchLog([]agyLogCand{
		{path: other, stamp: start.Add(time.Second)},
		{path: ours, stamp: start.Add(2 * time.Second)},
	}, start)
	if picked != other {
		t.Fatalf("disproof: first-at-or-after-start selected %q, want the concurrent other launch", picked)
	}
	req.PID = 2222
	ev, err := inspectAgyPinnedModelReady(req)
	if err == nil {
		t.Fatalf("own process still failed override; must not inherit peer ready log %+v", ev)
	}
	req.PID = 1111
	ev, err = inspectAgyPinnedModelReady(req)
	if err != nil {
		t.Fatal(err)
	}
	if ev.LogFile != other {
		t.Fatalf("pid 1111 must bind the other launch log, got %s", ev.LogFile)
	}
}

func TestAwaitAgyPinnedModelReadyRejectsConcurrentOtherLaunch(t *testing.T) {
	home := t.TempDir()
	cwdA := t.TempDir()
	cwdB := t.TempDir()
	logDir := filepath.Join(home, ".gemini", "antigravity-cli", "log")
	cache := filepath.Join(home, ".gemini", "antigravity-cli", "cache")
	conv := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	for _, d := range []string{logDir, cache, conv} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	startA := time.Date(2026, 9, 22, 14, 0, 0, 0, time.Local)
	startB := startA.Add(time.Second)
	logA := filepath.Join(logDir, "cli-"+startA.Format("20060102_150405")+".log")
	logB := filepath.Join(logDir, "cli-"+startB.Format("20060102_150405")+".log")
	if err := os.WriteFile(logA, []byte("Starting language server process with pid 1111\nResolving model gemini-3.1-pro-high\nPropagating selected model override to backend: label=\"Gemini 3.1 Pro (High)\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logB, []byte("Starting language server process with pid 2222\nResolving model gemini-3.1-pro-high\nfailed to apply model override: model gemini-3.1-pro-high is not recognized\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	idA := "aaaaaaaa-bbbb-cccc-dddd-111111111111"
	idBstale := "aaaaaaaa-bbbb-cccc-dddd-000000000000"
	idx, err := json.Marshal(map[string]string{cwdA: idA, cwdB: idBstale})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "last_conversations.json"), idx, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conv, idA+".db"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(conv, idA+".db"), startA.Add(time.Second), startA.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conv, idBstale+".db"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(conv, idBstale+".db"), startB.Add(-time.Second), startB.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	reqB := AgyPinnedModelReadyRequest{
		Home: home, Cwd: cwdB, PinnedModel: "gemini-3.1-pro-high",
		StartedAt: startB, Budget: time.Millisecond,
		LogDir: logDir, IndexPath: filepath.Join(cache, "last_conversations.json"), Conversations: conv,
	}
	ev, err := inspectAgyPinnedModelReady(reqB)
	if err == nil {
		t.Fatalf("launch B must not inherit launch A's ready log, got %+v", ev)
	}
	if ev.LogFile == logA {
		t.Fatal("launch B bound launch A's log")
	}

	reqBPID := reqB
	reqBPID.PID = 2222
	if _, err := inspectAgyPinnedModelReady(reqBPID); err == nil {
		t.Fatal("pid 2222 still failed override; must not be ready")
	}
	reqBPID.PID = 1111
	ev, err = inspectAgyPinnedModelReady(reqBPID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.LogFile != logA {
		t.Fatalf("pid 1111 must bind process A's log, got %s", ev.LogFile)
	}

	reqA := AgyPinnedModelReadyRequest{
		Home: home, Cwd: cwdA, PinnedModel: "gemini-3.1-pro-high",
		StartedAt: startA, PID: 1111, Budget: time.Millisecond,
		LogDir: logDir, IndexPath: filepath.Join(cache, "last_conversations.json"), Conversations: conv,
	}
	ev, err = inspectAgyPinnedModelReady(reqA)
	if err != nil {
		t.Fatal(err)
	}
	if ev.LogFile != logA || ev.ConversationID != idA {
		t.Fatalf("launch A evidence %+v", ev)
	}

	id, err := agyLaunchConversationID(reqB)
	if err == nil {
		t.Fatalf("stale cwd UUID %s must not bind to launch B", id)
	}

	reqLoose := reqB
	reqLoose.PID = 111
	if _, err := inspectAgyPinnedModelReady(reqLoose); err == nil {
		t.Fatal("pid 111 must not match log pid 1111")
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
		StartedAt: started, PID: cmd.Process.Pid, Budget: 30 * time.Second,
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
	body, err := os.ReadFile(ev.LogFile)
	if err != nil {
		t.Fatal(err)
	}
	tokens := regexp.MustCompile(`pid (\d+)`).FindAllStringSubmatch(string(body), -1)
	t.Logf("launch pid=%d log=%s pin=%s conv=%s pid_tokens=%v children=%v", cmd.Process.Pid, filepath.Base(ev.LogFile), ev.PinnedModel, ev.ConversationID, tokens, listAgyChildPIDs(cmd.Process.Pid))
}
