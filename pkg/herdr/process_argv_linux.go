//go:build linux

package herdr

import (
	"fmt"
	"os"
)

// systemPIDArgv returns the OS argv for pid via /proc/<pid>/cmdline.
func systemPIDArgv(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid pid %d", pid)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, fmt.Errorf("read process argv: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty process cmdline for pid %d", pid)
	}
	return parseProcCmdline(data)
}
