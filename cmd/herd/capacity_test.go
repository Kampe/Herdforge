package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// FAC-686: process counts are RECORDED but must not gate. Nobody has
// established the threshold where the host actually breaks, and inventing one
// would repeat the memory gate's mistake -- guarding a number because it was
// easy to read rather than because it was the one that mattered.
func TestCapacityDoesNotRefuseOnProcessCountsYet(t *testing.T) {
	o := healthy()
	o.Processes, o.Threads, o.FDLimit = 100000, 400000, 9223372036854775
	if c := decideCapacity(o, 4, 512, 2048); !c.Admit {
		t.Fatalf("process counts became a refusal without an established threshold: %s", c.Reason)
	}
}

func TestAdmissionLeaseIsExclusiveThenReclaimableAfterExpiry(t *testing.T) {
	// The launch storm: four preflights each ran against a census that did not
	// yet contain the other three, so all four saw room and all four passed.
	t.Setenv("HERD_ADMISSION_LEASE_PATH", filepath.Join(t.TempDir(), "admission.lease"))

	release, held, err := holdAdmissionLease(time.Minute)
	if err != nil || !held {
		t.Fatalf("first claim failed: held=%v err=%v", held, err)
	}
	if _, held2, err := holdAdmissionLease(time.Minute); err != nil || held2 {
		t.Fatalf("a second concurrent launch was admitted alongside the first: held=%v err=%v", held2, err)
	}
	release()
	if _, held3, err := holdAdmissionLease(time.Minute); err != nil || !held3 {
		t.Fatalf("lease was not reusable after release: held=%v err=%v", held3, err)
	}

	// A launch killed mid-flight must not fence the host forever: that turns a
	// crash into an outage.
	if _, held4, err := holdAdmissionLease(0); err != nil || !held4 {
		t.Fatalf("an expired lease was not reclaimable: held=%v err=%v", held4, err)
	}
}

// FAC-686: swap-in-use is an EARLIER signal than free bytes. W4 held 5GB of
// swap with active writeback while the Normal zone sat at its minimum watermark
// and MemAvailable still looked survivable. Fragmentation is invisible to
// MemAvailable; swapping is not.
func TestCapacityRefusesAHostThatHasStartedSwapping(t *testing.T) {
	o := healthy()
	o.MemAvailMiB = 30000 // plenty by the free-bytes test alone
	o.SwapUsedMiB = 5120
	c := decideCapacity(o, 4, 4096, 6144)
	if c.Admit {
		t.Fatal("admitted a reviewer onto a host that was already swapping")
	}
	if !strings.Contains(c.Reason, "swapping") {
		t.Fatalf("refusal does not name swap as the cause: %s", c.Reason)
	}
}

func TestIncidentalSwapDoesNotRefuse(t *testing.T) {
	// A host that reclaimed a few pages is not degrading. Refusing at one byte
	// of swap would fence healthy hosts permanently.
	o := healthy()
	o.SwapUsedMiB = 64
	if c := decideCapacity(o, 4, 4096, 6144); !c.Admit {
		t.Fatalf("incidental swap use refused a healthy host: %s", c.Reason)
	}
}

// The number the gate is built on. Sizing a reviewer at 512MiB (agent RSS)
// instead of its real cost admitted four launches heading for 41GB.
func TestReviewerIsSizedByItsToolchainNotItsAgentRSS(t *testing.T) {
	o := healthy()
	o.MemAvailMiB = 8000 // room for four 512MiB agents; NOT for one real review
	c := decideCapacity(o, 4, 4096, 6144)
	if c.Admit {
		t.Fatalf("admitted a review that does not fit once its toolchain is counted: %s", c.Reason)
	}
}

// FAC-686: the slot ceiling must be arithmetic on the host's real memory. A
// hardcoded 4 is the 512MiB mistake in a different variable -- a number that
// looks like policy and is actually a hope.
func TestDerivedReviewLimitScalesWithHostMemory(t *testing.T) {
	// 48GiB VM at 50% budget, 4GiB per reviewer -> 6 slots.
	if got := derivedReviewLimit(48*1024, 4096); got != 6 {
		t.Fatalf("48GiB host derived %d slots, want 6", got)
	}
	// Same host capped to 32GiB by .wslconfig -> 4 slots, no code change.
	if got := derivedReviewLimit(32*1024, 4096); got != 4 {
		t.Fatalf("32GiB host derived %d slots, want 4", got)
	}
	// The incident ran to ~41GiB of 48GiB. The derived budget must sit well
	// below that, or it authorises the thing that broke the host.
	if budget := int64(6) * 4096; budget >= 41*1024 {
		t.Fatalf("derived budget %dMiB reaches the level that failed", budget)
	}
}

func TestDerivedReviewLimitTreatsUnknownMemoryAsOneNotUnlimited(t *testing.T) {
	for _, total := range []int64{-1, 0} {
		if got := derivedReviewLimit(total, 4096); got != 1 {
			t.Fatalf("unmeasurable memory derived %d slots; unknown must not read as unlimited", got)
		}
	}
}

func TestDerivedReviewLimitNeverReturnsZero(t *testing.T) {
	// A host smaller than one reviewer still makes progress with one, behind
	// the memory and swap gates. Zero would fence it permanently.
	if got := derivedReviewLimit(1024, 4096); got != 1 {
		t.Fatalf("tiny host derived %d slots, want 1", got)
	}
}
