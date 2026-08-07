//go:build linux

package verifier

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var errPidfdUnsupported = errors.New("pidfd unsupported")

func tokenOf(pid int) (procToken, error) {
	if pid <= 1 {
		return procToken{}, fmt.Errorf("tokenOf: invalid pid %d", pid)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procToken{}, err
	}
	s := string(data)
	idx := strings.LastIndex(s, ") ")
	if idx < 0 {
		return procToken{}, fmt.Errorf("tokenOf: parse /proc/%d/stat", pid)
	}
	rest := strings.Fields(s[idx+2:])
	if len(rest) < 20 {
		return procToken{}, fmt.Errorf("tokenOf: short /proc/%d/stat", pid)
	}
	start, err := strconv.ParseInt(rest[19], 10, 64)
	if err != nil {
		return procToken{}, err
	}
	return procToken{pid: pid, startSec: start, startUsec: 0}, nil
}

func processIsZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(data)
	idx := strings.LastIndex(s, ") ")
	if idx < 0 {
		return false
	}
	rest := strings.Fields(s[idx+2:])
	if len(rest) < 1 {
		return false
	}
	return rest[0] == "Z"
}

func snapshotProcesses() (processSnapshot, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return processSnapshot{}, err
	}
	s := processSnapshot{
		byPID: make(map[int]procToken, len(entries)),
		ppid:  make(map[int]int, len(entries)),
		pgid:  make(map[int]int, len(entries)),
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		tok, err := tokenOf(pid)
		if err != nil {
			continue
		}
		s.byPID[pid] = tok
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		str := string(data)
		idx := strings.LastIndex(str, ") ")
		if idx < 0 {
			continue
		}
		rest := strings.Fields(str[idx+2:])
		if len(rest) < 3 {
			continue
		}
		ppid, _ := strconv.Atoi(rest[1])
		pgrp, _ := strconv.Atoi(rest[2])
		s.ppid[pid] = ppid
		s.pgid[pid] = pgrp
	}
	return s, nil
}

func pidfdOpen(pid int) (int, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			return -1, errPidfdUnsupported
		}
		return -1, err
	}
	return fd, nil
}

func pidfdSendSignal(fd int, sig syscall.Signal) error {
	return unix.PidfdSendSignal(fd, sig, nil, 0)
}

func isNotExistPidfd(err error) bool {
	return errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EINVAL)
}

func syscallSIGKILL() syscall.Signal { return syscall.SIGKILL }

func killPID(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
