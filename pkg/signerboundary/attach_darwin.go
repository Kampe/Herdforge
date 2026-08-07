//go:build darwin

package signerboundary

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// tryAttach on Darwin uses PT_ATTACH. Cross-uid / SIP should deny; success is a boundary failure.
func tryAttach(pid int) error {
	const ptAttach = 10
	const ptDetach = 11
	_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, uintptr(ptAttach), uintptr(pid), 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	_, _, _ = syscall.Syscall6(syscall.SYS_PTRACE, uintptr(ptDetach), uintptr(pid), 0, 0, 0, 0)
	return nil
}

func processUIDPlatform(pid int) (int, bool) {
	// ps -o uid= -p PID
	out, err := exec.Command("/bin/ps", "-o", "uid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	u, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, false
	}
	return u, true
}

// peerPIDOfSocket: connect and use LOCAL_PEERPID.
func peerPIDOfSocket(socketPath string) int {
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
	for i, c := range path {
		addr.Path[i] = int8(c)
	}
	_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	// Even if connect fails (auth), we may not get peer. Use getsockopt after successful dial in client.
	_ = errno
	// Fallback: lsof
	out, err := exec.Command("/usr/sbin/lsof", "-t", socketPath).Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0
	}
	// Prefer the listening process — first pid
	p, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0
	}
	_ = fmt.Sprintf // keep
	return p
}
