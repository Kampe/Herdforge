package security

import (
	"runtime"
)

// hostCredsGOOS is the platform the broker gate judges. Overridable in tests
// only — the unsupported branch is otherwise unreachable on any host that can
// build this package, so its fail-closed behaviour would go unproven.
var hostCredsGOOS = runtime.GOOS

// platformSupportsHostCredsBroker reports whether the HostCreds oracle can run.
// Unix domain sockets work on unix-like systems. Others fail closed.
func platformSupportsHostCredsBroker() error {
	return platformSupportsHostCredsBrokerFor(hostCredsGOOS)
}

func platformSupportsHostCredsBrokerFor(goos string) error {
	switch goos {
	case "darwin", "linux", "freebsd", "openbsd", "netbsd":
		return nil
	default:
		return &BlockedError{
			Reason: BlockUnsupportedPlat,
			Code:   "goos:" + goos,
		}
	}
}

// PlatformHostCredsStatus returns a redacted readiness string for diagnostics.
func PlatformHostCredsStatus() (supported bool, reason string) {
	if err := platformSupportsHostCredsBroker(); err != nil {
		if be, ok := err.(*BlockedError); ok {
			return false, string(be.Reason) + ":" + be.Code
		}
		return false, "unsupported"
	}
	return true, "supported"
}
