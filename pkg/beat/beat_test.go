package beat

import (
	"strings"
	"testing"
	"time"
)

// FAC-705: a supervisor whose beat was "launch this one exact queued review"
// ran a repository-wide `herd drain --json` instead, burned minutes, and had to
// be killed from the coordinator pane. The review was never launched.
func TestBroadScanIsRefusedInsideAnExactBeat(t *testing.T) {
	t.Setenv(ExactBeatEnv, "1")
	err := RefuseBroadScan("herd drain", "herd review <ref> --pool --sha <sha>")
	if err == nil {
		t.Fatal("a broad scan was permitted inside an exact beat")
	}
	// A refusal that does not name the alternative is just a slower stall.
	if !strings.Contains(err.Error(), "herd review") {
		t.Fatalf("refusal names no alternative: %v", err)
	}
}

func TestBroadScanIsPermittedOutsideAnExactBeat(t *testing.T) {
	// Discovery is legitimate when the action is NOT already known. Refusing
	// everywhere would break the exploratory path this command exists for.
	t.Setenv(ExactBeatEnv, "")
	if err := RefuseBroadScan("herd drain", "herd review"); err != nil {
		t.Fatalf("an exploratory scan was refused: %v", err)
	}
}

func TestExplicitlyDisabledBeatDoesNotRefuse(t *testing.T) {
	for _, v := range []string{"0", "false", "FALSE"} {
		t.Setenv(ExactBeatEnv, v)
		if InExactBeat() {
			t.Fatalf("%q was read as an active exact beat", v)
		}
	}
}

// The shape that matters most: an agent occupying a lane while appearing busy.
func TestUnproductiveBeatMustNameABlocker(t *testing.T) {
	_, err := Close("review-harvest", TransitionNone, 90*time.Second, DefaultBudget, "")
	if err == nil {
		t.Fatal("a beat that moved nothing and named no blocker was accepted")
	}
	if !strings.Contains(err.Error(), "cannot be told apart from a hung one") {
		t.Fatalf("rejection does not explain why silence is unacceptable: %v", err)
	}
}

func TestUnproductiveBeatWithABlockerIsAccepted(t *testing.T) {
	// Being blocked is legitimate. Being blocked SILENTLY is not.
	o, err := Close("review-harvest", TransitionNone, 90*time.Second, DefaultBudget, "no queued candidate carries a resolvable base")
	if err != nil {
		t.Fatalf("a blocked beat that stated its reason was rejected: %v", err)
	}
	if o.WithinSLO {
		t.Fatalf("a 90s beat was reported inside a 60s budget: %+v", o)
	}
}

func TestProductiveBeatInsideBudgetIsWithinSLO(t *testing.T) {
	o, err := Close("review-harvest", TransitionLaunch, 12*time.Second, DefaultBudget, "")
	if err != nil {
		t.Fatal(err)
	}
	if !o.WithinSLO || o.Transition != TransitionLaunch {
		t.Fatalf("a fast launch was not recorded as within SLO: %+v", o)
	}
}
