//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package resources

import (
	"context"
	"fmt"
	"io"
	"time"
)

type FileLockProvider struct{}

func (FileLockProvider) Acquire(context.Context, string, time.Duration, time.Duration) (io.Closer, error) {
	return nil, fmt.Errorf("host resource lock unsupported")
}
