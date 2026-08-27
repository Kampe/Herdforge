package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

func healthy() CapacityObservation {
	return CapacityObservation{HerdrRunning: true, AgentsListed: true, MemAvailMiB: 36000, SwapUsedMiB: 0, SwapTotalMiB: 8192, PressurePct: 0.2}
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

// FAC-688: a blocked reviewer holds a slot and makes no progress. Counting it
// only in the live total hides the accumulation that preceded the incident --
// W4 came back with three reviewers of which two were blocked, and the record
// reported that as indistinguishable from three healthy reviews.
func TestBlockedReviewersAreNamedWhenTheyHoldTheCap(t *testing.T) {
	o := healthy()
	o.Reviewers, o.ReviewersBlocked = 5, 2
	o.BlockedReviewerID = []string{"review-security-cha-320", "review-review-cha2191-c"}
	c := decideCapacity(o, 5, 4096, 6144)
	if c.Admit {
		t.Fatal("admitted past the cap")
	}
	if !strings.Contains(c.Reason, "BLOCKED") || !strings.Contains(c.Reason, "review-security-cha-320") {
		t.Fatalf("refusal hides which slots are stalled: %s", c.Reason)
	}
}

// FAC-688: an uncapped harness WARNS but must not refuse. Refusing would fence
// every host that has not adopted the wrapper, trading a bounded risk for a
// certain outage -- while the memory, swap and derived-slot gates already bound
// the aggregate. Visible, not fatal.
// FAC-711: this file used a literal user-home path, which made the test file
// itself an absolute-path leak and failed the preflight boundary check inside
// `herd selftest`. The path is only an IDENTIFIER for the harness -- any
// absolute path exercises the logic -- so it now uses a root carrying no leak
// marker.
//
// Worth recording twice over: the first rewrite of this comment still said the
// marker out loud and tripped the same gate again. A test that violates the
// invariant it helps enforce is worse than no test, because it makes the gate
// look broken rather than the code.
func TestUncappedHarnessWarnsWithoutRefusing(t *testing.T) {
	o := healthy()
	o.HarnessPath, o.HarnessCapped = "/opt/harness/bin/claude", false
	c := decideCapacity(o, 5, 4096, 6144)
	if !c.Admit {
		t.Fatalf("an uncapped harness fenced the host: %s", c.Reason)
	}
	if !strings.Contains(c.Reason, "NOT memory-capped") {
		t.Fatalf("an uncapped harness was admitted silently: %s", c.Reason)
	}
}

func TestCappedHarnessAddsNoWarning(t *testing.T) {
	o := healthy()
	o.HarnessPath, o.HarnessCapped = "/opt/harness/bin/claude", true
	if c := decideCapacity(o, 5, 4096, 6144); strings.Contains(c.Reason, "NOT memory-capped") {
		t.Fatalf("a capped harness still warned: %s", c.Reason)
	}
}

func TestAbsentHarnessIsUnmeasuredNotUncapped(t *testing.T) {
	// No harness on PATH is unmeasured. Warning that an absent binary is
	// uncapped is a confident claim about something never observed.
	o := healthy()
	o.HarnessPath, o.HarnessCapped = "", false
	if c := decideCapacity(o, 5, 4096, 6144); strings.Contains(c.Reason, "NOT memory-capped") {
		t.Fatalf("an unmeasured harness produced an uncapped claim: %s", c.Reason)
	}
}

// FAC-690: the memory reading goes through pkg/freshness so an UNKNOWN cannot
// be read as a value by forgetting to check. -1 invites exactly that mistake,
// because -1 IS a value every consumer must remember means "unmeasured".
func TestUnknownMemoryReadingYieldsNotOkNotZero(t *testing.T) {
	var unknown freshness.Reading[hostMemory]
	unknown = freshness.Degrade[hostMemory](unknown, "/proc/meminfo",
		errors.New("absent"), "run on the review host")
	v, ok := unknown.Value()
	if ok {
		t.Fatal("an unmeasured host reported a usable memory value")
	}
	if v.AvailMiB != 0 || v.TotalMiB != 0 {
		t.Fatalf("unknown reading leaked a non-zero value: %+v", v)
	}
	// The whole point: a consumer that ignores ok gets zero, and zero available
	// memory must not silently become a refusal reason with no explanation.
	if !strings.Contains(unknown.MustExplain(time.Now()), "/proc/meminfo") {
		t.Fatalf("unknown reading does not name its source: %s", unknown.MustExplain(time.Now()))
	}
}

func TestFreshMemoryReadingIsUsable(t *testing.T) {
	r := freshness.Fresh("/proc/meminfo", time.Now(), hostMemory{TotalMiB: 48173, AvailMiB: 40658})
	v, ok := r.Value()
	if !ok || v.TotalMiB != 48173 {
		t.Fatalf("a fresh reading was not usable: %+v ok=%v", v, ok)
	}
}

// FAC-693: stale swap is a scar, not a wound. After the incident W4 carried
// 1.7GB of swap while completely idle -- zero paging activity, PSI 0.26%,
// 23.9GB available, 0 reviewers -- and the level-based gate refused every
// launch. Nothing the fleet could do would clear it; only a manual swapoff.
func TestStaleSwapOnAnIdleHostDoesNotRefuse(t *testing.T) {
	o := healthy()
	o.SwapUsedMiB, o.SwapTotalMiB = 1751, 8192 // residue from an earlier incident
	o.PressurePct = 0.26                       // measured: no pressure at all
	c := decideCapacity(o, 3, 4096, 6144)
	if !c.Admit {
		t.Fatalf("a healthy idle host was fenced by swap RESIDUE: %s", c.Reason)
	}
}

func TestRealMemoryPressureRefuses(t *testing.T) {
	// The thing that actually hurts: work stalling on memory right now.
	o := healthy()
	o.PressurePct = 35
	c := decideCapacity(o, 3, 4096, 6144)
	if c.Admit {
		t.Fatal("admitted a reviewer onto a host where work is stalling on memory")
	}
	if !strings.Contains(c.Reason, "PSI") {
		t.Fatalf("refusal does not name the pressure signal: %s", c.Reason)
	}
}

func TestUnavailablePSIDoesNotRefuse(t *testing.T) {
	// A kernel without PSI must not be fenced for lacking an instrument.
	o := healthy()
	o.PressurePct = -1
	if c := decideCapacity(o, 3, 4096, 6144); !c.Admit {
		t.Fatalf("unmeasurable pressure was treated as high pressure: %s", c.Reason)
	}
}

func TestNearlyFullSwapStillRefusesAsABackstop(t *testing.T) {
	// Level is kept as a far backstop: swap genuinely near full is exhaustion,
	// not residue, and there is nowhere left to page into.
	o := healthy()
	o.SwapUsedMiB, o.SwapTotalMiB = 7000, 8192 // ~85%
	c := decideCapacity(o, 3, 4096, 6144)
	if c.Admit {
		t.Fatal("admitted with swap nearly exhausted")
	}
	if !strings.Contains(c.Reason, "consumed") {
		t.Fatalf("refusal does not name swap exhaustion: %s", c.Reason)
	}
}

func TestUnknownSwapTotalCannotTripTheBackstop(t *testing.T) {
	// Never divide by an invented total.
	o := healthy()
	o.SwapUsedMiB, o.SwapTotalMiB = 5000, -1
	if c := decideCapacity(o, 3, 4096, 6144); !c.Admit {
		t.Fatalf("unreadable swap total produced a refusal: %s", c.Reason)
	}
}
