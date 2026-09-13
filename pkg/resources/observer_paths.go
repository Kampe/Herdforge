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
	// ScopePerCheckout is the narrow fallback used when no home directory can
	// be resolved. The path is relative, so it resolves inside whichever
	// checkout the process runs in: exclusivity covers THAT checkout and
	// nothing else. It is named for what it does rather than for the
	// canonical root it is not.
	ScopePerCheckout ObserverScope = "per-checkout"
	// ScopeInjected is what a caller-supplied path buys: exclusivity over that
	// exact file and no claim beyond it. Tests inject; production does not.
	ScopeInjected ObserverScope = "injected-path"
)

// ObserverLockID is the LOGICAL name of the observer lock, safe to publish.
// It identifies which lock is meant without disclosing where the host keeps it.
func ObserverLockID() string { return observerDirName + "/" + observerLockName }

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
		return filepath.Join(observerFallbackSubdir, observerDirName), ScopePerCheckout
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

// ObserverLockScope reports the exclusivity actually in force for the CANONICAL
// paths, for publication alongside the observation. Any scope other than
// same-user-host tells a consumer that a second observer may be running
// elsewhere on this host.
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
	case ScopePerCheckout:
		return "exclusive for the checkout this process runs in: no home directory could be resolved, so the lock path is relative and another checkout gets its own"
	case ScopeInjected:
		return "exclusive over the injected path only: no host-wide or cross-worktree singleton is claimed"
	}
	return "unknown scope: treat exclusivity as unproven"
}
