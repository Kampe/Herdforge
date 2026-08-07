//go:build !darwin && !linux

package herdr

import "fmt"

// systemPIDArgv is unsupported outside darwin/linux; fail closed.
func systemPIDArgv(pid int) ([]string, error) {
	return nil, fmt.Errorf("process argv inspection unsupported on this platform (pid %d)", pid)
}
