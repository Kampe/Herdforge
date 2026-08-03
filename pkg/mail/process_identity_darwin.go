//go:build darwin

package mail

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processStartNS returns the process start time in Unix nanoseconds for pid.
func processStartNS(pid int) (int64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid")
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	tv := k.Proc.P_starttime
	return tv.Nano(), nil
}

func bootIdentity() string {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d.%d", tv.Sec, tv.Usec)
}
