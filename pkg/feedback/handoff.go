package feedback

import "github.com/Kampe/Herdforge/pkg/standing"

// HandoffObservation and HandoffTracker are exposed at the feedback boundary
// because census/status consumers must apply the same progress semantics.
type HandoffObservation = standing.HandoffObservation
type HandoffTracker = standing.HandoffTracker

func NewHandoffTracker() *HandoffTracker { return standing.NewHandoffTracker() }

func NormalizeHandoff(content string) (string, bool) {
	return standing.NormalizeHandoff(content)
}
