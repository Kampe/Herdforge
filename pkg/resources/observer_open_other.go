//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package resources

import "os"

// openRegularNonBlocking has no O_NONBLOCK to apply on this platform. The
// descriptor-level regular-file check in the caller still applies, so a
// non-regular path is refused; only the open itself is unprotected here.
func openRegularNonBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}
