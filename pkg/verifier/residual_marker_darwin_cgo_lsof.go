//go:build darwin && cgo

package verifier

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processesHoldingMarkerViaLsof is the Darwin-cgo permission fallback for
// marker lineage. It inspects only the private marker path and binds every
// returned PID to its current kernel start token before adoption.
func processesHoldingMarkerViaLsof(markerPath string, deadline time.Time) ([]procToken, error) {
	abs, err := filepath.Abs(markerPath)
	if err != nil {
		return nil, fmt.Errorf("processesHoldingMarker abs: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	abs = filepath.Clean(abs)
	privateAbs := abs
	if strings.HasPrefix(abs, "/var/") {
		privateAbs = "/private" + abs
	}

	args := []string{"-n", "-F", "pn", "--", abs}
	if privateAbs != abs {
		args = append(args, privateAbs)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", args...)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("processesHoldingMarker lsof deadline: %w", ctx.Err())
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok && len(out) == 0 {
			return nil, nil
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("processesHoldingMarker lsof: %w", err)
		}
	}

	excl := residualExcludePIDs()
	var currentPID int
	seen := make(map[int]struct{})
	result := make([]procToken, 0)
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, perr := strconv.Atoi(line[1:])
			if perr != nil {
				currentPID = 0
				continue
			}
			currentPID = pid
		case 'n':
			if currentPID <= 1 {
				continue
			}
			if _, skip := excl[currentPID]; skip {
				continue
			}
			name := line[1:]
			if i := strings.IndexByte(name, ' '); i >= 0 {
				name = name[:i]
			}
			name = filepath.Clean(name)
			if name != abs && name != privateAbs {
				continue
			}
			if _, ok := seen[currentPID]; ok {
				continue
			}
			tok, terr := tokenOf(currentPID)
			if terr != nil {
				if isESRCH(terr) || errors.Is(terr, syscall.EIO) {
					continue
				}
				return nil, fmt.Errorf("processesHoldingMarker token %d: %w", currentPID, terr)
			}
			seen[currentPID] = struct{}{}
			result = append(result, tok)
		}
	}
	return result, nil
}
