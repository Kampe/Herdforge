//go:build !linux && !darwin

package verifier

import (
	"fmt"
	"time"
)

// processesHoldingMarker is unsupported: fail closed so we never claim
// marker lineage residual ownership is complete.
func processesHoldingMarkerUntil(markerPath string, _ time.Time) ([]procToken, error) {
	return nil, fmt.Errorf("processesHoldingMarker: unsupported platform for marker %q", markerPath)
}
