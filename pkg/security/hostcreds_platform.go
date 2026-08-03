package security

import (
	"runtime"
)

// platformSupportsHostCredsBroker reports whether the HostCreds oracle can run.
// Unix domain sockets work on unix-like systems. Others fail closed.
func platformSupportsHostCredsBroker() error {
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "openbsd", "netbsd":
		return nil
	default:
		return &BlockedError{
			Reason: BlockUnsupportedPlat,
			Code:   "goos:" + runtime.GOOS,
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
