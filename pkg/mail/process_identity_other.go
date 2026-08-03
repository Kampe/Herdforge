//go:build !darwin && !linux

package mail

import "fmt"

// Unsupported platforms: refuse to claim process identity rather than
// silently using PID-only (PID reuse hazard).
func processStartNS(pid int) (int64, error) {
	return 0, fmt.Errorf("process start identity unsupported on this platform")
}

func bootIdentity() string { return "" }
