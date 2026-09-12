package resources

// FAC-829: where the observer locks and publishes, and what that scope ACTUALLY
// is.
//
// An earlier audit of mine claimed a repo-relative lock path gave a host-wide
// singleton. It does not: FileLockProvider locks the path it is handed, so two
// worktrees passing two repo-relative paths acquire two different locks and
// both run. The fix is not a stronger claim, it is a canonical path plus an
// honest scope string that says which guarantee is actually in force.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/posture"
)

// Observer artifact names. They are deliberately distinct from every existing
// lock in the tree: the observer must never take the capacity/reaper lock, and
// must never be blocked by one. Holding a dispatch lock for an observer's
// twelve-hour lifetime would convert an observation tool into an outage.
const (
	observerDirName        = "observer"
	observerLockName       = "resources-observer.lock"
	observerStatusName     = "resources-observer.json"
	observerFallbackSubdir = ".herd"
)

// ObserverScope names the guarantee a lock path actually provides.
type ObserverScope string

const (
	// ScopeSameUserHost is the real singleton: the lock lives under the user's
	// home-anchored state root, so every worktree, clone and checkout run by
	// this user on this host resolves the SAME file.
	ScopeSameUserHost ObserverScope = "same-user-host"
	// ScopeStateRoot is what an explicit HERD_STATE_DIR or XDG_STATE_HOME
	// gives: exclusivity across everything sharing that root, and nothing
	// beyond it. Two shells exporting different roots get two observers.
	ScopeStateRoot ObserverScope = "configured-state-root"
	// ScopeRepository is the narrow fallback used when no home directory can
	// be resolved. Exclusivity covers this checkout only. It is reported as
	// such rather than dressed up.
	ScopeRepository ObserverScope = "canonical-repository"
)

// observerRoot resolves the canonical observer directory and says which scope
// that resolution actually bought.
//
// posture.StateDir is reused rather than reimplemented: a second copy of the
// state-root rule is how two callers end up disagreeing about where state
// lives, which this repository has already paid for more than once.
func observerRoot() (string, ObserverScope) {
	base := posture.StateDir()
	dir := filepath.Join(base, observerDirName)

	if strings.TrimSpace(os.Getenv("HERD_STATE_DIR")) != "" ||
		strings.TrimSpace(os.Getenv("XDG_STATE_HOME")) != "" {
		return dir, ScopeStateRoot
	}
	// posture.StateDir falls back to a repo-relative path when there is no
	// home directory. A relative path is per-checkout, so the honest scope is
	// the narrow one.
	if !filepath.IsAbs(base) {
		return filepath.Join(observerFallbackSubdir, observerDirName), ScopeRepository
	}
	return dir, ScopeSameUserHost
}

// ObserverLockPath is the canonical observer lock.
//
// There is deliberately NO flag to override it. A caller-supplied lock path
// would let two observers each hold "a lock" and both believe they were the
// singleton, which is the false-exclusivity claim this exists to prevent.
func ObserverLockPath() string {
	dir, _ := observerRoot()
	return filepath.Join(dir, observerLockName)
}

// ObserverStatusPath is where the bounded snapshot is published.
//
// It is derived, not chosen, for the same reason: a caller-chosen status path
// could be pointed at the operational guard's report or at a source file, and
// the observer must never overwrite either.
func ObserverStatusPath() string {
	dir, _ := observerRoot()
	return filepath.Join(dir, observerStatusName)
}

// ObserverLockScope reports the exclusivity actually in force, for publication
// alongside the observation. A consumer reading "canonical-repository" knows a
// second observer may be running elsewhere on this host.
func ObserverLockScope() ObserverScope {
	_, scope := observerRoot()
	return scope
}

// ObserverScopeExplanation is the one-sentence form for help text and reports.
func ObserverScopeExplanation(scope ObserverScope) string {
	switch scope {
	case ScopeSameUserHost:
		return "exclusive for this user on this host: every worktree and clone resolves the same lock"
	case ScopeStateRoot:
		return "exclusive across the configured HERD_STATE_DIR/XDG_STATE_HOME root only; a different root elsewhere would run a second observer"
	case ScopeRepository:
		return "exclusive for this checkout only: no home directory could be resolved, so the lock is repository-relative"
	}
	return "unknown scope: treat exclusivity as unproven"
}
