//go:build darwin && !cgo

package verifier

import "time"

func processesHoldingMarkerUntil(markerPath string, deadline time.Time) ([]procToken, error) {
	return processesHoldingMarkerViaLsof(markerPath, deadline)
}
