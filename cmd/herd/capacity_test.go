package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

// noHerdPATH is /usr/bin:/bin only -- deterministically WITHOUT `herdr` or
// `herdr-route`, wherever they happen to be installed on the host running
// this test. It exists so these subprocess tests exercise the real refusal
// and refresh-failure branches without depending on whether this machine's
// own fleet daemon happens to be up.
func noHerdPATH() string {
	return "/usr/bin:/bin"
}

func TestAdmissionLeaseLockHelper(t *testing.T) {
	if os.Getenv("HERD_ADMISSION_LOCK_HELPER") != "1" {
		return
	}
	path := os.Getenv("HERD_ADMISSION_LOCK_PATH")
	ready := os.Getenv("HERD_ADMISSION_LOCK_READY")
	release := os.Getenv("HERD_ADMISSION_LOCK_RELEASE")
	lock, err := acquireAdmissionLeaseLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ready, []byte("held\n"), 0o600); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(release); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeAdmissionFixture(t *testing.T, path string, r admissionLeaseRecord) {
	t.Helper()
	if err := os.WriteFile(path, []byte(fmt.Sprintf("token=%s pid=%d taken=%s ttl=%s phase=%s\n", r.Token, r.PID, r.Taken.UTC().Format(time.RFC3339Nano), r.TTL, r.Phase)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runCapacitySubprocess runs the real, compiled herd binary rather than
// calling runCapacity in-process. Before FAC-770, the refusal and
// refresh-failure branches called os.Exit directly -- calling them in the
// test's own process would have killed the test binary, not just failed one
// case. The subprocess is what makes the exit code and the post-exit lease
// file state both observable.
func runCapacitySubprocess(t *testing.T, leasePath string, args ...string) (out []byte, exitCode int) {
	t.Helper()
	binary := buildHerd(t)
	cmd := exec.Command(binary, append([]string{"capacity"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+noHerdPATH(), "HERD_ADMISSION_LEASE_PATH="+leasePath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return out, 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode()
	}
	t.Fatalf("herd capacity did not run: %v, output: %s", err, out)
	return nil, -1
}

// FAC-770: the caller's own lease attempt was refused before it held
// anything, so it must not touch the file at all -- least of all a lease
// some other, still-live launcher owns.
func TestCapacityClaimRefusalLeavesForeignLeaseUntouched(t *testing.T) {
	lease := filepath.Join(t.TempDir(), "admission.lease")
	t.Setenv("HERD_ADMISSION_LEASE_PATH", lease)
	releaseForeign, held, err := holdAdmissionLease(time.Hour)
	if err != nil || !held {
		t.Fatalf("foreign holder failed to take the lease: held=%v err=%v", held, err)
	}
	t.Cleanup(releaseForeign)
	before, err := os.ReadFile(lease)
	if err != nil {
		t.Fatalf("foreign lease vanished before the subprocess ran: %v", err)
	}

	_, code := runCapacitySubprocess(t, lease, "--claim", "--json")
	if code != 3 {
		t.Fatalf("claim refusal exit code = %d, want 3", code)
	}
	after, err := os.ReadFile(lease)
	if err != nil {
		t.Fatalf("foreign lease was removed by the refused caller: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("foreign lease token changed after a refused claim:\nbefore=%s\nafter=%s", before, after)
	}
}

// FAC-770 core regression: before the fix, this os.Exit(3) ran before the
// deferred release(), so a claimed lease outlived its own refused launch for
// the full TTL. The subprocess must both exit 3 AND have released the lease
// it took, so the very next caller can proceed instead of serializing behind
// a launch that already gave up.
func TestCapacityClaimNotAdmittedStillReleasesItsOwnLease(t *testing.T) {
	lease := filepath.Join(t.TempDir(), "admission.lease")

	// No herdr on PATH (see noHerdPATH) makes decideCapacity refuse
	// deterministically, on any host, without depending on real memory.
	_, code := runCapacitySubprocess(t, lease, "--claim", "--json")
	if code != 3 {
		t.Fatalf("not-admitted exit code = %d, want 3", code)
	}
	if _, err := os.Stat(lease); !os.IsNotExist(err) {
		t.Fatalf("lease file still present after a refused claim released it: err=%v", err)
	}
}

// Same defect, the other os.Exit site: a refresh-launchable failure inside a
// held claim used to skip the deferred release exactly like the refusal
// above.
func TestCapacityClaimRefreshFailureStillReleasesItsOwnLease(t *testing.T) {
	lease := filepath.Join(t.TempDir(), "admission.lease")

	// No herdr-route on PATH (see noHerdPATH) makes refreshLaunchable fail
	// deterministically, on any host, without a live router.
	_, code := runCapacitySubprocess(t, lease, "--claim", "--refresh-launchable")
	if code != 1 {
		t.Fatalf("refresh-launchable failure exit code = %d, want 1", code)
	}
	if _, err := os.Stat(lease); !os.IsNotExist(err) {
		t.Fatalf("lease file still present after a refresh-launchable failure released it: err=%v", err)
	}
}

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
	release3, held3, err := holdAdmissionLease(time.Minute)
	if err != nil || !held3 {
		t.Fatalf("lease was not reusable after release: held=%v err=%v", held3, err)
	}
	release3()

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
func TestUncappedHarnessWarnsWithoutRefusing(t *testing.T) {
	o := healthy()
	o.HarnessPath, o.HarnessCapped = "/opt/testharness/bin/claude", false
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
	o.HarnessPath, o.HarnessCapped = "/opt/testharness/bin/claude", true
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

// FAC-713, from the FAC-584 review finding on 1d99c2ef6c4f. Reproduced there in
// a disposable clone; pinned here.
//
// release() removed the lease file unconditionally. If holder A's lease expired
// and B reclaimed it, A's deferred release deleted B's lease and a third
// launcher could take it -- THREE concurrent launchers from a gate whose whole
// purpose is to admit one. Worse than no lease, because it reports success
// while serializing nothing.
func TestExpiredHolderReleaseDoesNotEvictTheNewOwner(t *testing.T) {
	t.Setenv("HERD_ADMISSION_LEASE_PATH", filepath.Join(t.TempDir(), "admission.lease"))

	// A takes the lease with a TTL that expires immediately.
	releaseA, heldA, err := holdAdmissionLease(time.Nanosecond)
	if err != nil || !heldA {
		t.Fatalf("A failed to take the lease: held=%v err=%v", heldA, err)
	}
	time.Sleep(2 * time.Millisecond)

	// B reclaims the expired lease. A is still live.
	// B's recorded owner TTL must control later expiry; the reclaimer/caller's
	// TTL is not allowed to shorten a live successor's lease.
	releaseB, heldB, err := holdAdmissionLease(time.Hour)
	if err != nil || !heldB {
		t.Fatalf("B failed to reclaim an expired lease: held=%v err=%v", heldB, err)
	}

	// A finishes and releases. It must NOT remove B's lease.
	releaseA()

	// If A evicted B, a third launcher can now take the lease while B is live.
	_, heldC, err := holdAdmissionLease(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if heldC {
		t.Fatal("a third launcher was admitted: an expired holder's release evicted the live owner")
	}
	releaseB()
}

func TestReleaseByTheRealOwnerStillFreesTheLease(t *testing.T) {
	// The ownership check must not make release a no-op, or the lease leaks and
	// the gate becomes an outage for a full TTL.
	t.Setenv("HERD_ADMISSION_LEASE_PATH", filepath.Join(t.TempDir(), "admission.lease"))

	release, held, err := holdAdmissionLease(time.Hour)
	if err != nil || !held {
		t.Fatalf("first claim failed: held=%v err=%v", held, err)
	}
	release()
	if _, held2, err := holdAdmissionLease(time.Hour); err != nil || !held2 {
		t.Fatalf("lease was not freed by its real owner: held=%v err=%v", held2, err)
	}
}

func TestDefaultLeaseTTLExceedsMeasuredRouteResolution(t *testing.T) {
	// Route resolution was measured at 29-272s. A TTL shorter than its critical
	// section is not a short lease, it is no lease.
	if defaultAdmissionLeaseTTL <= 272*time.Second {
		t.Fatalf("default TTL %s does not exceed measured route resolution (272s); the lease can expire mid-launch",
			defaultAdmissionLeaseTTL)
	}
}

func TestAdmissionLeaseReclaimsDeadPreProbeHolderWithinRecordedTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.lease")
	t.Setenv("HERD_ADMISSION_LEASE_PATH", path)
	writeAdmissionFixture(t, path, admissionLeaseRecord{Token: "dead-token", PID: 999999, Taken: time.Now().Add(-time.Minute), TTL: time.Hour, Phase: admissionPhaseCandidate})
	release, held, err := holdAdmissionLeaseWithHooks(time.Minute, admissionLeaseHooks{processAlive: func(int) admissionLeaseLiveness { return admissionDead }})
	if err != nil || !held {
		t.Fatalf("dead candidate was not reclaimed: held=%v err=%v", held, err)
	}
	release()
	if _, err := os.Stat(admissionLeaseLockPath(path)); err != nil {
		t.Fatalf("permanent lock sidecar missing: %v", err)
	}
}

func TestAdmissionLeaseDoesNotEarlyReclaimLiveUnknownOrInFlight(t *testing.T) {
	for _, tt := range []struct {
		name  string
		phase admissionLeasePhase
		live  admissionLeaseLiveness
	}{
		{"live candidate", admissionPhaseCandidate, admissionLive},
		{"unknown candidate", admissionPhaseCandidate, admissionUnknown},
		{"dead route", admissionPhaseRoute, admissionDead},
		{"dead probe", admissionPhaseProbe, admissionDead},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "admission.lease")
			t.Setenv("HERD_ADMISSION_LEASE_PATH", path)
			writeAdmissionFixture(t, path, admissionLeaseRecord{Token: "protected", PID: 4321, Taken: time.Now(), TTL: time.Hour, Phase: tt.phase})
			_, held, err := holdAdmissionLeaseWithHooks(time.Nanosecond, admissionLeaseHooks{processAlive: func(int) admissionLeaseLiveness { return tt.live }})
			if err != nil {
				t.Fatal(err)
			}
			if held {
				t.Fatal("protected lease was reclaimed by an unproven-dead or in-flight holder")
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(raw), "token=protected ") {
				t.Fatalf("protected lease changed: %s", raw)
			}
		})
	}
}

func TestAdmissionLeaseExpiryUsesRecordedOwnerTTL(t *testing.T) {
	for _, tt := range []struct {
		name       string
		ttl        time.Duration
		age        time.Duration
		expectHeld bool
	}{
		{"long owner ttl remains held", 1200 * time.Second, 700 * time.Second, false},
		{"short owner ttl expires", 60 * time.Second, 120 * time.Second, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "admission.lease")
			t.Setenv("HERD_ADMISSION_LEASE_PATH", path)
			writeAdmissionFixture(t, path, admissionLeaseRecord{Token: "owner", PID: 4321, Taken: time.Now().Add(-tt.age), TTL: tt.ttl, Phase: admissionPhaseRoute})
			release, held, err := holdAdmissionLeaseWithHooks(600*time.Second, admissionLeaseHooks{processAlive: func(int) admissionLeaseLiveness { return admissionUnknown }})
			if err != nil {
				t.Fatal(err)
			}
			if held != tt.expectHeld {
				t.Fatalf("held=%v, want %v", held, tt.expectHeld)
			}
			if held {
				release()
			}
		})
	}
}

func TestAdmissionLeaseLegacyNoPhaseUsesRecordedOwnerTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.lease")
	t.Setenv("HERD_ADMISSION_LEASE_PATH", path)
	writeAdmissionFixture(t, path, admissionLeaseRecord{Token: "legacy", PID: 4321, Taken: time.Now(), TTL: time.Hour})
	_, held, err := holdAdmissionLeaseWithHooks(time.Nanosecond, admissionLeaseHooks{processAlive: func(int) admissionLeaseLiveness { return admissionDead }})
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("fresh legacy lease without a phase was reclaimed early")
	}

	writeAdmissionFixture(t, path, admissionLeaseRecord{Token: "legacy-expired", PID: 4321, Taken: time.Now().Add(-time.Hour), TTL: 10 * time.Minute})
	release, held, err := holdAdmissionLeaseWithHooks(600*time.Second, admissionLeaseHooks{processAlive: func(int) admissionLeaseLiveness { return admissionUnknown }})
	if err != nil || !held {
		t.Fatalf("expired readable legacy lease was not reclaimed: held=%v err=%v", held, err)
	}
	release()
}

