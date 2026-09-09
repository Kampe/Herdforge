package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/mail"
)

func queuedSendRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "fac773@test"},
		{"config", "user.name", "fac773"},
		{"commit", "--allow-empty", "-q", "-m", "base"},
	} {
		if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func installQueuedSendFake(t *testing.T, status, pid string) (bin, logPath, statusPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "herdr")
	logPath = filepath.Join(dir, "calls.log")
	statusPath = filepath.Join(dir, "status")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$HERD_FAKE_LOG"
case "$1 $2" in
  "agent list")
    st=$(cat "$HERD_FAKE_STATUS")
    printf '{"result":{"agents":[{"name":"worker","pane_id":"p1","workspace_id":"wK","agent_status":"%s"}]}}\n' "$st"
    ;;
  "agent prompt")
    shift 2
    printf '%s' "$*" > "$HERD_FAKE_PROMPT"
    printf '{"result":{"delivered":true}}\n'
    ;;
  "agent send-keys") printf '{"result":{"ok":true}}\n' ;;
  "pane read")
    if [ -s "$HERD_FAKE_PROMPT" ]; then
      body=$(cat "$HERD_FAKE_PROMPT")
      printf '{"result":{"text":"%s"}}\n' "$body"
    else
      printf '{"result":{"text":"running sleep"}}\n'
    fi
    ;;
  "pane process-info")
    printf '{"result":{"process_info":{"foreground_processes":[{"pid":%s,"name":"sleep"}]}}}\n' "$(cat "$HERD_FAKE_PID")"
    ;;
  "workspace list") printf '{"result":{"workspaces":[{"workspace_id":"wK"}]}}\n' ;;
  *) printf '{"result":{}}\n' ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(dir, "pid")
	if err := os.WriteFile(pidPath, []byte(pid), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(herdr.BinaryEnv, bin)
	t.Setenv(herdr.NoLiveEnv, "1")
	promptPath := filepath.Join(dir, "prompt")
	if err := os.WriteFile(promptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_FAKE_LOG", logPath)
	t.Setenv("HERD_FAKE_STATUS", statusPath)
	t.Setenv("HERD_FAKE_PID", pidPath)
	t.Setenv("HERD_FAKE_PROMPT", promptPath)
	t.Setenv("HERD_WORKSPACE", "wK")
	return bin, logPath, statusPath
}

func startFakeCommand(t *testing.T) *os.Process {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake command: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process
}

func queuedSendEnv(bin, repo string) []string {
	return []string{
		herdr.BinaryEnv + "=" + bin,
		herdr.NoLiveEnv + "=1",
		"HERD_WORKSPACE=wK",
		"HERD_ROOT=" + repo,
		"PATH=" + filepath.Dir(bin) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HERD_FAKE_LOG=" + os.Getenv("HERD_FAKE_LOG"),
		"HERD_FAKE_STATUS=" + os.Getenv("HERD_FAKE_STATUS"),
		"HERD_FAKE_PID=" + os.Getenv("HERD_FAKE_PID"),
		"HERD_FAKE_PROMPT=" + os.Getenv("HERD_FAKE_PROMPT"),
	}
}

func fakeCallLog(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSendCLIQueuesBusyWithoutTouchingLiveCommand(t *testing.T) {
	proc := startFakeCommand(t)
	repo := queuedSendRepo(t)
	bin, logPath, _ := installQueuedSendFake(t, "working", strconv.Itoa(proc.Pid))
	payload := "routine status\n`keep ticks` and $(not a subshell)"
	payloadFile := filepath.Join(repo, "msg.txt")
	if err := os.WriteFile(payloadFile, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runHerd(t, repo, queuedSendEnv(bin, repo), "send", "worker", "--file", payloadFile, "--no-verify")
	if err != nil {
		t.Fatalf("herd send busy: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, herdr.StatusQueuedDurable) || !strings.Contains(text, "envelope ") {
		t.Fatalf("want queued-durable receipt, got %s", text)
	}
	if strings.Contains(text, "consumption confirmed") {
		t.Fatalf("queued send claimed consumption: %s", text)
	}
	log := fakeCallLog(t, logPath)
	for _, forbidden := range []string{"agent prompt", "send-keys", "kill", "signal"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("busy send touched the pane (%q):\n%s", forbidden, log)
		}
	}
	if err := syscall.Kill(proc.Pid, 0); err != nil {
		t.Fatalf("fake live command was signaled or exited: %v", err)
	}

	box := mail.NewMailbox(filepath.Join(repo, ".herd", "control-mail.jsonl"))
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != payload {
		t.Fatalf("mailbox payload = %+v", pending)
	}
}

func TestSendCLIIdleKickoffStillPrompts(t *testing.T) {
	proc := startFakeCommand(t)
	repo := queuedSendRepo(t)
	bin, logPath, _ := installQueuedSendFake(t, "idle", strconv.Itoa(proc.Pid))
	out, err := runHerd(t, repo, queuedSendEnv(bin, repo), "send", "worker", "short kick", "--no-verify")
	if err != nil {
		t.Fatalf("idle send: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "submitted") {
		t.Fatalf("idle kickoff output = %s", out)
	}
	log := fakeCallLog(t, logPath)
	if !strings.Contains(log, "agent prompt") || !strings.Contains(log, "send-keys") {
		t.Fatalf("idle kickoff did not submit: %s", log)
	}
	if err := syscall.Kill(proc.Pid, 0); err != nil {
		t.Fatalf("idle kickoff killed the command: %v", err)
	}
}

func TestSendCLIDrainSurfacesThenAcksWithoutStoppingCommand(t *testing.T) {
	proc := startFakeCommand(t)
	repo := queuedSendRepo(t)
	bin, logPath, statusPath := installQueuedSendFake(t, "working", strconv.Itoa(proc.Pid))
	env := queuedSendEnv(bin, repo)
	body := "queued packet bytes"
	out, err := runHerd(t, repo, env, "send", "worker", body, "--no-verify")
	if err != nil {
		t.Fatalf("queue: %v\n%s", err, out)
	}

	if out, err := runHerd(t, repo, env, "send", "worker", "--drain"); err == nil {
		t.Fatalf("drain while working must refuse, got %s", out)
	}
	if strings.Contains(fakeCallLog(t, logPath), "agent prompt") {
		t.Fatalf("working drain prompted:\n%s", fakeCallLog(t, logPath))
	}
	if err := syscall.Kill(proc.Pid, 0); err != nil {
		t.Fatalf("working drain stopped the command: %v", err)
	}

	if err := os.WriteFile(statusPath, []byte("idle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runHerd(t, repo, env, "send", "worker", "--drain")
	if err != nil {
		t.Fatalf("idle drain: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "acknowledged=true") {
		t.Fatalf("idle drain output = %s", out)
	}
	log := fakeCallLog(t, logPath)
	if !strings.Contains(log, "agent prompt") || !strings.Contains(log, body) {
		t.Fatalf("idle drain did not surface the envelope:\n%s", log)
	}
	if err := syscall.Kill(proc.Pid, 0); err != nil {
		t.Fatalf("idle drain stopped the command: %v", err)
	}

	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runHerd(t, repo, env, "send", "worker", "--drain")
	if err != nil {
		t.Fatalf("repeat drain: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "no pending durable envelopes") {
		t.Fatalf("repeat drain output = %s", out)
	}
	if strings.Contains(fakeCallLog(t, logPath), "agent prompt") {
		t.Fatalf("acked drain re-prompted:\n%s", fakeCallLog(t, logPath))
	}
}

func TestWatchWakeReconcilesPreexistingOrdinaryReportAndRestartDoesNotRedeliver(t *testing.T) {
	proc := startFakeCommand(t)
	repo := queuedSendRepo(t)
	bin, logPath, _ := installQueuedSendFake(t, "idle", strconv.Itoa(proc.Pid))
	env := queuedSendEnv(bin, repo)
	body := "exact report bytes\nsecond line"
	box := mail.NewMailbox(filepath.Join(repo, ".herd", "control-mail.jsonl"))
	report, err := box.SendMessage("worker", "worker", "FAC-773 report", body)
	if err != nil {
		t.Fatal(err)
	}

	out, err := runHerd(t, repo, env, "watch", "--wake", "--recipient", "worker", "--workspace", "wK", "--interval", "1", "--timeout", "3")
	if err != nil {
		t.Fatalf("watch wake: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "WAKE worker workspace=wK durable mail consumed") {
		t.Fatalf("wake output = %s", out)
	}
	log := fakeCallLog(t, logPath)
	if strings.Count(log, "agent prompt") != 1 || !strings.Contains(log, body) {
		t.Fatalf("report was not delivered byte-for-byte once:\n%s", log)
	}
	handled, err := box.Handled("worker", report.ID)
	if err != nil || !handled {
		t.Fatalf("report acknowledgement = %t, %v", handled, err)
	}

	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = runHerd(t, repo, env, "watch", "--wake", "--recipient", "worker", "--workspace", "wK", "--interval", "1", "--timeout", "1")
	if exitCode(err) != 2 {
		t.Fatalf("empty post-ack watch exit = %d, want bounded timeout", exitCode(err))
	}
	if strings.Contains(fakeCallLog(t, logPath), "agent prompt") {
		t.Fatalf("restart redelivered acknowledged report:\n%s", fakeCallLog(t, logPath))
	}
}

func TestMailCLIFromWorktreeWritesCanonicalProjectMailbox(t *testing.T) {
	repo := queuedSendRepo(t)
	lane := filepath.Join(repo, ".worktrees", "mender-fac773-route")
	if out, err := testgit.Command(repo, "worktree", "add", "-q", "-b", "fac773-route", lane).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}
	bin, _, _ := installQueuedSendFake(t, "working", "0")
	env := queuedSendEnv(bin, repo)
	env = append(env, "HERD_ROOT="+lane, "HERD_PROJECT_ROOT=")
	body := "worktree producer report"
	out, err := runHerd(t, lane, env, "mail", "send", "--from", "worker", "--to", "coordinator", "--subject", "FAC-773 report", "--body", body)
	if err != nil {
		t.Fatalf("mail send from worktree: %v\n%s", err, out)
	}

	canonical := filepath.Join(repo, ".herd", "control-mail.jsonl")
	canonicalBox := mail.NewMailbox(canonical)
	inbox, err := canonicalBox.ReadInbox("coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].Body != body {
		t.Fatalf("canonical inbox = %+v, want exact report body", inbox)
	}
	if _, err := os.Stat(filepath.Join(lane, ".herd", "control-mail.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("worktree-local mailbox was written, stat err=%v", err)
	}
}

func TestSendCLIRefusesForeignWorkspace(t *testing.T) {
	proc := startFakeCommand(t)
	repo := queuedSendRepo(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	logPath := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$HERD_FAKE_LOG"
case "$1 $2" in
  "agent list") printf '{"result":{"agents":[{"name":"worker","pane_id":"p1","workspace_id":"wB","agent_status":"idle"}]}}\n' ;;
  *) printf '{"result":{}}\n' ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		herdr.BinaryEnv + "=" + bin,
		herdr.NoLiveEnv + "=1",
		"HERD_WORKSPACE=wK",
		"HERD_FAKE_LOG=" + logPath,
		"PATH=" + filepath.Dir(bin) + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	if out, err := runHerd(t, repo, env, "send", "worker", "do not misroute", "--no-verify"); err == nil {
		t.Fatalf("foreign workspace send succeeded: %s", out)
	}
	log := fakeCallLog(t, logPath)
	if strings.Contains(log, "agent prompt") || strings.Contains(log, "send-keys") {
		t.Fatalf("foreign workspace was prompted:\n%s", log)
	}
	if err := syscall.Kill(proc.Pid, 0); err != nil {
		t.Fatalf("foreign-workspace refusal killed the command: %v", err)
	}
}

func TestSendCLIDoesNotContactLiveBusyPanes(t *testing.T) {
	// A missing HERD_HERDR_BIN override must refuse rather than reach the
	// operator fleet. PATH is reduced so LookPath cannot find a live herdr.
	repo := queuedSendRepo(t)
	env := []string{herdr.NoLiveEnv + "=1", "PATH=/usr/bin:/bin", herdr.BinaryEnv + "="}
	out, err := runHerd(t, repo, env, "send", "worker", "no live", "--no-verify")
	if err == nil {
		t.Fatalf("send without fake herdr reached a live binary: %s", out)
	}
	if !strings.Contains(string(out), "herdr CLI not found") && !strings.Contains(string(out), "LIVE herdr") && !strings.Contains(string(out), "herdr") {
		t.Fatalf("missing-fake refusal = %s", out)
	}
}
