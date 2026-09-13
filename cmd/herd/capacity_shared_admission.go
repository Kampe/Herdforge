//go:build !herdfixture

package main

import (
	"context"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// withSharedAdmission attaches the shared resource decision LAST, after the
// herdr, memory and process probes have already run.
//
// Order matters: taken first, the decision would be minutes old by the time the
// slow probes finished, and capacity would gate on a host state that had moved.
// decideCapacity revalidates it again at its own boundary, so a decision that
// ages out between here and there refuses rather than admitting on history.
//
// This is the SHIPPED definition. Its fixture twin exists only under the
// herdfixture build tag, so the released binary carries no seam at all.
func withSharedAdmission(o CapacityObservation) CapacityObservation {
	admission := resources.Admit(context.Background())
	o.admission = &admission
	// DecidedAt is deliberately LEFT ZERO here: decideCapacity stamps its own
	// boundary. Copying the observation time would make every revalidation
	// trivially fresh, which is the check this is meant to perform.
	return o
}
