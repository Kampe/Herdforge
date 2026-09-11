//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package resources

import (
	"context"
	"fmt"
)

type OSPhysicalMeasurer struct{}

func (m OSPhysicalMeasurer) Measure(path string, maxEntries int) (PhysicalUsage, error) {
	return m.MeasureContext(context.Background(), path, maxEntries)
}

func (OSPhysicalMeasurer) MeasureContext(context.Context, string, int) (PhysicalUsage, error) {
	return PhysicalUsage{}, fmt.Errorf("physical-byte accounting unsupported")
}
