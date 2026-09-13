//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package mail

import (
	"errors"
	"os"
)

// OpenRegularStore is unsupported here, deliberately and explicitly.
//
// Bounding a read requires opening a store without the open itself being able
// to block on a FIFO, which needs a non-blocking open this platform does not
// expose through the standard library. Degrading to a plain os.Open would
// reinstate exactly the hang this function exists to prevent, so the bounded
// path refuses rather than pretending to be bounded. The unbounded legacy
// read is unaffected.
func OpenRegularStore(path string) (*os.File, error) {
	return nil, errors.New("mail: bounded reads require a platform with non-blocking open; use the unflagged read here")
}
