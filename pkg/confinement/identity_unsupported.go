//go:build !darwin && !linux

package confinement

import "os"

func platformIdentity(os.FileInfo) (fileIdentity, error) {
	return fileIdentity{}, ErrUnsupportedIdentity
}

func platformNlink(os.FileInfo) uint64 { return 0 }