func TestAdmissionLeaseReleaseLeavesOnlyPermanentSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.lease")
	t.Setenv("HERD_ADMISSION_LEASE_PATH", path)
	release, held, err := holdAdmissionLease(time.Hour)
	if err != nil || !held {
		t.Fatalf("acquire failed: held=%v err=%v", held, err)
	}
	release()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "admission.lease.lock" {
		t.Fatalf("lease directory contains unexpected files: %v", entries)
	}
}

func TestAdmissionLeaseStatusRedactsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.lease")
	t.Setenv("HERD_ADMISSION_LEASE_PATH", path)
	writeAdmissionFixture(t, path, admissionLeaseRecord{Token: "super-secret-token", PID: os.Getpid(), Taken: time.Now(), TTL: time.Hour, Phase: admissionPhaseCandidate})
	old := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	err = emitAdmissionLeaseStatus()
	_ = write.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(read)
	if strings.Contains(string(out), "super-secret-token") {
		t.Fatalf("status exposed raw token: %s", out)
	}
	if !strings.Contains(string(out), "token_digest") || !strings.Contains(string(out), "candidate") {
		t.Fatalf("status omitted digest or phase: %s", out)
	}
}

func TestAdmissionLeasePhaseAndReleaseRefuseSuccessor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.lease")
	t.Setenv("HERD_ADMISSION_LEASE_PATH", path)
	release, held, err := holdAdmissionLease(time.Hour)
	if err != nil || !held {
		t.Fatal(err)
	}
	r, _, err := readAdmissionLease(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateAdmissionLeasePhase(path, r.Token, admissionPhaseRoute); err != nil {
		t.Fatal(err)
	}
	writeAdmissionFixture(t, path, admissionLeaseRecord{Token: "successor", PID: os.Getpid(), Taken: time.Now(), TTL: time.Hour, Phase: admissionPhaseRoute})
	release()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "token=successor ") {
		t.Fatalf("stale release removed successor: %s", raw)
	}
	if err := updateAdmissionLeasePhase(path, r.Token, admissionPhaseSpawn); err == nil {
		t.Fatal("stale phase update modified successor")
	}
}

