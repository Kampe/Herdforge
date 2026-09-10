//go:build fac151_hermetic_integration

package verifier

import (
	"errors"
	"os"
	"path/filepath"
)

// The FAC-151 binary never runs mutation admission. Keep only the exact
// compile-time contract needed by the shared verifier implementation; the
// real resource gate remains the normal-build implementation above.
type DiskRequest struct {
	Operation, Path, TempPath string
}

type DiskDecision struct {
	State    string `json:"state"`
	Allowed  bool   `json:"allowed"`
	Evidence any    `json:"evidence"`
}

type DiskAdmission interface {
	Admit(DiskRequest) DiskDecision
}

func defaultDiskAdmission() DiskAdmission { return nil }

func resolveExistingPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	clean := filepath.Clean(path)
	if _, err := os.Stat(clean); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(clean)
}
