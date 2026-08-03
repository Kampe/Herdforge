//go:build linux

package security

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// localPeerPID resolves the client PID for an accepted localhost TCP conn
// by matching the peer's source port inode in /proc/net/tcp{,6} (kernel tables).
// Used only as a secondary peer path; primary production auth is client-port claim.
func localPeerPID(c net.Conn) int {
	ta, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok || ta == nil {
		return 0
	}
	port := ta.Port
	if port <= 0 {
		return 0
	}
	if pid := pidForTCPPortLinux(port, false); pid > 0 {
		return pid
	}
	return pidForTCPPortLinux(port, true)
}

func peerPIDSupported() bool { return true }

func pidForTCPPortLinux(port int, v6 bool) int {
	path := "/proc/net/tcp"
	if v6 {
		path = "/proc/net/tcp6"
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	want := fmt.Sprintf("%04X", port)
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0
	}
	var inodes []string
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		la := fields[1]
		i := strings.IndexByte(la, ':')
		if i < 0 {
			continue
		}
		if !strings.EqualFold(la[i+1:], want) {
			continue
		}
		// state 01 = ESTABLISHED
		if fields[3] != "01" {
			continue
		}
		inodes = append(inodes, fields[9])
	}
	if len(inodes) == 0 {
		return 0
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	wantSet := map[string]bool{}
	for _, in := range inodes {
		wantSet[in] = true
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if strings.HasPrefix(link, "socket:[") && strings.HasSuffix(link, "]") {
				ino := link[len("socket:[") : len(link)-1]
				if wantSet[ino] {
					return pid
				}
			}
		}
	}
	return 0
}
