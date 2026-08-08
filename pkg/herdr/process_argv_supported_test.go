//go:build darwin || linux

package herdr

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSystemPIDArgvCurrentProcess(t *testing.T) {
	argv, err := systemPIDArgv(os.Getpid())
	if err != nil {
		t.Fatalf("systemPIDArgv(current): %v", err)
	}
	if len(argv) == 0 {
		t.Fatal("systemPIDArgv returned empty argv")
	}
	if strings.TrimSpace(argv[0]) == "" {
		t.Fatalf("argv[0] empty/whitespace: %#v", argv)
	}
}

func TestSystemPIDArgvPreservesArguments(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "2")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	// cmd.Start() returns once the child is forked, which is BEFORE the exec has
	// necessarily replaced its image. On Linux /proc/<pid>/cmdline is empty in
	// that window, and CI failed here with "empty process cmdline for pid 11926"
	// while a developer box won the race every time. Poll for the argv to appear;
	// the assertion below is unchanged, so this removes the race without
	// weakening what the test proves.
	var argv []string
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for {
		argv, err = systemPIDArgv(cmd.Process.Pid)
		if err == nil && len(argv) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("argv never became readable within 5s: argv=%q err=%v", argv, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(argv) != 2 || argv[1] != "2" {
		t.Fatalf("system argv = %q, want executable plus exact argument 2", argv)
	}
}
