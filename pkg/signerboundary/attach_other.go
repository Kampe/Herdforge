//go:build !unix

package signerboundary

import "fmt"

func tryAttach(pid int) error {
	return fmt.Errorf("%w: attach probe unavailable", ErrUnsupportedPlatform)
}

func processUID(pid int) (int, bool) { return 0, false }

func peerPIDOfSocket(socketPath string) int { return 0 }
