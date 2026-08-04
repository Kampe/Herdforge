//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package resources

import "fmt"

// OSBackend is unavailable on unsupported platforms; callers still get a
// deterministic fail-closed decision through Evaluate.
type OSBackend struct{}

func (OSBackend) StatFS(string) (Capacity, error) {
	return Capacity{}, fmt.Errorf("statfs unsupported")
}
