//go:build !darwin && !linux

package godepscheck

import (
	"fmt"
	"os"
	"runtime"
)

func openPTY() (*os.File, *os.File, error) {
	return nil, nil, fmt.Errorf("openpty is not available on %s", runtime.GOOS)
}
