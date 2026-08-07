//go:build fac151_hermetic_integration

package verifier

// Compatibility symbol for the preserved, denied native bodies. Ordinary
// production execution uses ownedSubprocess.reapFn and cannot race this shim.
var ownedTreeReapFn = (*ownedSubprocess).killTracked
