package resources

// FAC-829: the lock scope must be honest. An earlier audit claimed a
// repo-relative path gave a host-wide singleton; it does not, and these tests
// exist so the claim published alongside an observation matches the file that
// is actually locked.

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestObserverLockIsDistinctFromCapacityAndReaperLocks protects the rule that
// an observer must never take, or be blocked by, a sweep's lock. Holding a
// dispatch lock for a twelve-hour observer lifetime would turn observation into
// an outage.
func TestObserverLockIsDistinctFromCapacityAndReaperLocks(t *testing.T) {
	t.Setenv("HERD_STATE_DIR", t.TempDir())
	lock := ObserverLockPath()
	status := ObserverStatusPath()

	if lock == status {
		t.Fatalf("observer lock and status resolve to the same file %q", lock)
	}
	if filepath.Base(lock) != observerLockName {
		t.Fatalf("observer lock file is %q, expected %q", filepath.Base(lock), observerLockName)
	}
	if filepath.Base(filepath.Dir(lock)) != observerDirName {
		t.Fatalf("observer lock lives in %q, expected its own %q directory",
			filepath.Dir(lock), observerDirName)
	}
	// Names owned by other subsystems. Sharing any of them would mean the
	// observer is contending for a lock that is not about observation.
	for _, foreign := range []string{"resource-governor", "worktree-reap-pulse.lock", "cache-use.lock"} {
		if strings.Contains(lock, foreign) {
			t.Fatalf("observer lock %q collides with the %q lock", lock, foreign)
		}
	}
}

// TestObserverLockScopeIsReportedHonestly is the point of the whole file: the
// scope string must describe the guarantee that the resolved path actually
// provides, not the strongest-sounding one.
func TestObserverLockScopeIsReportedHonestly(t *testing.T) {
	t.Run("configured state root narrows the claim", func(t *testing.T) {
		t.Setenv("HERD_STATE_DIR", t.TempDir())
		if got := ObserverLockScope(); got != ScopeStateRoot {
			t.Fatalf("an explicit HERD_STATE_DIR reported scope %q, expected %q: two shells with different roots run two observers",
				got, ScopeStateRoot)
		}
	})

	t.Run("xdg state home narrows the claim", func(t *testing.T) {
		t.Setenv("HERD_STATE_DIR", "")
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		if got := ObserverLockScope(); got != ScopeStateRoot {
			t.Fatalf("an explicit XDG_STATE_HOME reported scope %q, expected %q", got, ScopeStateRoot)
		}
	})

	t.Run("every scope explains itself", func(t *testing.T) {
		for _, scope := range []ObserverScope{ScopeSameUserHost, ScopeStateRoot, ScopePerCheckout, ScopeInjected} {
			explanation := ObserverScopeExplanation(scope)
			if strings.TrimSpace(explanation) == "" {
				t.Fatalf("scope %q has no explanation to publish", scope)
			}
			if strings.Contains(explanation, "unknown scope") {
				t.Fatalf("scope %q fell through to the unknown-scope text", scope)
			}
		}
	})

	t.Run("the per-checkout label does not claim a canonical root", func(t *testing.T) {
		// The fallback path is relative, so it resolves inside whichever
		// checkout runs it. Calling that "canonical-repository" would imply a
		// single resolved root that does not exist.
		if strings.Contains(string(ScopePerCheckout), "canonical") {
			t.Fatalf("scope %q claims a canonical root it does not resolve", ScopePerCheckout)
		}
		if !strings.Contains(ObserverScopeExplanation(ScopePerCheckout), "another checkout") {
			t.Fatalf("the per-checkout explanation does not say another checkout gets its own lock")
		}
	})

	t.Run("an unrecognised scope refuses to sound safe", func(t *testing.T) {
		explanation := ObserverScopeExplanation(ObserverScope("invented"))
		if !strings.Contains(explanation, "unproven") {
			t.Fatalf("an unrecognised scope explained itself as %q; it must state that exclusivity is unproven", explanation)
		}
	})
}

// TestObserverPathsHaveNoCallerOverride records the deliberate absence of a
// flag: DefaultObserverConfig must take both paths from the canonical
// resolvers, because a caller-supplied lock path lets two observers each
// believe they are the singleton.
func TestObserverPathsHaveNoCallerOverride(t *testing.T) {
	t.Setenv("HERD_STATE_DIR", t.TempDir())
	cfg := DefaultObserverConfig()
	if ObserverLockID() == cfg.LockPath {
		t.Fatalf("the published lock id %q is the runtime path; it must be logical", ObserverLockID())
	}
	if filepath.IsAbs(ObserverLockID()) {
		t.Fatalf("the published lock id %q is absolute", ObserverLockID())
	}
	if cfg.LockPath != ObserverLockPath() {
		t.Fatalf("default lock path %q is not the canonical %q", cfg.LockPath, ObserverLockPath())
	}
	if cfg.StatusPath != ObserverStatusPath() {
		t.Fatalf("default status path %q is not the canonical %q", cfg.StatusPath, ObserverStatusPath())
	}
}

// TestObserverStatusPathCannotBeTheGuardReport is a blunt guard on the rule
// that the observer publishes its own artifact and overwrites nobody.
func TestObserverStatusPathCannotBeTheGuardReport(t *testing.T) {
	t.Setenv("HERD_STATE_DIR", t.TempDir())
	status := ObserverStatusPath()
	for _, guardArtifact := range []string{"latest.json", "status.json", "recent-samples.jsonl", "pane-ownership.json", "incidents.md"} {
		if filepath.Base(status) == guardArtifact {
			t.Fatalf("observer status path %q is the operational guard's %q", status, guardArtifact)
		}
	}
	if filepath.Base(status) != observerStatusName {
		t.Fatalf("observer status file is %q, expected %q", filepath.Base(status), observerStatusName)
	}
}
