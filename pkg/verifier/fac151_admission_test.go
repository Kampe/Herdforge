//go:build !fac151_hermetic_integration

package verifier

// The ordinary profile has no native fixture admission. Keeping this hook
// explicit makes the denied profile boundary compile-safe without importing
// process-executing native files.
func fac151HermeticAdmission() error { return nil }

func fac151TestMainAdmission() error { return nil }
