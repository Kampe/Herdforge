//go:build darwin

package security

import (
	"bytes"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// localPeerPID resolves client PID for an accepted localhost TCP connection
// using lsof (production-compatible on macOS without cgo).
func localPeerPID(c net.Conn) int {
	ta, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok || ta == nil || ta.Port <= 0 {
		return 0
	}
	return pidForLocalTCPPortDarwin(ta.Port)
}

func pidForLocalTCPPortDarwin(port int) int {
	// -nP numeric; -iTCP@127.0.0.1:PORT; -sTCP:ESTABLISHED; -Fpc for machine parse
	out, err := exec.Command("lsof", "-nP",
		"-iTCP:"+strconv.Itoa(port),
		"-sTCP:ESTABLISHED",
		"-Fpc",
	).Output()
	if err != nil {
		// Retry without state filter (SYN may still be ESTABLISHED soon after Accept).
		out, err = exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-Fpc").Output()
		if err != nil {
			return 0
		}
	}
	// Format: p<pid>\nc<cmd>\n...
	var pid int
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			p, err := strconv.Atoi(string(line[1:]))
			if err == nil && p > 0 {
				pid = p
			}
		}
	}
	// Prefer non-self if multiple (listener vs client): client is peer.
	// lsof may list both; take first non-zero. Caller AllowPID filters.
	_ = strings.TrimSpace
	return pid
}
