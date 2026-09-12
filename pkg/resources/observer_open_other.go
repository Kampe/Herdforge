//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package resources

import (
	"fmt"
	"os"
)

// openRegularNonBlocking refuses a non-regular path BEFORE opening it.
//
// There is no O_NONBLOCK to apply here, and a plain open is not a bounded
// equivalent: on any system that has FIFOs or devices, opening one can block
// indefinitely, and it blocks before the caller ever gets a descriptor to
// inspect. So the mode is checked first and a non-regular path is refused
// outright rather than opened hopefully.
//
// What this buys, stated exactly: the common cases -- the status path IS a
// FIFO, a device, a directory, or a symlink to one -- never reach an open at
// all. What it does not buy: the path could be replaced between the check and
// the open, which is why the caller re-verifies the mode on the descriptor it
// actually holds. That second check is what makes the read safe; this one is
// what keeps it from hanging first.
func openRegularNonBlocking(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to open %s: mode %s is not a regular file, and opening it could block on this platform",
			path, info.Mode())
	}
	return os.OpenFile(path, os.O_RDONLY, 0)
}
