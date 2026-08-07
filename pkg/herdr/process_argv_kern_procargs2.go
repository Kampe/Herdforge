package herdr

import (
	"encoding/binary"
	"fmt"
)

// parseKERNProcargs2 parses a Darwin KERN_PROCARGS2 buffer.
// Layout (native little-endian):
//
//	int32 argc
//	executable path (NUL-terminated C string)
//	NUL padding
//	argv[0..argc) each as a NUL-terminated C string
//	env and Apple strings follow and are ignored
//
// Pure and platform-neutral so synthetic buffers can be unit-tested everywhere.
func parseKERNProcargs2(data []byte) ([]string, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("procargs2 buffer too short: %d", len(data))
	}
	argc := int(int32(binary.LittleEndian.Uint32(data[:4])))
	if argc <= 0 || argc > 4096 {
		return nil, fmt.Errorf("procargs2 argc invalid: %d", argc)
	}

	i := 4
	// Executable path.
	for i < len(data) && data[i] != 0 {
		i++
	}
	if i >= len(data) {
		return nil, fmt.Errorf("procargs2 executable path unterminated")
	}
	i++ // skip path NUL

	// NUL padding between path and argv[0].
	for i < len(data) && data[i] == 0 {
		i++
	}
	if i >= len(data) {
		return nil, fmt.Errorf("procargs2 missing argv")
	}

	argv := make([]string, 0, argc)
	for a := 0; a < argc; a++ {
		if i >= len(data) {
			return nil, fmt.Errorf("procargs2 argv truncated at %d/%d", a, argc)
		}
		start := i
		for i < len(data) && data[i] != 0 {
			i++
		}
		if i >= len(data) {
			return nil, fmt.Errorf("procargs2 argv[%d] unterminated", a)
		}
		argv = append(argv, string(data[start:i]))
		i++ // skip arg NUL
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("procargs2 empty argv")
	}
	if argv[0] == "" {
		return nil, fmt.Errorf("procargs2 argv[0] is empty")
	}
	return argv, nil
}
