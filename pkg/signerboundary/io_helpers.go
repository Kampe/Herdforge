package signerboundary

import (
	"io"
	"os"
)

func ioReadAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func ioReadAllClose(f *os.File) ([]byte, error) {
	defer f.Close()
	return io.ReadAll(f)
}

// ensureSignerFDHygiene documents that the server opens keys with CLOEXEC and
// never passes key FDs over IPC (pre-opened FD inheritance mitigation).
func ensureSignerFDHygiene() {}
