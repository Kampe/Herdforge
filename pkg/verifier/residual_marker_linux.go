//go:build linux

package verifier

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// processesHoldingMarker returns tokens for processes whose open FDs still
// reference the ownership marker inode. Lineage authority only — not path
// contact under the candidate.
func processesHoldingMarkerUntil(markerPath string, deadline time.Time) ([]procToken, error) {
	if markerPath == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(markerPath)
	if err != nil {
		return nil, fmt.Errorf("processesHoldingMarker abs: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("processesHoldingMarker stat: %w", err)
	}
	want, ok := fileIdent(info)
	if !ok {
		return nil, fmt.Errorf("processesHoldingMarker: no file identity for %s", abs)
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("processesHoldingMarker /proc: %w", err)
	}
	excl := residualExcludePIDs()
	var out []procToken
	seen := map[int]struct{}{}
	for _, e := range entries {
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("processesHoldingMarker: deadline exceeded")
		}
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if _, skip := excl[pid]; skip {
			continue
		}
		holds, herr := linuxPIDHoldsMarker(pid, want)
		if herr != nil {
			if os.IsNotExist(herr) || isESRCH(herr) || os.IsPermission(herr) {
				continue
			}
			return nil, fmt.Errorf("processesHoldingMarker pid %d: %w", pid, herr)
		}
		if !holds {
			continue
		}
		tok, terr := tokenOf(pid)
		if terr != nil {
			if os.IsNotExist(terr) || isESRCH(terr) {
				continue
			}
			return nil, fmt.Errorf("processesHoldingMarker token %d: %w", pid, terr)
		}
		if _, ok := seen[tok.pid]; ok {
			continue
		}
		seen[tok.pid] = struct{}{}
		out = append(out, tok)
	}
	return out, nil
}

func linuxPIDHoldsMarker(pid int, want fileID) (bool, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return false, err
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if err != nil {
			continue
		}
		if !filepath.IsAbs(target) {
			continue
		}
		st, err := os.Stat(target)
		if err != nil {
			continue
		}
		got, ok := fileIdent(st)
		if ok && got == want {
			return true, nil
		}
	}
	return false, nil
}
