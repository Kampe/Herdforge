package security

// FAC-169 dependency (hard blocker for live admission).
//
// FAC-170 must NOT implement separate-UID / ptrace / proc-mem / task_for_pid
// theater. After FAC-169 merges to main, FAC-170 rebases and sets
// RequireOSBoundary to the merged signerboundary (or successor) API.
//
// Until then, live author sessions fail closed with code fac169_required.

// OSBoundary is the post-merge FAC-169 surface HostCreds will consume.
// Defined here only as a dependency contract — not an implementation.
type OSBoundary interface {
	// Mechanism is the OS authority class (e.g. separate-uid from FAC-169).
	Mechanism() string
	// ProbeDigest is the live adversarial probe digest from FAC-169.
	ProbeDigest() string
	// AdversarialProbe re-runs worker-side denial checks (FAC-169).
	AdversarialProbe() error
}

// RequireOSBoundary is the sole OS-boundary gate for live HostCreds.
// Default: BLOCKED awaiting FAC-169 merge. Production wiring after rebase:
//
//	security.RequireOSBoundary = func() (security.OSBoundary, error) {
//	    // call merged FAC-169 API, return adapter
//	}
var RequireOSBoundary = func() (OSBoundary, error) {
	return nil, &BlockedError{
		Reason: BlockSecretExposure,
		Code:   "fac169_required",
	}
}

// SetRequireOSBoundaryForTest overrides the gate in unit tests that must not
// pull FAC-169. Production live path never uses a same-UID fake boundary.
func SetRequireOSBoundaryForTest(fn func() (OSBoundary, error)) (restore func()) {
	prev := RequireOSBoundary
	if fn == nil {
		RequireOSBoundary = func() (OSBoundary, error) {
			return nil, &BlockedError{Reason: BlockSecretExposure, Code: "fac169_required"}
		}
	} else {
		RequireOSBoundary = fn
	}
	return func() { RequireOSBoundary = prev }
}
