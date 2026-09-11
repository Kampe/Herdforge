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
	darwinMaxPID        = 1<<31 - 1
)

func checkedDarwinPID(pid int) (uint32, error) {
	// Darwin's pid_t is a positive signed 32-bit value. Validate before the
	// narrowing conversion so an invalid caller cannot wrap into another PID.
	if pid <= 0 || pid > darwinMaxPID {
		return 0, errors.New("darwin process PID is outside pid_t bounds")
	}
	return uint32(pid), nil
}

func readDarwinProcessArgs(pid int) ([]byte, error) {
	return readDarwinProcessArgsWith(pid, queryDarwinProcessArgs, func(pid int) error {
		return syscall.Kill(pid, 0)
	})
}

// queryDarwinProcessArgs uses a nil buffer to query the required allocation,
// and a populated buffer to read it. Both sysctl calls can race process exit.
func queryDarwinProcessArgs(pid uint32, data []byte) (uint64, syscall.Errno) {
	mib := [4]uint32{darwinCTLKern, darwinKernProcArgs2, pid, 0}
	size := uint64(len(data))
	if len(data) == 0 {
		_, _, errno := syscall.Syscall6(darwinSysctl, uintptr(unsafe.Pointer(&mib[0])), 4, 0, uintptr(unsafe.Pointer(&size)), 0, 0)
		return size, errno
	}
	_, _, errno := syscall.Syscall6(darwinSysctl, uintptr(unsafe.Pointer(&mib[0])), 4, uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&size)), 0, 0)
	return size, errno
}

func readDarwinProcessArgsWith(pid int, query func(uint32, []byte) (uint64, syscall.Errno), probe func(int) error) ([]byte, error) {
	checkedPID, err := checkedDarwinPID(pid)
	if err != nil {
		return nil, err
	}
	classify := func(errno syscall.Errno) error {
		// EINVAL also denotes a live process whose argument block cannot be
		// read. Only ESRCH from a fresh liveness probe resolves it as gone.
		// A live/reused PID, EPERM, or any unknown probe error stays protected.
		if errno == syscall.ESRCH || (errno == syscall.EINVAL && errors.Is(probe(pid), syscall.ESRCH)) {
			return os.ErrProcessDone
		}
		return errno
	}
	size, errno := query(checkedPID, nil)
	if errno != 0 {
		return nil, classify(errno)
	}
	if size == 0 || size > 1<<20 {
		return nil, errors.New("darwin process argument block exceeds bound")
	}
	data := make([]byte, size)
	size, errno = query(checkedPID, data)
	if errno != 0 {
		return nil, classify(errno)
	}
	if size == 0 || size > uint64(len(data)) {
		return nil, errors.New("darwin process argument block exceeds bound")
	}
	return data[:size], nil
}
