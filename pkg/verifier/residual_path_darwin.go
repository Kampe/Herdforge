//go:build darwin

package verifier

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// processesTouchingDir returns causal tokens for processes that hold open the
// candidate directory (cwd or any open path under root), discovered via lsof.
// This is structural residual ownership for Darwin (no PID namespace/subreaper):
// a setsid/double-fork writer that still mutates the candidate remains visible.
// lsof / parse failures fail closed.
func processesTouchingDir(root string) ([]procToken, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("processesTouchingDir abs: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	// Darwin often surfaces /var as /private/var in lsof.
	privateAbs := abs
	if strings.HasPrefix(abs, "/var/") {
		privateAbs = "/private" + abs
	}
	abs = filepath.Clean(abs)
	privateAbs = filepath.Clean(privateAbs)

	// -n: no host resolution; -F pn: machine parseable pid + name fields.
	// No +D (slow recursive); we filter paths under root ourselves.
	cmd := exec.Command("lsof", "-n", "-F", "pn")
	out, err := cmd.Output()
	if err != nil {
		// lsof returns 1 when it finds no matches or partial; still parse stdout.
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) == 0 && len(out) > 0 {
			// fall through with partial output
		} else if len(out) == 0 {
			return nil, fmt.Errorf("processesTouchingDir lsof: %w", err)
		}
	}
	self := os.Getpid()
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
			if curPID <= 1 || curPID == self {
				continue
			}
			name := line[1:]
			if !pathUnderRootDarwin(name, abs) && !pathUnderRootDarwin(name, privateAbs) {
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
				return nil, fmt.Errorf("processesTouchingDir token %d: %w", curPID, terr)
			}
			seen[curPID] = struct{}{}
			outTok = append(outTok, tok)
		}
	}
	return outTok, nil
}

func pathUnderRootDarwin(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	sep := string(os.PathSeparator)
	return strings.HasPrefix(path, root+sep)
}
