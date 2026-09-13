//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package mail

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// OpenRegularStore opens path for reading and refuses anything that is not a
// regular file, WITHOUT ever blocking on the open itself.
//
// The ordering matters and was wrong twice. Checking the path with os.Stat
// before opening left a window in which the approved regular file was
// replaced. Opening first and checking the handle closed that window but
// opened a worse one: open(2) on a FIFO with no writer BLOCKS, indefinitely,
// before any handle check or context check can run. A bounded read that can
// hang in its own open is not bounded.
//
// O_NONBLOCK makes the open return immediately for a FIFO, which fstat then
// rejects. Regular files ignore O_NONBLOCK for read semantics, so the flag
// costs the supported case nothing and the descriptor is handed back as-is.
func OpenRegularStore(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return nil, errors.Join(fmt.Errorf("mail: stat opened store: %w", statErr), file.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(
			fmt.Errorf("mail: %s is not a regular file; a bounded read cannot bound a stream", "store"),
			file.Close(),
		)
	}
	return file, nil
}
