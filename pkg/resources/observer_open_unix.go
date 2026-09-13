//go:build darwin || linux || freebsd || netbsd || openbsd

package resources

import (
	"errors"
	"os"
	"syscall"
)

// observerReadSupported says whether this platform has a bounded-open primitive
// for the status file. It does: O_NONBLOCK returns from the open itself, and
// the caller then validates the mode on the descriptor it actually holds.
const observerReadSupported = true

// ErrObserverReadUnsupported exists on every platform so consumers can test for
// it uniformly. It is never returned here.
var ErrObserverReadUnsupported = errors.New("resources: reading the observer status is unsupported on this platform")

// openRegularNonBlocking opens path without blocking on a FIFO or device.
//
// A plain open of a FIFO with no writer blocks indefinitely, which would hang
// the reader before any size or mode check could run. O_NONBLOCK makes the open
// itself return, and the caller then verifies the mode on the resulting
// descriptor rather than on a path that may have been replaced since.
func openRegularNonBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
