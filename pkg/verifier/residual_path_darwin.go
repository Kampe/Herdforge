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

// processesTouchingDir returns causal tokens for processes that still hold any
// path under the candidate root open (cwd OR any open descendant file FD).
// Discovery uses a full open-file table filtered by path-under-root — not
// directory-inode-only — so a setsid writer that chdir's away but keeps a
// descendant file open remains visible. Control-plane PIDs (self + ancestors)
// are excluded so residual ownership never targets the verifier/daemon.
//
// Fail-closed: lsof binary/tool failure with no parseable stdout is an error.
// lsof exit 1 with empty stdout means no match (empty residual set).
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

	// Full open-file table, filtered by path-under-root. This is O(open files
	// on host) rather than O(tree size); it finds descendant FDs after chdir.
	// -n: no host resolution; -F pn: machine-parseable pid + name fields.
	cmd := exec.Command("lsof", "-n", "-F", "pn")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if len(out) == 0 {
				// No open files matched anything lsof could report.
				_ = ee
				return nil, nil
			}
			// Partial output: parse what we got.
		} else if len(out) == 0 {
			return nil, fmt.Errorf("processesTouchingDir lsof: %w", err)
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
			// lsof may append (mount flags) etc.; take the path token.
			if i := strings.IndexByte(name, ' '); i >= 0 {
				name = name[:i]
			}
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
