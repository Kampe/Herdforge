//go:build linux

package signerboundary

import (
	"fmt"
	"syscall"
	"unsafe"
)

func peerCredsFD(fd uintptr) (uid, pid int, err error) {
	const solSocket = 1
	const soPeerCred = 17
	var cred struct {
		Pid int32
		Uid uint32
		Gid uint32
	}
	n := unsafe.Sizeof(cred)
	_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, solSocket, soPeerCred,
		uintptr(unsafe.Pointer(&cred)), uintptr(unsafe.Pointer(&n)), 0)
	if errno != 0 {
		return 0, 0, fmt.Errorf("SO_PEERCRED: %w", errno)
	}
	return int(cred.Uid), int(cred.Pid), nil
}
