package attention

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

// FAC-593: every non-denial from CheckLaneAndTaskHold was stamped "hold
// authority unavailable", so an ambiguous active-card binding -- which names
// the candidate refs and the remedy -- was reported to the operator as an
// infrastructure failure. Eight lanes read as broken authority when the actual
// fault was board hygiene, and the two have different owners and different
// fixes.
func TestAmbiguousBindingIsNotLabelledAuthorityUnavailable(t *testing.T) {
	err := fmt.Errorf("%w: lane=api-crusader has 2 active tasks: CHA-1784, CHA-1863", lifecycle.ErrActiveTaskUnknown)
	got := degradedReason(err)
	if strings.Contains(got, "hold authority unavailable") {
		t.Fatalf("ambiguous binding mislabelled as an authority failure: %s", got)
	}
	for _, want := range []string{"CHA-1784", "CHA-1863", "api-crusader"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reason does not name %q: %s", want, got)
		}
	}
}

func TestGenuineAuthorityFailureKeepsItsLabel(t *testing.T) {
	got := degradedReason(errors.New("database is locked"))
	if !strings.HasPrefix(got, "hold authority unavailable: ") {
		t.Fatalf("authority failure lost its label: %s", got)
	}
}
