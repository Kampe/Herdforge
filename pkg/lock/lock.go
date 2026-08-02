package lock

// DirLock is an advisory, short-lived mutual-exclusion primitive that
// serializes MUTATIONS of the shared checkout. A mid-repair in-place hotfix
// was raced out TWICE while the deployable was DOWN by concurrent
// `git pull --autostash` in that same checkout (2026-07-24, platform-ops).
// mkdir is atomic and portable, so no flock (absent on macOS). WITH a lock
// never blocks our raw git command.
//
// A crashed holder never wedges the fleet: a lock whose `holder` pid is
// dead, or whose directory is older than maxAge (HERD_SHARED_LOCK_MAX_AGE,
// default 300s), is broken automatically before acquisition.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// EnvHeld is the re-entrancy marker: nested `with` calls see it set and
	// skip acquire/release so `dev-up -> apply-migrations` cannot deadlock.
	EnvHeld = "HERD_SHARED_LOCK_HELD"
	// EnvLockDir overrides the lockdir location.
	EnvLockDir = "HERD_SHARED_LOCK_DIR"
	// EnvCanonicalRoot overrides the shared checkout root.
	EnvCanonicalRoot = "HERD_CANONICAL_ROOT"
	// EnvMaxAge overrides the stale-lock age bound in seconds.
	EnvMaxAge = "HERD_SHARED_LOCK_MAX_AGE"
	// EnvDirtyOK disables the fail-closed dirty-checkout refusal.
	EnvDirtyOK = "HERD_SHARED_DIRTY_OK"

	// HolderFile is the name of the lock's holder metadata file.
	HolderFile = "holder"
	// DefaultRelDir is the default lockdir relative to the canonical root.
	DefaultRelDir = ".git/herd-shared-checkout.lock.d"
	// DefaultMaxAge is the stale-lock age bound.
	DefaultMaxAge = 300 * time.Second
)

// DirLock is an advisory mkdir-based file-system lock.
type DirLock struct {
	dir    string
	holder string
	maxAge time.Duration
}

// NewDirLock returns a lock rooted at dir with the default stale-age bound.
func NewDirLock(dir string) *DirLock {
	return &DirLock{dir: dir, holder: filepath.Join(dir, HolderFile), maxAge: DefaultMaxAge}
}

// Dir returns the lock directory path.
func (l *DirLock) Dir() string { return l.dir }

// SetMaxAge overrides the default stale-lock age bound.
func (l *DirLock) SetMaxAge(age time.Duration) { l.maxAge = age }

// Acquire takes the lock, breaking any stale lock first. A re-entrant call
// (HERD_SHARED_LOCK_HELD set in the environment) returns immediately without
// touching the filesystem. Returns an error after wait expires.
func (l *DirLock) Acquire(ctx context.Context, wait time.Duration, reason string) error {
	if os.Getenv(EnvHeld) != "" {
		return nil
	}
	waited := 0
	waitSecs := int(wait.Seconds())
	for {
		l.breakIfStale()
		if err := os.Mkdir(l.dir, 0o755); err == nil {
			// holder write is best-effort (zsh `> "$holder" ... || true`).
			l.writeHolder(reason)
			return nil
		}
		if waited >= waitSecs {
			return fmt.Errorf("shared checkout locked by [%s], waited %ds", l.holderStr(), waitSecs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		waited++
	}
}

// Release removes the lock. Advisory-only: ownership is by convention.
func (l *DirLock) Release() {
	_ = os.RemoveAll(l.dir)
}

// Status reports whether the lock is held and, if so, the holder string.
func (l *DirLock) Status() (held bool, holderStr string) {
	if _, err := os.Stat(l.dir); err != nil {
		return false, ""
	}
	return true, l.holderStr()
}

// breakIfStale removes the lockdir when the holder pid is dead or the
// directory is older than maxAge. The two and only two auto-release rules.
func (l *DirLock) breakIfStale() (removed bool) {
	info, err := os.Stat(l.dir)
	if err != nil {
		return false // dir missing -> not stale
	}
	// dead/invalid holder pid -> stale. A pid is ALIVE only when kill(pid,0)
	// returns nil or EPERM; ESRCH (gone) and EINVAL (above PID_MAX on macOS)
	// both mean the holder can't be live.
	if pid := l.holderPID(); pid != "" {
		if n, err := strconv.Atoi(pid); err == nil {
			kerr := syscall.Kill(n, 0)
			if !(kerr == nil || errors.Is(kerr, syscall.EPERM)) {
				_ = os.RemoveAll(l.dir)
				return true
			}
		}
	}
	// too old -> stale
	if time.Since(info.ModTime()) > l.maxAge {
		_ = os.RemoveAll(l.dir)
		return true
	}
	return false
}

// holderPID returns the first `pid=` value in the holder file, or "".
func (l *DirLock) holderPID() string {
	f, err := os.Open(l.holder)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "pid=") {
			return strings.TrimPrefix(line, "pid=")
		}
	}
	return ""
}

// holderStr prints the holder file with newlines collapsed to spaces, or
// "(unknown)".
func (l *DirLock) holderStr() string {
	data, err := os.ReadFile(l.holder)
	if err != nil {
		return "(unknown)"
	}
	collapsed := strings.ReplaceAll(strings.TrimSpace(string(data)), "\n", " ")
	if collapsed == "" {
		return "(unknown)"
	}
	return collapsed
}

func (l *DirLock) writeHolder(reason string) {
	agent := os.Getenv("HERD_LANE")
	if agent == "" {
		agent = os.Getenv("HERD_AGENT")
	}
	if agent == "" {
		agent = username()
	}
	content := fmt.Sprintf("pid=%d\nagent=%s\nreason=%s\n", os.Getpid(), agent, reason)
	if f, err := os.OpenFile(l.holder, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644); err == nil {
		_, _ = f.WriteString(content)
		_ = f.Close()
	}
}

func username() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}
