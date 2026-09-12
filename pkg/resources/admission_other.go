//go:build !darwin && !linux

package resources

import (
	"context"
	"fmt"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

// An unsupported platform reports UNKNOWN and therefore REFUSES.
//
// This is the whole card in one file: the honest answer on a platform nobody
// wrote a probe for is "nothing is known", and nothing known is not headroom.
// The alternative -- what this package did before FAC-826 -- was to return 100%
// free and admit, which is a measurement claim about a machine that was never
// measured.
func observeCPU(ctx context.Context, at time.Time, limits Limits) freshness.Reading[CPULoad] {
	return unknownReading[CPULoad](unsupportedSource, errUnsupportedPlatform, unsupportedRecovery)
}

func observeMemory(ctx context.Context, at time.Time, limits Limits) freshness.Reading[MemHeadroom] {
	return unknownReading[MemHeadroom](unsupportedSource, errUnsupportedPlatform, unsupportedRecovery)
}

const (
	unsupportedSource   = "unsupported platform"
	unsupportedRecovery = "run the fleet on darwin or linux, or add a probe for this platform; admission will refuse until then"
)

var errUnsupportedPlatform = fmt.Errorf("no resource probe is implemented for this platform")