func TestAdmissionLeaseReclaimRejectsStaleObservationABA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.lease")
	t.Setenv("HERD_ADMISSION_LEASE_PATH", path)
	writeAdmissionFixture(t, path, admissionLeaseRecord{Token: "old", PID: 4321, Taken: time.Now().Add(-time.Minute), TTL: time.Hour, Phase: admissionPhaseCandidate})
	_, held, err := holdAdmissionLeaseWithHooks(time.Minute, admissionLeaseHooks{
		processAlive: func(int) admissionLeaseLiveness { return admissionDead },
		beforeReplace: func() {
			writeAdmissionFixture(t, path, admissionLeaseRecord{Token: "successor", PID: os.Getpid(), Taken: time.Now(), TTL: time.Hour, Phase: admissionPhaseRoute})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("stale reclaim observation admitted over a successor")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "token=successor ") {
		t.Fatalf("successor was not preserved: %s", raw)
	}
}

func TestAdmissionLeaseLockIsCrossProcessAndSidecarIsPermanent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.lease")
	ready, release := filepath.Join(dir, "ready"), filepath.Join(dir, "release")
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Other CLI tests legitimately redirect os.Args[0] while exercising a
	// compiled herd binary. The child must use the test process identity, not
	// that mutable display/argv alias.
	originalArg0 := os.Args[0]
	os.Args[0] = "herd"
	t.Cleanup(func() { os.Args[0] = originalArg0 })
	cmd := exec.Command(testExecutable, "-test.run=^TestAdmissionLeaseLockHelper$")
	cmd.Env = append(os.Environ(), "HERD_ADMISSION_LOCK_HELPER=1", "HERD_ADMISSION_LOCK_PATH="+path, "HERD_ADMISSION_LOCK_READY="+ready, "HERD_ADMISSION_LOCK_RELEASE="+release)
	var childOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &childOut, &childOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		_ = cmd.Wait()
	}()
	// This is a test-only child-start budget, not an admission deadline. Full
	// shuffled unit runs can contend for four Go workers while starting the
	// already-built test binary; one second was shorter than that startup path
	// on Linux. Ten seconds remains bounded, and a child that cannot publish its
	// marker still fails with its captured diagnostic.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock helper did not acquire lock within test startup budget: %s", childOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Setenv("HERD_ADMISSION_LEASE_PATH", path)
	if _, _, err := holdAdmissionLease(time.Hour); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("held cross-process lock did not refuse with timeout: %v", err)
	}
	if _, err := os.Stat(admissionLeaseLockPath(path)); err != nil {
		t.Fatalf("sidecar vanished: %v", err)
	}
}
