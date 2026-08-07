//go:build darwin

package herdr

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// systemPIDArgv returns the OS argv for pid via sysctl kern.procargs2.
func systemPIDArgv(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid pid %d", pid)
	}
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("read process argv: %w", err)
	}
	argv, err := parseKERNProcargs2(data)
	if err != nil {
		return nil, fmt.Errorf("parse process argv for pid %d: %w", pid, err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty process argv for pid %d", pid)
	}
	return argv, nil
}
