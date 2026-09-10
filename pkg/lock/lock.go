package lock

// DirLock is an advisory, short-lived mutual-exclusion primitive that
// serializes MUTATIONS of the shared checkout. A mid-repair in-place hotfix
// was raced out TWICE while the deployable was DOWN by concurrent
// `git pull --autostash` in that same checkout (2026-07-24, platform-ops).
// mkdir is atomic and portable and carries the visible holder state; the
// MUTUAL EXCLUSION itself is anchored by a kernel-held advisory flock
// (LOCK_EX) on a persistent sibling lock file that is created once and
// never unlinked — the same primitive pkg/harvestmerge and pkg/security
// already rely on (the historical note that flock is absent on macOS is
// wrong). A waiter may never stale-break a lock while the flock is held
// (the holder is provably live), and Release removes only the directory
// inode it pinned. WITH a lock never blocks our raw git command.
//
// A crashed holder never wedges the fleet: a lock whose `holder` pid is
// dead, or whose directory is older than maxAge (HERD_SHARED_LOCK_MAX_AGE,
// default 300s), is broken automatically before acquisition.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
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

// DirLock is an advisory mkdir-based file-system lock whose exclusion is
// anchored by a kernel-held advisory flock on a persistent lock file.
type DirLock struct {
	dir       string
	holder    string
	token     string
	maxAge    time.Duration
	flockFile *os.File
	dirFile   *os.File
}

// NewDirLock returns a lock rooted at dir with the default stale-age bound.
func NewDirLock(dir string) *DirLock {
	return &DirLock{dir: dir, holder: filepath.Join(dir, HolderFile), maxAge: DefaultMaxAge}
}

// Dir returns the lock directory path.
func (l *DirLock) Dir() string { return l.dir }

// SetMaxAge overrides the default stale-lock age bound.
func (l *DirLock) SetMaxAge(age time.Duration) { l.maxAge = age }

