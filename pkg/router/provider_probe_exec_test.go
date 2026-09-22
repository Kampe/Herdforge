package router

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Disposable fake child: timeout must kill the owned process group, including
// a grandchild holding stdout, and Wait must return without hanging on the pipe.
func TestDefaultExecProviderProbeTimeoutReapsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := `#!/bin/sh
# Grandchild inherits stdout so a leader-only kill would stall Wait.
( while true; do printf 'still\n'; sleep 1; done ) &
echo $! > "$1"
wait
`
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, _, timedOut := defaultExecProviderProbe(ctx, "sh", []string{"-c", script, "probe", pidFile}, "")
	elapsed := time.Since(started)
	if !timedOut {
		t.Fatal("fake child must time out")
	}
	// timeout + WaitDelay (100ms) plus a small reap window; a leaked pipe waiter
	// hangs far past this.
	if elapsed > 2*time.Second {
		t.Fatalf("pipe wait was not bounded: waited %s", elapsed)
	}
	body, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("script never reached the descendant spawn: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	alive := false
	for i := 0; i < 40; i++ {
		if err := syscall.Kill(pid, 0); err == nil {
			alive = true
			time.Sleep(50 * time.Millisecond)
			continue
		}
		alive = false
		break
	}
	if alive {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("probe descendant %d survived process-group teardown", pid)
	}
}

func TestAgyAdmissionArgvPlacesJSONFlagsBeforePrint(t *testing.T) {
	command, args, stdin, err := providerProbeCommand("agy", "gemini-3.1-pro-high")
	if err != nil {
		t.Fatal(err)
	}
	if command != "agy" {
		t.Fatalf("command = %q", command)
	}
	if strings.TrimSpace(stdin) != "" {
		t.Fatalf("agy prompt is positional, stdin = %q", stdin)
	}
	printAt, formatAt := -1, -1
	foundJSON, foundDisable := false, false
	for i, a := range args {
		switch a {
		case "--print", "-p", "--prompt":
			if printAt < 0 {
				printAt = i
			}
		case "--output-format":
			formatAt = i
		case "json":
			foundJSON = true
		case "--disable-slash-commands":
			foundDisable = true
		}
	}
	if printAt < 0 || formatAt < 0 || !foundJSON || !foundDisable {
		t.Fatalf("agy argv missing structured-print flags: %v", args)
	}
	if formatAt > printAt {
		t.Fatalf("--output-format after --print would become the prompt: %v", args)
	}
}

func TestAgyAdmissionTimeoutReasonStaysUnknownAndNonAdmitting(t *testing.T) {
	original := execProviderProbe
	execProviderProbe = func(context.Context, string, []string, string) (string, string, error, bool) {
		healthy := `{"conversation_id":"sess-1","status":"SUCCESS","response":"HERD_PROVIDER_PROBE_OK"}`
		return healthy, "", context.DeadlineExceeded, true
	}
	t.Cleanup(func() { execProviderProbe = original })

	ok, reason := runProviderProbe("agy", "gemini-3.1-pro-high", providerProbeBudget)
	if ok {
		t.Fatal("timeout must not admit")
	}
	if !strings.Contains(reason, ProbeUnknownPrefix) {
		t.Fatalf("timeout must stay UNKNOWN: %q", reason)
	}
	if strings.Contains(strings.ToLower(reason), "healthy") {
		t.Fatalf("timeout reason must not claim healthy: %q", reason)
	}
}
