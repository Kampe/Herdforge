//go:build darwin

package signerboundary

import (
	"fmt"
	"syscall"
	"unsafe"
)

func peerCredsFD(fd uintptr) (uid, pid int, err error) {
	// LOCAL_PEERCRED / LOCAL_PEERPID on Darwin
	const solLocal = 0         // SOL_LOCAL
	const localPeerPID = 0x002 // LOCAL_PEERPID
	var pidOut int32
	n := uint32(4)
	_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, solLocal, localPeerPID,
		uintptr(unsafe.Pointer(&pidOut)), uintptr(unsafe.Pointer(&n)), 0)
	if errno != 0 {
		// Fallback: xucred
		return peerCredsXucred(fd)
	}
	uidOut, uerr := peerUIDXucred(fd)
	if uerr != nil {
		return 0, int(pidOut), nil // pid only
	}
	return uidOut, int(pidOut), nil
}

func peerCredsXucred(fd uintptr) (uid, pid int, err error) {
	uid, err = peerUIDXucred(fd)
	return uid, 0, err
}

func peerUIDXucred(fd uintptr) (int, error) {
	// struct xucred
	const localPeerCred = 0x001
	const solLocal = 0
	type xucred struct {
		Version uint32
		UID     uint32
		NGroups int16
		Groups  [16]uint32
	}
	var xc xucred
	n := uint32(unsafe.Sizeof(xc))
	_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, solLocal, localPeerCred,
		uintptr(unsafe.Pointer(&xc)), uintptr(unsafe.Pointer(&n)), 0)
	if errno != 0 {
		return 0, fmt.Errorf("LOCAL_PEERCRED: %w", errno)
	}
	return int(xc.UID), nil
}
