//go:build darwin

package verifier

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// processesHoldingMarker returns tokens for processes that still hold the
// ownership marker path open. This is lineage authority: only descendants that
// inherited (and retained) the marker FD appear here. Unrelated editors /
// watchers / fleet lanes that open candidate files without the marker are
// invisible to this scan.
func processesHoldingMarkerUntil(markerPath string, deadline time.Time) ([]procToken, error) {
	if markerPath == "" {
		return nil, nil
	}
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

	// Path-targeted lsof on the marker only — not the candidate tree.
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
		} else if len(out) == 0 {
			return nil, fmt.Errorf("processesHoldingMarker lsof: %w", err)
		}
	}

	excl := residualExcludePIDs()
	var (
		curPID int
		outTok []procToken
		seen   = map[int]struct{}{}
	)
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, perr := strconv.Atoi(line[1:])
			if perr != nil {
				curPID = 0
				continue
			}
			curPID = pid
		case 'n':
			if curPID <= 1 {
				continue
			}
			if _, skip := excl[curPID]; skip {
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
			if _, ok := seen[curPID]; ok {
				continue
			}
			tok, terr := tokenOf(curPID)
			if terr != nil {
				if isESRCH(terr) {
					continue
				}
				return nil, fmt.Errorf("processesHoldingMarker token %d: %w", curPID, terr)
			}
			seen[curPID] = struct{}{}
			outTok = append(outTok, tok)
		}
	}
	return outTok, nil
}
