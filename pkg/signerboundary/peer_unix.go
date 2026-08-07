//go:build unix

package signerboundary

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// peerCreds extracts OS peer identity from a unix connection.
func peerCreds(conn net.Conn) (uid, pid int, exe string, err error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, 0, "", fmt.Errorf("not a unix conn")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, "", err
	}
	var (
		sysErr error
		ruid   int
		rpid   int
	)
	cerr := raw.Control(func(fd uintptr) {
		ruid, rpid, sysErr = peerCredsFD(fd)
	})
	if cerr != nil {
		return 0, 0, "", cerr
	}
	if sysErr != nil {
		return 0, 0, "", sysErr
	}
	exe = peerExe(rpid)
	return ruid, rpid, exe, nil
}

func peerExe(pid int) string {
	if pid <= 0 {
		return ""
	}
	// Linux
	if p, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return canonPath(p)
	}
	// Darwin / generic: ps full command path when absolute
	out, err := exec.Command("/bin/ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return ""
	}
	// First field is often the executable path or name.
	return canonPath(fields[0])
}
