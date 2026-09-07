package reviewledger

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockVerdictMutation serializes compare-and-append across ledger instances
// and processes, including aliases of the same ledger inode. Kernel locks
// release on process exit; no env marker skips it.
func lockVerdictMutation(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() error { return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close()) }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return nil, errors.Join(err, f.Close())
		}
		if time.Now().After(deadline) {
			return nil, errors.Join(fmt.Errorf("timed out acquiring verdict mutation lock"), f.Close())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
