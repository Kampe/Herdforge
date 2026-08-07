//go:build linux

package signerboundary

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

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
	// Parse /proc/net/unix is heavy; use abstract connect + SO_PEERCRED after dial.
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return 0
	}
	defer syscall.Close(fd)
	var addr syscall.RawSockaddrUnix
	addr.Family = syscall.AF_UNIX
	path := []byte(socketPath)
	if len(path) >= len(addr.Path) {
		return 0
	}
	for i, b := range path {
		addr.Path[i] = int8(b)
	}
	_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	if errno != 0 {
		return 0
	}
	// SO_PEERCRED
	const solSocket = 1
	const soPeerCred = 17
	var cred struct {
		Pid int32
		Uid uint32
		Gid uint32
	}
	size := unsafe.Sizeof(cred)
	_, _, errno = syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(fd), solSocket, soPeerCred,
		uintptr(unsafe.Pointer(&cred)), uintptr(unsafe.Pointer(&size)), 0)
	if errno != 0 {
		return 0
	}
	return int(cred.Pid)
}
