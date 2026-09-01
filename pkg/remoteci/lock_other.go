//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package remoteci

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

func acquireLedgerFileLock(context.Context, *os.File) error {
	return fmt.Errorf("cross-process remote-CI ledger locking is unavailable on %s", runtime.GOOS)
}

func releaseLedgerFileLock(*os.File) error { return nil }
