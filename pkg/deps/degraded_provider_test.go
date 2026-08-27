package deps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-707: `herd deps reconcile` timed out identically for CHA-2776, CHA-3192
// and CHA-3176, and was read as three blocked cards. It was ONE degraded
// provider reported three times.
//
// ListProjectRelations is a WHOLE-PROJECT fetch, so its failure is not a
// property of whichever card the caller asked about. A lane that reads it as a
// card blocker advances to the next card, fails again, and burns its entire
// beat on a condition no card can escape.

func TestTimeoutClassificationSurvivesWrapping(t *testing.T) {
	// The scope message is only useful if callers can STILL classify the
	// underlying failure. A wrap that hides the timeout would trade one
	// missing distinction for another.
	inner := fmt.Errorf("kaneo: ListProjectRelations: %w", context.DeadlineExceeded)
	wrapped := fmt.Errorf("deps: PROVIDER DEGRADED, not a card blocker: %w", inner)

	if provider.ClassifyOpError(wrapped) != provider.OpTimeout {
		t.Fatal("wrapping destroyed the timeout classification; callers can no longer recover")
	}
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Fatal("underlying deadline no longer reachable through the wrap")
	}
}

func TestDegradedMessageNamesTheScopeAndTheRemedy(t *testing.T) {
	inner := fmt.Errorf("kaneo: %w", context.DeadlineExceeded)
	msg := fmt.Errorf("deps: PROVIDER DEGRADED, not a card blocker: the project-wide relation fetch timed out, "+
		"so EVERY card reconciles identically until the board responds; preserve the blocker and do not retry other cards for this reason: %w", inner).Error()

	for _, want := range []string{"not a card blocker", "EVERY card", "preserve the blocker"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("degraded message omits %q, so a lane cannot tell this apart from a blocked card: %s", want, msg)
		}
	}
}

func TestNonTimeoutFailuresAreNotClaimedAsDegraded(t *testing.T) {
	// A board REJECTION is a real answer about real state. Labelling it
	// "provider degraded" would tell a lane to stop trying other cards when
	// other cards may be perfectly fine.
	rejected := errors.New("ListProjectRelations: 403 forbidden")
	if provider.ClassifyOpError(rejected) == provider.OpTimeout {
		t.Fatal("a non-timeout failure classified as a timeout; degraded scoping would fire wrongly")
	}
}
