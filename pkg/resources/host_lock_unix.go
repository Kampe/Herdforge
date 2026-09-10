//go:build darwin || linux || freebsd || netbsd || openbsd

package resources

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type FileLockProvider struct{}

func (FileLockProvider) Acquire(ctx context.Context, path string, timeout, retry time.Duration) (io.Closer, error) {
	if timeout <= 0 || retry <= 0 || retry > timeout {
		return nil, errors.New("invalid host lock bounds")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create host lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open host lock: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return &fileLock{file: file}, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) {
			file.Close()
			return nil, fmt.Errorf("acquire host lock: %w", err)
		}
		if time.Now().Add(retry).After(deadline) {
			file.Close()
			return nil, fmt.Errorf("not acquired within %s", timeout)
		}
		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

type fileLock struct {
	file *os.File
}

func (l *fileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return errors.Join(err, l.file.Close())
}
