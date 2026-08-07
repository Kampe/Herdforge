//go:build !linux && !darwin

package signerboundary

import "fmt"

func tryProcMemRead(pid int) error {
	return fmt.Errorf("%w: proc-mem unavailable", ErrUnsupportedPlatform)
}

func tryProcessVMRead(pid int) error {
	return fmt.Errorf("%w: process vm read unavailable", ErrUnsupportedPlatform)
}

func isProcMemUnavailable(err error) bool {
	return true
}
