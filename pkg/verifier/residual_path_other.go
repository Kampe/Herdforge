//go:build !linux && !darwin

package verifier

import "fmt"

// processesTouchingDir is unsupported on this platform: fail closed so we never
// silently claim residual ownership is complete.
func processesTouchingDir(root string) ([]procToken, error) {
	return nil, fmt.Errorf("processesTouchingDir: unsupported platform for candidate %q", root)
}
