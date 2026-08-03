//go:build linux

package verifier

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// processesTouchingDir returns causal tokens for processes that still hold the
// candidate open via cwd or any open FD path under root (including after
// chdir-away). Control-plane PIDs (self + ancestors) are excluded. Fail-closed
// on unrecoverable /proc errors.
func processesTouchingDir(root string) ([]procToken, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("processesTouchingDir abs: %w", err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		// Candidate may not exist yet; still require a clean abs path.
		abs, err = filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("processesTouchingDir abs: %w", err)
		}
	}
	abs = filepath.Clean(abs)

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("processesTouchingDir /proc: %w", err)
	}
	excl := residualExcludePIDs()
	var out []procToken
	seen := map[int]struct{}{}
	for _, e := range entries {
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
		if touches, terr := linuxPIDTouchesDir(pid, abs); terr != nil {
			// ESRCH/ENOENT: process exited mid-scan — not an error.
			if os.IsNotExist(terr) || isESRCH(terr) {
				continue
			}
			// Permission denied on foreign processes is common; skip those PIDs.
			if os.IsPermission(terr) {
				continue
			}
			return nil, fmt.Errorf("processesTouchingDir pid %d: %w", pid, terr)
		} else if !touches {
			continue
		}
		tok, err := tokenOf(pid)
		if err != nil {
			if os.IsNotExist(err) || isESRCH(err) {
				continue
			}
			return nil, fmt.Errorf("processesTouchingDir token %d: %w", pid, err)
		}
		if _, ok := seen[tok.pid]; ok {
			continue
		}
		seen[tok.pid] = struct{}{}
		out = append(out, tok)
	}
	return out, nil
}

func linuxPIDTouchesDir(pid int, root string) (bool, error) {
	// cwd under root counts (writer still rooted in candidate).
	if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		if pathUnderRoot(cwd, root) {
			return true, nil
		}
	} else if !os.IsNotExist(err) && !isESRCH(err) && !os.IsPermission(err) {
		// keep scanning fds; permission on cwd alone is not fatal
	}
	// open fds — includes descendant files after chdir-away.
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		if os.IsNotExist(err) || isESRCH(err) || os.IsPermission(err) {
			return false, nil
		}
		return false, err
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if err != nil {
			continue
		}
		// Skip non-path targets (sockets, pipes).
		if strings.HasPrefix(target, "/") && pathUnderRoot(target, root) {
			return true, nil
		}
	}
	return false, nil
}

func pathUnderRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}
