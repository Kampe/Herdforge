//go:build !darwin && !linux

package verifier

import (
	"errors"
	"fmt"
	"syscall"
)

var errPidfdUnsupported = errors.New("pidfd unsupported")

func tokenOf(pid int) (procToken, error) {
	return procToken{}, fmt.Errorf("tokenOf: unsupported platform")
}

func processIsZombie(pid int) bool { return false }

func snapshotProcesses() (processSnapshot, error) {
	return processSnapshot{}, fmt.Errorf("snapshotProcesses: unsupported platform")
}

func pidfdOpen(pid int) (int, error) { return -1, errPidfdUnsupported }

func pidfdSendSignal(fd int, sig syscall.Signal) error { return errPidfdUnsupported }

func pidfdExited(fd int) (bool, error) { return false, errPidfdUnsupported }

func isNotExistPidfd(err error) bool { return errors.Is(err, errPidfdUnsupported) }

func syscallSIGKILL() syscall.Signal { return syscall.SIGKILL }

func killPID(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
