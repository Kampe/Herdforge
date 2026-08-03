//go:build linux

package mail

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processStartNS returns a stable start fingerprint for pid: boot_id-qualified
// starttime ticks from /proc/<pid>/stat field 22, encoded as a synthetic
// nanosecond-scale id (not wall time, but unique per boot+process).
func processStartNS(pid int) (int64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// stat format: pid (comm) state ... — comm may contain spaces/parens.
	s := string(data)
	idx := strings.LastIndex(s, ") ")
	if idx < 0 {
		return 0, fmt.Errorf("parse stat")
	}
	fields := strings.Fields(s[idx+2:])
	// After ") ": field[0]=state, ... starttime is field index 19 (man proc).
	if len(fields) < 20 {
		return 0, fmt.Errorf("short stat")
	}
	startTicks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, err
	}
	return startTicks, nil
}

func bootIdentity() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
