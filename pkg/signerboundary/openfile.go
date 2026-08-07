package signerboundary

import (
	"os"
	"syscall"
)

func openFileRO(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_CLOEXEC, 0)
}
