//go:build linux

package signerboundary

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// tryAttach attempts PTRACE_ATTACH. nil means attach succeeded (boundary fail).
func tryAttach(pid int) error {
	if err := unix.PtraceAttach(pid); err != nil {
		return err
	}
	_ = unix.PtraceDetach(pid)
	var wstat syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &wstat, 0, nil)
	return nil
}

func processUIDPlatform(pid int) (int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				u, err := strconv.Atoi(fields[1])
				if err == nil {
					return u, true
				}
			}
		}
	}
	return 0, false
}

func peerPIDOfSocket(socketPath string) int {
	// Let x/sys own Linux's filesystem/abstract sockaddr encoding and length
	// validation, then ask the connected socket for its authenticated peer.
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0
	}
	defer unix.Close(fd)
	if err := unix.Connect(fd, &unix.SockaddrUnix{Name: socketPath}); err != nil {
		return 0
	}
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0
	}
	return int(cred.Pid)
}
