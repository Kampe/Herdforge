//go:build !fac151_hermetic_integration

package verifier

import "github.com/Kampe/Herdforge/pkg/resources"

func setHermeticVerifierResources() func() {
	return resources.SetOSBackendStatFSForTest(resources.HermeticStatFSForTest)
}
