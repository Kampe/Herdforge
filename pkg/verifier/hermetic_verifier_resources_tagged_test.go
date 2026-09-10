//go:build fac151_hermetic_integration

package verifier

func setHermeticVerifierResources() func() { return func() {} }
