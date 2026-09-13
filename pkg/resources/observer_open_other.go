//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package resources

import (
	"errors"
	"os"
)

// observerReadSupported says whether this platform has a bounded-open primitive
// for the status file. Tests that exercise the read path narrow their
// expectations with it rather than skipping wholesale.
const observerReadSupported = false

// ErrObserverReadUnsupported is returned on platforms with no bounded open.
var ErrObserverReadUnsupported = errors.New("resources: reading the observer status is unsupported on this platform")

// openRegularNonBlocking refuses without touching the filesystem.
//
// There is no O_NONBLOCK here, and no precheck substitutes for one: an Lstat
// that reports a regular file can be followed by the path being replaced with a
// FIFO, and the open then blocks before any descriptor exists to validate.
// Check-then-open cannot close that window, so the only honest answer is to
// refuse the read until a bounded primitive exists for this platform.
func openRegularNonBlocking(string) (*os.File, error) {
	return nil, ErrObserverReadUnsupported
}
