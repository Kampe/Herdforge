//go:build !darwin && !linux

package harvest

import "os"

type runtimeFileMeta struct {
	Owner  uint64
	Links  uint64
	Blocks int64
	ID     string
}

func runtimeFileMetaFromInfo(os.FileInfo) (runtimeFileMeta, bool) {
	return runtimeFileMeta{}, false
}

func runtimeCurrentUID() (uint64, bool) { return 0, false }

func runtimeInstallSupported() bool { return false }
