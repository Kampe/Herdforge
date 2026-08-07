//go:build !unix

package signerboundary

import (
	"fmt"
	"os"
)

func openKeyVerified(path string, wantUID int) (*os.File, error) {
	return nil, fmt.Errorf("%w: openKeyVerified unsupported", ErrUnsupportedPlatform)
}