// Acquire takes the lock, breaking any stale lock first. A re-entrant call —
// HERD_SHARED_LOCK_HELD naming THIS lockdir, as `herd lock with` exports it —
// returns immediately without touching the filesystem. Returns an error after
// wait expires.
//
// The marker is compared against l.dir: any-value-means-held granted every
// OTHER lock for free, so a distinct lock (the forge coordinator fence) taken
// underneath `herd lock with` was never actually held.
func (l *DirLock) Acquire(ctx context.Context, wait time.Duration, reason string) error {
	if held := os.Getenv(EnvHeld); held != "" && held == l.dir {
		return nil
	}
	waited := 0
	waitSecs := int(wait.Seconds())
	flockUnsupported := false
	flockHeld := false
	for {
		if !flockHeld && !flockUnsupported {
			switch held, err := l.tryFlock(); {
			case err != nil:
				// flock is unsupported on this filesystem: degrade to the
				// historical mkdir-only semantics rather than refuse.
				flockUnsupported = true
			case held:
				flockHeld = true
			default:
				// EWOULDBLOCK: a live holder owns the kernel exclusion. Its
				// lock directory must never be stale-broken by a waiter.
				if waited >= waitSecs {
					return fmt.Errorf("shared checkout locked by [%s], waited %ds", l.holderStr(), waitSecs)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
				waited++
				continue
			}
		}
		l.breakIfStale()
		if err := os.Mkdir(l.dir, 0o755); err == nil {
			// owner token: Release must never remove a lock that a successor
			// took over in the meantime.
			l.token = newOwnerToken()
			// holder write is best-effort (zsh `> "$holder" ... || true`).
			l.writeHolder(reason)
			// Pin the directory inode this owner created: Release removes
			// only the directory it pinned, never a replacement.
			l.dirFile, _ = os.Open(l.dir)
			return nil
		}
		if waited >= waitSecs {
			l.releaseFlock()
			return fmt.Errorf("shared checkout locked by [%s], waited %ds", l.holderStr(), waitSecs)
		}
		select {
		case <-ctx.Done():
			l.releaseFlock()
			return ctx.Err()
		case <-time.After(time.Second):
		}
		waited++
	}
}

// flockPath returns the persistent advisory lock file path: a SIBLING of the
// lock directory that is created once and NEVER unlinked, so its inode is
// stable and kernel-held for the lifetime of the checkout. The historical
// comment claiming flock is absent on macOS is wrong — darwin has BSD
// flock, and pkg/harvestmerge already relies on it.
func (l *DirLock) flockPath() string { return l.dir + ".flock" }

// tryFlock takes LOCK_EX|LOCK_NB on the persistent lock file. Returns
// (true, nil) when exclusion is held, (false, nil) when a live holder owns
// it (EWOULDBLOCK), and an error when flock is unsupported.
func (l *DirLock) tryFlock() (bool, error) {
	if l.flockFile != nil {
		return true, nil
	}
	f, err := os.OpenFile(l.flockPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return false, err
	}
	l.flockFile = f
	return true, nil
}

// releaseFlock drops the kernel exclusion and closes the descriptors.
func (l *DirLock) releaseFlock() {
	if l.flockFile != nil {
		_ = syscall.Flock(int(l.flockFile.Fd()), syscall.LOCK_UN)
		_ = l.flockFile.Close()
		l.flockFile = nil
	}
	if l.dirFile != nil {
		_ = l.dirFile.Close()
		l.dirFile = nil
	}
}

// Release removes the lock — but only when this owner still owns it. If a
// successor took the lock over (stale break, takeover) and wrote its own
// holder token, the stale owner's release is a no-op instead of destroying
// the successor's exclusion. Holder files from before owner tokens existed
// (no token line) keep the historical remove behavior.
//
// The removal is additionally bound to the PINNED directory inode: the
// kernel-held flock guarantees no compliant acquirer could have replaced
// the directory while this owner held the exclusion, and the SameFile check
// refuses even a non-compliant (raw mkdir/rm) replacement installed inside
// the check/remove interval. Releasing the flock is the LAST action: a
// successor cannot exist between the token/identity check and the removal.
func (l *DirLock) Release() {
	// This owner's kernel exclusion and pinned descriptor are dropped on
	// EVERY path — including the no-op paths: a stale owner must never keep
	// the flock its successor now needs. The deferred release also means
	// the flock is still held while the directory is removed, so no
	// compliant successor can appear between the checks and the removal.
	defer l.releaseFlock()
	if current := holderToken(l.holder); current != "" && current != l.token {
		return
	}
	if interleaveHook != nil {
		interleaveHook("release-pre-remove")
	}
	// Bind the removal to the PINNED directory inode as the last action
	// before destruction: even a non-compliant (raw mkdir/rm) replacement
	// installed inside the check/remove interval is never destroyed.
	if l.dirFile != nil {
		if pinned, err := l.dirFile.Stat(); err == nil {
			if now, lerr := os.Lstat(l.dir); lerr != nil || !os.SameFile(pinned, now) {
				// The directory at the name is not the one this owner
				// created: a replacement (successor or foreign writer) must
				// survive.
				return
			}
		}
	}
	_ = removeLockDir(l.dir)
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
// Callers that hold the kernel flock (all compliant acquirers) run this
// under exclusion; the removal itself is bound to the OBSERVED inode — a
// fresh lock installed between the staleness decision and the removal is
// never destroyed.
func (l *DirLock) breakIfStale() (removed bool) {
	info, err := os.Stat(l.dir)
	if err != nil {
		return false // dir missing -> not stale
	}
	breakAndRemove := func() bool {
		if interleaveHook != nil {
			interleaveHook("stale-pre-remove")
		}
		if now, lerr := os.Lstat(l.dir); lerr != nil || !os.SameFile(info, now) {
			return false
		}
		_ = removeLockDir(l.dir)
		return true
	}
	// dead/invalid holder pid -> stale. A pid is ALIVE only when kill(pid,0)
	// returns nil or EPERM; ESRCH (gone) and EINVAL (above PID_MAX on macOS)
	// both mean the holder can't be live.
	if pid := l.holderPID(); pid != "" {
		if n, err := strconv.Atoi(pid); err == nil {
			kerr := syscall.Kill(n, 0)
			if !(kerr == nil || errors.Is(kerr, syscall.EPERM)) {
				return breakAndRemove()
			}
		}
	}
	// too old -> stale
	if time.Since(info.ModTime()) > l.maxAge {
		return breakAndRemove()
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
	content := fmt.Sprintf("pid=%d\nagent=%s\nreason=%s\ntoken=%s\n", os.Getpid(), agent, reason, l.token)
	if f, err := os.OpenFile(l.holder, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644); err == nil {
		_, _ = f.WriteString(content)
		_ = f.Close()
	}
}

// holderToken returns the `token=` value in the holder file, or "".
func holderToken(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "token=") {
			return strings.TrimPrefix(line, "token=")
		}
	}
	return ""
}

func newOwnerToken() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("fallback-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
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

// interleaveHook, when non-nil, fires at the exact check-then-remove
// boundaries of Release and breakIfStale. It is a deterministic test seam
// for interleaving reproduction (bundle-interleaving-repair-2306);
// production leaves it nil.
var interleaveHook func(stage string)

// removeLockDir is the removal primitive for the lock directory, a package
// variable so tests can observe the check/remove boundary.
var removeLockDir = os.RemoveAll
