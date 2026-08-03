//go:build !linux && !darwin

package verifier

import "fmt"

// processesHoldingMarker is unsupported: fail closed so we never claim
// marker lineage residual ownership is complete.
func processesHoldingMarker(markerPath string) ([]procToken, error) {
	return nil, fmt.Errorf("processesHoldingMarker: unsupported platform for marker %q", markerPath)
}
