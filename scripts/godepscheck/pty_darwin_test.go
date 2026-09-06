//go:build darwin

package godepscheck

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY() (*os.File, *os.File, error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	master := os.NewFile(uintptr(mfd), "/dev/ptmx")
	if _, err := unix.IoctlGetInt(mfd, unix.TIOCPTYGRANT); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCPTYGRANT: %w", err)
	}
	if _, err := unix.IoctlGetInt(mfd, unix.TIOCPTYUNLK); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCPTYUNLK: %w", err)
	}
	var buf [128]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mfd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCPTYGNAME: %w", errno)
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	slave, err := os.OpenFile(string(buf[:n]), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open slave: %w", err)
	}
	return master, slave, nil
}
