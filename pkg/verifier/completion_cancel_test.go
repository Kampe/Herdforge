package verifier

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

// TestRunShell_NilContextFailsClosed: nil context must not spawn work.
func TestRunShell_NilContextFailsClosed(t *testing.T) {
	if runShell(nil, t.TempDir(), "true") {
		t.Fatal("nil context must fail closed")
	}
}

func TestRunShellUsesHermeticGitConfiguration(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/host-gitconfig")
	if !runShell(context.Background(), t.TempDir(), `sh -c 'test "$GIT_CONFIG_GLOBAL" = /dev/null && test "$GIT_CONFIG_SYSTEM" = /dev/null && test "$GIT_CONFIG_NOSYSTEM" = 1'`) {
		t.Fatal("completion gate inherited host Git configuration")
	}
}

// TestRunShellCancellationKillsProcessGroup proves FAC-192: completion-gate
// cancel reaps the full Setpgid tree via procsignal, not leader-only kill.
// Wait for a live background child, then cancel — same barrier as execute.
//
// Liveness probes use package killPID (not raw syscall.Kill) so ordinary-source
// static recovery (FAC-198) still forbids unmediated host kill calls.
func TestRunShellCancellationKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	writeExecutable(t, filepath.Join(dir, "spawn-child"),
		"#!/bin/sh\nsleep 30 &\nchild=$!\nprintf '%s\\n' \"$child\" > \"$1\"\nwait \"$child\"\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		done <- runShell(ctx, dir, "./spawn-child "+pidFile)
	}()

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			if p, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && p > 1 {
				pid = p
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 1 {
		cancel()
		<-done
		t.Fatal("background child pid file never written")
	}
	if err := killPID(pid, 0); err != nil {
		cancel()
		<-done
		t.Fatalf("child %d already dead before cancel: %v", pid, err)
	}
	cancel()
	ok := <-done
	if ok {
		t.Fatal("canceled runShell must report failure")
	}
	goneDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(goneDeadline) {
		if err := killPID(pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = killPID(pid, syscall.SIGKILL)
	t.Fatalf("canceled completion gate left process-group child %d alive", pid)
}
