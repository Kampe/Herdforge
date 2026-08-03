package verifier

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// residualExcludePIDs returns PIDs that must never be path-residual killed:
// the current verifier process and every ancestor up the PPID chain (control
// plane / daemon / test runner). Path residual ownership is for escaped
// candidate writers, not self or the orchestration plane.
func residualExcludePIDs() map[int]struct{} {
	excl := make(map[int]struct{}, 8)
	for _, start := range []int{os.Getpid(), os.Getppid()} {
		pid := start
		for depth := 0; pid > 1 && depth < 64; depth++ {
			if _, seen := excl[pid]; seen {
				break
			}
			excl[pid] = struct{}{}
			ppid, err := processParentPID(pid)
			if err != nil || ppid <= 1 || ppid == pid {
				if ppid > 1 {
					excl[ppid] = struct{}{}
				}
				break
			}
			pid = ppid
		}
	}
	return excl
}

// processParentPID returns the parent PID of pid (best-effort). Failures are
// non-fatal for exclusion building — the caller still excludes self/ppid.
func processParentPID(pid int) (int, error) {
	if pid <= 1 {
		return 0, fmt.Errorf("processParentPID: invalid pid %d", pid)
	}
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("processParentPID parse: %w", err)
	}
	return ppid, nil
}

// filterResidualTokens drops control-plane and invalid PIDs from a path
// residual scan. leader, when > 1, is also excluded (ownership supervisor).
func filterResidualTokens(toks []procToken, leader int) []procToken {
	excl := residualExcludePIDs()
	if leader > 1 {
		excl[leader] = struct{}{}
	}
	out := make([]procToken, 0, len(toks))
	for _, tok := range toks {
		if tok.pid <= 1 {
			continue
		}
		if _, skip := excl[tok.pid]; skip {
			continue
		}
		out = append(out, tok)
	}
	return out
}
