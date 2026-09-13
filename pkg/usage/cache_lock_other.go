//go:build !darwin && !linux

package usage

import (
	"errors"
	"fmt"
	"os"
	"time"
)

func withCacheFileLock(path string, wait time.Duration, fn func() error) error {
	lock := path + ".lock"
	for deadline := time.Now().Add(wait); ; {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			defer os.Remove(lock)
			return fn()
		}
		// Same distinction as the unix twin: only an already-held lock becomes
		// ErrCacheLockBusy, and any other failure surfaces unwrapped.
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s: %w", ErrCacheLockBusy, wait, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
