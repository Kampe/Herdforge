//go:build fac151_hermetic_integration && linux

package verifier

import "errors"

// This legacy admission hook remains denied. The tagged TestMain path uses
// fac151TestMainAdmission, which performs the compiled receipt boundary.
func fac151HermeticAdmission() error {
	return errors.New("FAC-151 native admission remains denied outside compiled TestMain")
}
