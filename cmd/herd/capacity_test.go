package main

import (
	"strings"
	"testing"
)

func healthy() CapacityObservation {
	return CapacityObservation{HerdrRunning: true, AgentsListed: true, MemAvailMiB: 36000, SwapUsedMiB: 0}
}

func TestCapacityRefusesWhenHerdrIsDown(t *testing.T) {
	// The incident itself: the remote review failed because herdr was not
	// running, AFTER a worktree had already been prepared.
	o := healthy()
	o.HerdrRunning = false
	c := decideCapacity(o, 4, 512, 2048)
	if c.Admit {
		t.Fatal("admitted a launch onto a host with no herdr server")
	}
}

func TestCapacityRefusesUnreadableCensusRatherThanCallingItIdle(t *testing.T) {
	o := healthy()
	o.AgentsListed = false
	c := decideCapacity(o, 4, 512, 2048)
	if c.Admit {
		t.Fatal("an unreadable census was treated as an empty fleet")
	}
}

func TestCapacityCapsOnLiveCensusNotConfiguredSlots(t *testing.T) {
	o := healthy()
	o.Reviewers, o.ReviewersIdle = 4, 3
	o.IdleReviewerID = []string{"review-a", "review-b", "review-c"}
	c := decideCapacity(o, 4, 512, 2048)
	if c.Admit {
		t.Fatal("admitted past the concurrency cap")
	}
	// A refusal that does not name the remedy still stops the fleet.
	if !strings.Contains(c.Reason, "idle and reapable") || !strings.Contains(c.Reason, "review-a") {
		t.Fatalf("refusal names no reapable remedy: %s", c.Reason)
	}
}

func TestCapacityRefusesWithoutHeadroomForOneMoreReviewer(t *testing.T) {
	o := healthy()
	o.MemAvailMiB = 2400 // less than 512 + 2048
	c := decideCapacity(o, 4, 512, 2048)
	if c.Admit {
		t.Fatal("admitted a reviewer that does not fit in available memory")
	}
}

func TestCapacityAdmitsWhenMemoryIsUnmeasurable(t *testing.T) {
	// Darwin has no /proc/meminfo. Unknown must not read as a refusal, or the
	// gate becomes an outage on every host it cannot measure.
	o := healthy()
	o.MemAvailMiB, o.SwapUsedMiB = -1, -1
	c := decideCapacity(o, 4, 512, 2048)
	if !c.Admit {
		t.Fatalf("unmeasurable memory refused the launch: %s", c.Reason)
	}
	if !strings.Contains(c.Reason, "unmeasurable") {
		t.Fatalf("admitted without disclosing the unmeasured gate: %s", c.Reason)
	}
}

func TestCapacityAdmitsHealthyHost(t *testing.T) {
	c := decideCapacity(healthy(), 4, 512, 2048)
	if !c.Admit {
		t.Fatalf("healthy host refused: %s", c.Reason)
	}
}

func TestReviewerMatchIsHyphenBounded(t *testing.T) {
	for _, name := range []string{"review-cha-3028", "review"} {
		if !isReviewerAgent(name) {
			t.Fatalf("%s is a reviewer and was not counted against the cap", name)
		}
	}
	for _, name := range []string{"reviewer-tooling", "forge-review-harvest", "builder-x"} {
		if isReviewerAgent(name) {
			t.Fatalf("%s is not a reviewer but consumed a review slot", name)
		}
	}
}
