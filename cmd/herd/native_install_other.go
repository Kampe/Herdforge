//go:build windows

package main

import (
	"errors"
	"os/exec"
)

func startProcessGroup(cmd *exec.Cmd) error { return nil }

func killProcessGroup(pid int) error { return errors.New("process-group kill unsupported") }
