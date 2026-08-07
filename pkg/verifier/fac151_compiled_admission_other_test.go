//go:build fac151_hermetic_integration && !linux

package verifier

import "errors"

func fac151TestMainAdmission() error { return compiledFAC151Admission() }

func compiledFAC151Admission() error {
	return errors.New("FAC-151 hermetic admission requires the fixed linux container runner")
}
