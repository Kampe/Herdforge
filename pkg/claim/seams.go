package claim

import (
	"sync/atomic"
	"time"
)

// TestSeams are process-local overrides for crash/clock/TTL proofs.
// Production binaries never call InstallTestSeams; compiled tests that
// need abrupt death or short TTLs install seams via the herd_test_seams
// build tag (or in-process test helpers), not ambient production env.
type TestSeams struct {
	LeaseTTL            time.Duration
	ProviderLockTimeout time.Duration
	// ProviderLockStale shortens PeekStaleProviderLock window (default 5m).
	ProviderLockStale time.Duration
	// CrashAt is invoked with "before-remote" / "after-remote" from
	// provider.AuthBroker when wired; nil means no crash.
	CrashAt func(phase string)
}

var installedSeams atomic.Pointer[TestSeams]

// InstallTestSeams replaces process-local test seams. Pass nil to clear.
// Not for production call sites.
func InstallTestSeams(s *TestSeams) {
	installedSeams.Store(s)
}

// CurrentTestSeams returns installed seams or nil.
func CurrentTestSeams() *TestSeams {
	return installedSeams.Load()
}

func effectiveProviderLockStaleAfter() time.Duration {
	if s := CurrentTestSeams(); s != nil && s.ProviderLockStale > 0 {
		return s.ProviderLockStale
	}
	return providerLockStaleAfter
}
