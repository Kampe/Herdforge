//go:build darwin || linux

package usage

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func withCacheFileLock(path string, wait time.Duration, fn func() error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(wait)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		// A lock someone else holds and a lock that is broken are different
		// answers. Only the first becomes ErrCacheLockBusy; anything else
		// surfaces unwrapped so it cannot be read as ordinary contention.
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s: %w", ErrCacheLockBusy, wait, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}
