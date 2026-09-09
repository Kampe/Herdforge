//go:build !darwin && !linux

package usage

import (
	"errors"
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
		if !errors.Is(err, os.ErrExist) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
