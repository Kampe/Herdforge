//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package resources

import "fmt"

type OSPhysicalMeasurer struct{}

func (OSPhysicalMeasurer) Measure(string, int) (PhysicalUsage, error) {
	return PhysicalUsage{}, fmt.Errorf("physical-byte accounting unsupported")
}
