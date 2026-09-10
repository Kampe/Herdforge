//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// startProcessGroup puts the build command into its own process group so
// timeout cleanup can signal every descendant, not just the shell.
func startProcessGroup(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

// killProcessGroup signals the negative process group with SIGKILL.
func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
