//go:build !fac151_hermetic_integration

package verifier

import "github.com/Kampe/Herdforge/pkg/resources"

// Keep the public verifier disk-admission contract identical to the resource
// gate in ordinary builds. The FAC-151 tagged compile has a deliberately
// smaller local contract so it does not pull the host-only SQLite census tree.
type DiskAdmission = resources.DiskAdmission
type DiskRequest = resources.DiskRequest

func defaultDiskAdmission() DiskAdmission {
	return resources.NewCapacityGate(resources.OSBackend{}, resources.DefaultDiskPolicy())
}

func resolveExistingPath(path string) (string, error) {
	return resources.ResolveExistingPath(path)
}
