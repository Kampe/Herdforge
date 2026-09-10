//go:build darwin

package resources

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

const (
	darwinCTLKern       = 1
	darwinKernProcArgs2 = 49
	darwinSysctl        = 202
)

func readDarwinProcessArgs(pid int) ([]byte, error) {
	mib := [4]uint32{darwinCTLKern, darwinKernProcArgs2, uint32(pid), 0}
	var size uint64
	_, _, errno := syscall.Syscall6(darwinSysctl, uintptr(unsafe.Pointer(&mib[0])), 4, 0, uintptr(unsafe.Pointer(&size)), 0, 0)
	if errno != 0 {
		if errno == syscall.ESRCH {
			return nil, os.ErrProcessDone
		}
		return nil, errno
	}
	if size == 0 || size > 1<<20 {
		return nil, errors.New("darwin process argument block exceeds bound")
	}
	data := make([]byte, size)
	_, _, errno = syscall.Syscall6(darwinSysctl, uintptr(unsafe.Pointer(&mib[0])), 4, uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&size)), 0, 0)
	if errno != 0 {
		return nil, errno
	}
	return data[:size], nil
}
