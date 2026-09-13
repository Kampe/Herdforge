//go:build darwin || linux || freebsd || netbsd || openbsd

package resources

import (
	"os"
	"syscall"
)

// observerReadSupported says whether this platform has a bounded-open primitive
// for the status file. It does: O_NONBLOCK returns from the open itself, and
// the caller then validates the mode on the descriptor it actually holds.
//
// ErrObserverReadUnsupported is declared once in observer.go and is never
// returned here.
const observerReadSupported = true

// openRegularNonBlocking opens path without blocking on a FIFO or device.
//
// A plain open of a FIFO with no writer blocks indefinitely, which would hang
// the reader before any size or mode check could run. O_NONBLOCK makes the open
// itself return, and the caller then verifies the mode on the resulting
// descriptor rather than on a path that may have been replaced since.
func openRegularNonBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
