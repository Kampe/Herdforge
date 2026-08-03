package security

import (
	"fmt"
	"runtime"
)

// platformSupportsHostCredsBroker reports whether the HostCreds localhost
// broker can run on this OS. Loopback TCP proxy works on unix-like systems
// used by Herdforge. Windows and unknown platforms fail closed.
func platformSupportsHostCredsBroker() error {
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "openbsd", "netbsd":
		return nil
	default:
		return &BlockedError{
			Reason: BlockUnsupportedPlat,
			Detail: fmt.Sprintf("HostCreds broker unsupported on GOOS=%s (fail-closed)", runtime.GOOS),
		}
	}
}

// PlatformHostCredsStatus returns a redacted readiness string for diagnostics.
func PlatformHostCredsStatus() (supported bool, reason string) {
	if err := platformSupportsHostCredsBroker(); err != nil {
		if be, ok := err.(*BlockedError); ok {
			return false, string(be.Reason) + ": " + be.Detail
		}
		return false, err.Error()
	}
	return true, "supported"
}
