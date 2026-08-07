package confinement

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SessionRelRoot is the coordinator-owned confinement material root under the
// shared checkout. It is deliberately outside every task worktree so a
// confined agent cannot rewrite profile.sb or PATH wrappers (round-5 CRITICAL).
const SessionRelRoot = ".herd/confine-sessions"

// SessionPaths is the durable, outside-worktree layout for one launch.
type SessionPaths struct {
	Root    string // absolute session directory
	Profile string
	BinDir  string
	ZdotDir string
}

// NewSessionPaths builds <shared>/.herd/confine-sessions/<task>/g<lease>/.
// The directory is created by the coordinator (unsandboxed) before AgentStart.
func NewSessionPaths(sharedRoot, taskRef string, leaseGeneration int64) (SessionPaths, error) {
	if strings.TrimSpace(sharedRoot) == "" || strings.TrimSpace(taskRef) == "" || leaseGeneration <= 0 {
		return SessionPaths{}, fmt.Errorf("confinement: session identity incomplete")
	}
	shared, err := filepath.Abs(filepath.Clean(sharedRoot))
	if err != nil {
		return SessionPaths{}, err
	}
	// Sanitize task ref for a single path segment.
	task := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, taskRef)
	root := filepath.Join(shared, filepath.FromSlash(SessionRelRoot), task, "g"+strconv.FormatInt(leaseGeneration, 10))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return SessionPaths{}, err
	}
	bin := filepath.Join(root, "bin")
	zdot := filepath.Join(root, "zdot")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return SessionPaths{}, err
	}
	if err := os.MkdirAll(zdot, 0o755); err != nil {
		return SessionPaths{}, err
	}
	return SessionPaths{
		Root:    root,
		Profile: filepath.Join(root, "profile.sb"),
		BinDir:  bin,
		ZdotDir: zdot,
	}, nil
}

// FreezeSession makes session *files* non-writable after install (0444 profile,
// 0555 wrappers, 0444 zdot rc). Directories stay 0755 so the coordinator can
// replace material on the next lease and so test cleanup can unlink files.
// The confined agent still cannot write these paths: they sit outside the
// worktree write grant.
func FreezeSession(s SessionPaths) error {
	if err := os.Chmod(s.Profile, 0o444); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.BinDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Chmod(filepath.Join(s.BinDir, e.Name()), 0o555); err != nil {
			return err
		}
	}
	zentries, err := os.ReadDir(s.ZdotDir)
	if err != nil {
		return err
	}
	for _, e := range zentries {
		_ = os.Chmod(filepath.Join(s.ZdotDir, e.Name()), 0o444)
	}
	return nil
}
