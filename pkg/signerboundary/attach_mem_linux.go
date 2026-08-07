//go:build linux

package signerboundary

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// tryProcMemRead attempts process_vm_readv then /proc/pid/mem. nil = success (boundary fail).
func tryProcMemRead(pid int) error {
	if err := tryProcessVMRead(pid); err == nil {
		return nil
	}
	p := fmt.Sprintf("/proc/%d/mem", pid)
	f, err := openFileRO(p)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 16)
	_, err = f.ReadAt(buf, 0)
	return err
}

func tryProcessVMRead(pid int) error {
	local := make([]byte, 16)
	type iovec struct {
		Base *byte
		Len  uint64
	}
	localIov := iovec{&local[0], uint64(len(local))}
	remoteIov := struct {
		Base uint64
		Len  uint64
	}{0x400000, 16}
	// SYS_PROCESS_VM_READV: 310 on amd64, 270 on arm64
	num := unix.SYS_PROCESS_VM_READV
	_, _, errno := syscall.Syscall6(
		uintptr(num),
		uintptr(pid),
		uintptr(unsafe.Pointer(&localIov)),
		1,
		uintptr(unsafe.Pointer(&remoteIov)),
		1,
		0,
	)
	if errno == 0 {
		return nil
	}
	return errno
}

func isProcMemUnavailable(err error) bool {
	return false
}
