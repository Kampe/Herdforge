//go:build darwin

package verifier

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

var errPidfdUnsupported = errors.New("pidfd unsupported")

// SZOMB is Darwin zombie state (sys/proc.h).
const szomb int8 = 5

func tokenOf(pid int) (procToken, error) {
	if pid <= 1 {
		return procToken{}, fmt.Errorf("tokenOf: invalid pid %d", pid)
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return procToken{}, err
	}
	return procToken{
		pid:       int(kp.Proc.P_pid),
		startSec:  int64(kp.Proc.P_starttime.Sec),
		startUsec: int64(kp.Proc.P_starttime.Usec),
	}, nil
}

func processIsZombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false
	}
	return kp.Proc.P_stat == szomb
}

func snapshotProcesses() (processSnapshot, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return processSnapshot{}, fmt.Errorf("kern.proc.all: %w", err)
	}
	s := processSnapshot{
		byPID: make(map[int]procToken, len(kps)),
		ppid:  make(map[int]int, len(kps)),
		pgid:  make(map[int]int, len(kps)),
	}
	for i := range kps {
		kp := &kps[i]
		pid := int(kp.Proc.P_pid)
		if pid <= 1 {
			continue
		}
		s.byPID[pid] = procToken{
			pid:       pid,
			startSec:  int64(kp.Proc.P_starttime.Sec),
			startUsec: int64(kp.Proc.P_starttime.Usec),
		}
		s.ppid[pid] = int(kp.Eproc.Ppid)
		s.pgid[pid] = int(kp.Eproc.Pgid)
	}
	return s, nil
}

// Darwin has no pidfd; openHandle falls back to token-only.
func pidfdOpen(pid int) (int, error) {
	return -1, errPidfdUnsupported
}

func pidfdSendSignal(fd int, sig syscall.Signal) error {
	return errPidfdUnsupported
}

func isNotExistPidfd(err error) bool {
	return errors.Is(err, errPidfdUnsupported)
}

func syscallSIGKILL() syscall.Signal { return syscall.SIGKILL }

func killPID(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
