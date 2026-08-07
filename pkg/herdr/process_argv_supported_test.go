//go:build darwin || linux

package herdr

import (
	"os"
	"os/exec"
	"strings"
	"testing"
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
	argv, err := systemPIDArgv(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 2 || argv[1] != "2" {
		t.Fatalf("system argv = %q, want executable plus exact argument 2", argv)
	}
}
