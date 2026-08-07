package herdr

import (
	"bytes"
	"fmt"
)

// parseProcCmdline parses Linux /proc/<pid>/cmdline without erasing empty
// arguments. The kernel appends one terminal NUL; remove exactly that framing
// byte and preserve every remaining NUL-delimited argument.
func parseProcCmdline(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty process cmdline")
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("process cmdline is not NUL-terminated")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	if len(parts) == 0 || len(parts[0]) == 0 {
		return nil, fmt.Errorf("process argv[0] is empty")
	}
	argv := make([]string, len(parts))
	for i := range parts {
		argv[i] = string(parts[i])
	}
	return argv, nil
}
