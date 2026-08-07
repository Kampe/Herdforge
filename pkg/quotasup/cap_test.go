package quotasup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// fakeNow is the supervisor's clock in every test below. Nothing here reads
// wall time: a boundary suite that drifts with the calendar proves nothing.
var fakeNow = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func healthy(class usage.BurnClass, used float64) *usage.BurnState {
	return &usage.BurnState{Class: class, Used: used, Reason: "ok", Window: "5h", Available: true}
}

func obs(b *usage.BurnState) Observation {
	return Observation{Surface: Surface{Provider: "claude", Pool: "default"}, Burn: b, SourceAt: fakeNow}
}

// Quota nobody refreshed is not evidence that headroom still exists.
func TestObservationAgeBoundaryFlipsToUnknown(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		want Capacity
	}{
		{"fresh", 0, Healthy},
		{"one second inside", DefaultMaxObservationAge - time.Second, Healthy},
		{"exactly at the limit", DefaultMaxObservationAge, Healthy},
		{"one nanosecond past", DefaultMaxObservationAge + time.Nanosecond, Unknown},
		{"long past", 2 * DefaultMaxObservationAge, Unknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := obs(healthy(usage.BurnOnpace, 40))
			o.SourceAt = fakeNow.Add(-c.age)
			e := o.Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
			if e.Capacity != c.want {
				t.Fatalf("age %s graded %q, want %q", c.age, e.Capacity, c.want)
			}
			if c.want == Unknown {
				if cap, _ := TargetCap(e); cap != 0 {
					t.Fatalf("UNKNOWN quota authorized %d concurrent", cap)
				}
			}
		})
	}
}

// A reading with no provenance is the same fail-closed case as an old one.
func TestMissingSourceTimestampIsUnknown(t *testing.T) {
	o := obs(healthy(usage.BurnUnderspent, 5))
	o.SourceAt = time.Time{}
	e := o.Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
	if e.Capacity != Unknown || !e.Stale {
		t.Fatalf("missing source timestamp graded %q (stale=%v), want unknown/stale", e.Capacity, e.Stale)
	}
	if cap, why := TargetCap(e); cap != 0 || !strings.Contains(why, "UNKNOWN") {
		t.Fatalf("cap=%d why=%q, want 0 and an UNKNOWN reason", cap, why)
	}
}

// A caller must not be able to switch the freshness gate off by leaving the
// field at its zero value.
func TestZeroMaxAgeFallsBackToTheDefaultRatherThanNoLimit(t *testing.T) {
	o := obs(healthy(usage.BurnOnpace, 40))
	o.SourceAt = fakeNow.Add(-time.Hour)
	if e := o.Grade(fakeNow, DefaultWarnRunwayMinutes, 0); e.Capacity != Unknown {
		t.Fatalf("maxAge 0 graded an hour-old reading %q, want unknown", e.Capacity)
	}
}

// Clock skew must not manufacture freshness or a negative age.
func TestFutureReadingIsNotExtraFresh(t *testing.T) {
	o := obs(healthy(usage.BurnOnpace, 40))
	o.SourceAt = fakeNow.Add(time.Hour)
	e := o.Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
	if e.AgeSeconds != 0 {
		t.Fatalf("future reading aged %ds, want 0", e.AgeSeconds)
	}
	if e.Capacity != Healthy {
		t.Fatalf("future reading graded %q", e.Capacity)
	}
}

func TestTargetCapPerCapacity(t *testing.T) {
	cases := []struct {
		name string
		ev   Evidence
		want int
	}{
		{"exhausted", Evidence{Capacity: Exhausted}, 0},
		{"unknown", Evidence{Capacity: Unknown}, 0},
		{"untracked", Evidence{Capacity: Untracked}, 0},
		{"at risk", Evidence{Capacity: AtRisk}, 1},
		{"healthy overpace", Evidence{Capacity: Healthy, BurnClass: usage.BurnOverpace}, 1},
		{"healthy onpace", Evidence{Capacity: Healthy, BurnClass: usage.BurnOnpace}, 2},
		{"healthy underspent", Evidence{Capacity: Healthy, BurnClass: usage.BurnUnderspent}, 3},
		// A pool we cannot pace is worth a trickle, not the middle of the range.
		{"healthy but unpaceable", Evidence{Capacity: Healthy, BurnClass: usage.BurnUntracked}, 1},
		// A cool outranks every quota reading, however healthy.
		{"cooling despite healthy quota", Evidence{
			Capacity: Healthy, BurnClass: usage.BurnUnderspent, Cooldown: "manual hold"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := TargetCap(c.ev)
			if got != c.want {
				t.Fatalf("TargetCap = %d, want %d (%s)", got, c.want, why)
			}
			if strings.TrimSpace(why) == "" {
				t.Fatal("a cap with no stated evidence is not auditable")
			}
		})
	}
}

// Scale down before exhaustion; scale back up only after verified recovery.
func TestCapDropsAtOnceAndClimbsOneStepPerVerifiedObservation(t *testing.T) {
	s := Surface{Provider: "claude", Pool: "default"}
	exhausted := obs(&usage.BurnState{Class: usage.BurnExhausted, Used: 96, Reason: "exhausted"}).
		Grade(fakeNow, DefaultWarnRunwayMinutes, 0)

	// A surface at full tilt is cut to zero in a single observation.
	d := Decide(s, exhausted, State{Cap: 3, Streak: 7})
	if d.Cap != 0 || d.Posture != PostureBlocked {
		t.Fatalf("exhausted surface = cap %d posture %s, want 0/blocked", d.Cap, d.Posture)
	}
	if d.Streak != 0 {
		t.Fatalf("exhaustion left a healthy streak of %d", d.Streak)
	}

	// Recovery is not one good reading. Quota is now underspent (target 3).
	recovered := obs(healthy(usage.BurnUnderspent, 4)).Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
	want := []struct{ cap, streak int }{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {3, 5}}
	st := d.State()
	for i, w := range want {
		next := Decide(s, recovered, st)
		if next.Cap != w.cap || next.Streak != w.streak {
			t.Fatalf("observation %d: cap=%d streak=%d, want cap=%d streak=%d (%s)",
				i+1, next.Cap, next.Streak, w.cap, w.streak, next.Reason)
		}
		st = next.State()
	}
}

// A single healthy sample between two bad ones must not reset the ratchet.
func TestOneGoodReadingBetweenFailuresDoesNotRaiseTheCap(t *testing.T) {
	s := Surface{Provider: "codex", Pool: "spark"}
	bad := obs(&usage.BurnState{Class: usage.BurnExhausted, Used: 99, Reason: "exhausted"}).
		Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
	good := obs(healthy(usage.BurnUnderspent, 3)).Grade(fakeNow, DefaultWarnRunwayMinutes, 0)

	st := Decide(s, bad, State{Cap: 2, Streak: 4}).State()
	st = Decide(s, good, st).State()
	if st.Cap != 0 {
		t.Fatalf("cap rose to %d on the first healthy reading after exhaustion", st.Cap)
	}
	st = Decide(s, bad, st).State()
	if st.Cap != 0 || st.Streak != 0 {
		t.Fatalf("relapse left cap=%d streak=%d", st.Cap, st.Streak)
	}
}

// Same evidence plus same persisted state must always produce the same cap:
// that equality IS restart safety, and it is what PriorState round-trips.
func TestDecisionIsDeterministicAndSurvivesAStateRoundTrip(t *testing.T) {
	s := Surface{Provider: "antigravity", Pool: "gemini"}
	e := obs(healthy(usage.BurnOnpace, 55)).Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
	prior := State{Cap: 1, Streak: 3}

	first := Decide(s, e, prior)
	for i := 0; i < 20; i++ {
		if again := Decide(s, e, prior); again.Cap != first.Cap || again.Posture != first.Posture ||
			again.Reason != first.Reason || again.Streak != first.Streak {
			t.Fatalf("run %d diverged: %+v vs %+v", i, again, first)
		}
	}

	// Persist the way the CLI does, reload, and decide again.
	body, err := json.Marshal(&Snapshot{Decisions: []Decision{first}})
	if err != nil {
		t.Fatal(err)
	}
	var reloaded Snapshot
	if err := json.Unmarshal(body, &reloaded); err != nil {
		t.Fatal(err)
	}
	if got := PriorState(&reloaded, s); got != first.State() {
		t.Fatalf("state after restart = %+v, want %+v", got, first.State())
	}
	if after := Decide(s, e, PriorState(&reloaded, s)); after.Cap != Decide(s, e, first.State()).Cap {
		t.Fatal("a restart changed the cap the same evidence produces")
	}
}

// No cap may exceed MaxCap or move up by more than one step, whatever a
// corrupted or hand-edited state file claims.
func TestCapsAreBoundedAgainstAbsurdPriorState(t *testing.T) {
	s := Surface{Provider: "claude", Pool: "default"}
	e := obs(healthy(usage.BurnUnderspent, 2)).Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
	for _, prior := range []State{{Cap: 99, Streak: 99}, {Cap: -7, Streak: 50}, {Cap: MaxCap + 1, Streak: 2}} {
		d := Decide(s, e, prior)
		if d.Cap < 0 || d.Cap > MaxCap {
			t.Fatalf("prior %+v produced out-of-range cap %d", prior, d.Cap)
		}
		if sane := clampCap(prior.Cap); d.Cap > sane+MaxRisePerObservation {
			t.Fatalf("prior %+v jumped %d->%d, more than one step", prior, sane, d.Cap)
		}
	}
}

func TestPostureAndAdmission(t *testing.T) {
	s := Surface{Provider: "claude", Pool: "default"}
	atRisk := obs(&usage.BurnState{
		Class: usage.BurnOverpace, Used: 80, Reason: "ok", Window: "5h",
		ExhaustsBeforeReset: ptrB(true), RunwayMinutes: ptrI(30),
	}).Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
	if d := Decide(s, atRisk, State{Cap: 3, Streak: 5}); d.Posture != PostureDrain || d.Cap != 1 {
		t.Fatalf("at-risk = %s cap %d, want drain/1", d.Posture, d.Cap)
	}

	// Admission compares live use against the cap; a full surface admits
	// nothing even while open.
	full := obs(healthy(usage.BurnOnpace, 50))
	full.Active = 2
	d := Decide(s, full.Grade(fakeNow, DefaultWarnRunwayMinutes, 0), State{Cap: 2, Streak: 9})
	if d.Cap != 2 || d.Posture != PostureOpen {
		t.Fatalf("healthy onpace = %s cap %d", d.Posture, d.Cap)
	}
	if d.Admits() {
		t.Fatal("a surface running at its cap must not admit more work")
	}
	room := obs(healthy(usage.BurnOnpace, 50))
	room.Active = 1
	if !Decide(s, room.Grade(fakeNow, DefaultWarnRunwayMinutes, 0), State{Cap: 2, Streak: 9}).Admits() {
		t.Fatal("a surface below its cap must admit work")
	}
	blocked := Decide(s, Evidence{Capacity: Exhausted}, State{})
	if blocked.Admits() {
		t.Fatal("a blocked surface admits nothing")
	}
}

// Every decision has to name the evidence behind it.
func TestReasonNamesTheEvidenceUsed(t *testing.T) {
	s := Surface{Provider: "codex", Pool: "spark"}
	o := obs(&usage.BurnState{Class: usage.BurnExhausted, Used: 97, Reason: "exhausted", Window: "weekly"})
	o.Surface, o.Active = s, 2
	d := Decide(s, o.Grade(fakeNow, DefaultWarnRunwayMinutes, 0), State{Cap: 3, Streak: 4})
	for _, want := range []string{"codex/spark", "blocked", "cap=0", "97%", "weekly", "2 active",
		fakeNow.Format(time.RFC3339)} {
		if !strings.Contains(d.Reason, want) {
			t.Fatalf("reason %q omits %q", d.Reason, want)
		}
	}
}

// ---- surface resolution -------------------------------------------------

// The whole point of keying caps on (provider, pool): an exhausted pool must
// not take an independently metered sibling down with it.
func TestExhaustedPoolLeavesItsHealthySiblingOpen(t *testing.T) {
	computed := map[string]usage.BurnState{
		"antigravity": {Reason: "ok", Pools: map[string]usage.BurnState{
			"gemini":    {Class: usage.BurnExhausted, Used: 98, Reason: "exhausted", Window: "5h"},
			"nonGemini": {Class: usage.BurnUnderspent, Used: 6, Reason: "ok", Window: "5h"},
		}},
	}
	surfaces := Surfaces(computed, nil)
	if len(surfaces) != 2 {
		t.Fatalf("surfaces = %v, want the two agy pools", surfaces)
	}
	caps := map[string]Decision{}
	for _, s := range surfaces {
		e := Observation{Surface: s, Burn: BurnFor(computed, s), SourceAt: fakeNow}.
			Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
		// Give both a long clean history so only the evidence differs.
		caps[s.String()] = Decide(s, e, State{Cap: 3, Streak: 9})
	}
	if d := caps["antigravity/gemini"]; d.Cap != 0 || d.Posture != PostureBlocked {
		t.Fatalf("exhausted gemini pool = cap %d posture %s", d.Cap, d.Posture)
	}
	if d := caps["antigravity/nonGemini"]; d.Cap != 3 || d.Posture != PostureOpen {
		t.Fatalf("healthy sibling was disabled: cap %d posture %s — %s", d.Cap, d.Posture, d.Reason)
	}
}

// A pool with no row of its own must not inherit a busy sibling's burn.
func TestPoolWithoutItsOwnLedgerRowIsUntrackedNotAggregated(t *testing.T) {
	computed := map[string]usage.BurnState{
		"antigravity": {
			Class: usage.BurnUnderspent, Used: 3, Reason: "ok",
			Pools: map[string]usage.BurnState{
				"nonGemini": {Class: usage.BurnUnderspent, Used: 3, Reason: "ok", Window: "5h"},
			},
		},
		// A provider the ledger does not split meters everything through its
		// single top-level row, which IS that pool's row.
		"grok": {Class: usage.BurnOnpace, Used: 44, Reason: "ok", Window: "5h"},
	}
	gemini := Surface{Provider: "antigravity", Pool: "gemini"}
	if BurnFor(computed, gemini) != nil {
		t.Fatal("a pool with no row inherited the provider aggregate")
	}
	e := Observation{Surface: gemini, SourceAt: fakeNow}.Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
	if e.Capacity != Untracked {
		t.Fatalf("rowless pool graded %q, want untracked", e.Capacity)
	}
	if cap, _ := TargetCap(e); cap != 0 {
		t.Fatalf("untracked pool authorized %d concurrent", cap)
	}
	if BurnFor(computed, Surface{Provider: "grok", Pool: "default"}) == nil {
		t.Fatal("an unsplit provider's own row must serve its default pool")
	}
	if BurnFor(computed, Surface{Provider: "nope", Pool: "default"}) != nil {
		t.Fatal("an absent provider must not resolve a row")
	}
}

// A lane running on a surface the ledger never mentions has to be graded, not
// quietly skipped.
func TestSurfacesIncludeLiveSurfacesTheLedgerOmits(t *testing.T) {
	computed := map[string]usage.BurnState{"claude": {Reason: "ok"}}
	live := []Surface{{Provider: "kimi", Pool: "default"}, {Provider: "claude", Pool: "default"}}
	got := Surfaces(computed, live)
	want := []Surface{{Provider: "claude", Pool: "default"}, {Provider: "kimi", Pool: "default"}}
	if len(got) != len(want) {
		t.Fatalf("surfaces = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] { // sorted, so this also pins the deterministic order
			t.Fatalf("surfaces = %v, want %v", got, want)
		}
	}
}

func TestPriorStateColdStartsBlocked(t *testing.T) {
	s := Surface{Provider: "claude", Pool: "default"}
	if got := PriorState(nil, s); got != (State{}) {
		t.Fatalf("no snapshot = %+v, want the blocked zero value", got)
	}
	if got := PriorState(&Snapshot{}, s); got != (State{}) {
		t.Fatalf("no decision for the surface = %+v", got)
	}
}

// ---- act ----------------------------------------------------------------

func TestActWritesPoolScopedCoolsAndNeverAProviderWideOne(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", dir)
	blocked := Decision{
		Surface: Surface{Provider: "antigravity", Pool: "gemini"},
		Cap:     0, Posture: PostureBlocked, Reason: "exhausted (98% used, window 5h)",
	}
	open := Decision{
		Surface: Surface{Provider: "antigravity", Pool: "nonGemini"},
		Cap:     3, Posture: PostureOpen, Reason: "healthy",
	}
	if _, err := Act(fakeNow, MaxActCooldown, []Decision{blocked, open}); err != nil {
		t.Fatal(err)
	}

	// The ledger name is "antigravity"; the router queries "agy". A cool
	// filed under the wrong name gates nothing.
	if _, err := os.Stat(CooldownPath(dir, "agy", "gemini")); err != nil {
		t.Fatalf("gemini pool was not cooled under the router's provider name: %v", err)
	}
	if _, err := os.Stat(CooldownPath(dir, "agy", "nonGemini")); !os.IsNotExist(err) {
		t.Fatal("a healthy sibling pool was cooled")
	}
	if _, err := os.Stat(filepath.Join(dir, "agy.cooldown.json")); !os.IsNotExist(err) {
		t.Fatal("a provider-wide cool would take every sibling pool down with it")
	}
}

func TestActClampsCooldownLifetime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", dir)
	d := Decision{Surface: Surface{Provider: "claude", Pool: "fable"}, Posture: PostureBlocked, Reason: "exhausted"}
	for _, c := range []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"below the floor", time.Second, MinActCooldown},
		{"above the ceiling", 30 * 24 * time.Hour, MaxActCooldown},
		{"in range", 10 * time.Minute, 10 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Act(fakeNow, c.ttl, []Decision{d}); err != nil {
				t.Fatal(err)
			}
			var e struct {
				ExpiresAt int64  `json:"expiresAt"`
				Source    string `json:"source"`
			}
			raw, err := os.ReadFile(CooldownPath(dir, "claude", "fable"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatal(err)
			}
			if got := time.Unix(e.ExpiresAt, 0).UTC().Sub(fakeNow); got != c.want {
				t.Fatalf("ttl %s wrote a %s cool, want %s", c.ttl, got, c.want)
			}
			if e.Source != ActSource {
				t.Fatalf("source = %q, want %q", e.Source, ActSource)
			}
		})
	}
}

func TestActLiftsOnlyItsOwnCools(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mine := CooldownPath(dir, "claude", "default")
	theirs := CooldownPath(dir, "claude", "fable")
	write := func(path, source string) {
		body, _ := json.Marshal(cooldownEntry{
			Provider: "claude", ExpiresAt: fakeNow.Add(time.Hour).Unix(),
			Reason: "hold", Source: source,
		})
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(mine, ActSource)
	write(theirs, "") // a human's manual hold

	changes, err := Act(fakeNow, MaxActCooldown, []Decision{
		{Surface: Surface{Provider: "claude", Pool: "default"}, Cap: 2, Posture: PostureOpen, Reason: "healthy"},
		{Surface: Surface{Provider: "claude", Pool: "fable"}, Cap: 2, Posture: PostureOpen, Reason: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Fatal("the supervisor did not lift its own cool")
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Fatal("the supervisor cleared a hold it did not write")
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %v, want exactly the one lift", changes)
	}
}

// One ledger can meter several router provider names; a cool has to cover all
// of them or launches keep landing on the pool through an alias.
func TestActCoolsEveryRouteProviderMeteredByTheLedger(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", dir)
	d := Decision{Surface: Surface{Provider: "opencode", Pool: "default"}, Posture: PostureBlocked, Reason: "exhausted"}
	if _, err := Act(fakeNow, MaxActCooldown, []Decision{d}); err != nil {
		t.Fatal(err)
	}
	for _, rp := range []string{"opencode", "ollama", "lazer"} {
		if _, err := os.Stat(CooldownPath(dir, rp, "default")); err != nil {
			t.Fatalf("%s meters through the opencode ledger but was not cooled: %v", rp, err)
		}
	}
}

func TestRouteProvidersMapLedgerNamesToRouterNames(t *testing.T) {
	cases := map[string][]string{
		"antigravity": {"agy"},
		"opencode":    {"opencode", "ollama", "lazer"},
		"claude":      {"claude"},
		"codex":       {"codex"},
	}
	for ledger, want := range cases {
		got := RouteProviders(ledger)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("RouteProviders(%q) = %v, want %v", ledger, got, want)
		}
	}
}

// --act writes into the same store the supervisor reads. Its own entry is a
// consequence of a past decision, not independent evidence: counting it would
// build a latch that no amount of recovered quota can open.
func TestSupervisorDoesNotReadBackItsOwnCoolAsEvidence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", dir)
	s := Surface{Provider: "claude", Pool: "fable"}

	blocked := Decision{Surface: s, Cap: 0, Posture: PostureBlocked, Reason: "exhausted (97% used)"}
	if _, err := Act(fakeNow, MaxActCooldown, []Decision{blocked}); err != nil {
		t.Fatal(err)
	}
	if got := SurfaceCooldown(fakeNow, s); got != "" {
		t.Fatalf("the supervisor read its own cool back as evidence: %q", got)
	}

	// A hold it did not write is real evidence and must gate.
	body, _ := json.Marshal(cooldownEntry{
		Provider: "claude", Pool: "default",
		ExpiresAt: fakeNow.Add(time.Hour).Unix(), Reason: "manual hold",
	})
	if err := os.WriteFile(CooldownPath(dir, "claude", "default"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := SurfaceCooldown(fakeNow, Surface{Provider: "claude", Pool: "default"}); got != "manual hold" {
		t.Fatalf("foreign hold = %q, want it to gate", got)
	}
}

// The full loop the latch would break: exhausted -> cooled -> quota recovers
// -> cap climbs back under the verified-recovery rule -> cool lifted.
func TestBlockedSurfaceRecoversThroughActAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", dir)
	s := Surface{Provider: "claude", Pool: "fable"}
	computed := map[string]usage.BurnState{"claude": {Reason: "ok", Pools: map[string]usage.BurnState{
		"fable": {Class: usage.BurnExhausted, Used: 97, Reason: "exhausted", Window: "5h"},
	}}}

	run := func(now time.Time, prior *Snapshot) *Snapshot {
		e := Observation{
			Surface: s, Burn: BurnFor(computed, s), SourceAt: now,
			Cooldown: SurfaceCooldown(now, s),
		}.Grade(now, DefaultWarnRunwayMinutes, 0)
		next := &Snapshot{Decisions: []Decision{Decide(s, e, PriorState(prior, s))}}
		if _, err := Act(now, MaxActCooldown, next.Decisions); err != nil {
			t.Fatal(err)
		}
		return next
	}

	at := fakeNow
	state := run(at, &Snapshot{Decisions: []Decision{{Surface: s, Cap: 3, Streak: 9}}})
	if state.Decisions[0].Cap != 0 {
		t.Fatalf("exhausted surface kept cap %d", state.Decisions[0].Cap)
	}
	if _, err := os.Stat(CooldownPath(dir, "claude", "fable")); err != nil {
		t.Fatalf("blocked surface was not cooled: %v", err)
	}

	// Quota recovers. Each run advances the clock so no reading goes stale.
	computed["claude"].Pools["fable"] = usage.BurnState{
		Class: usage.BurnUnderspent, Used: 5, Reason: "ok", Window: "5h",
	}
	wantCaps := []int{0, 1, 2, 3}
	for i, want := range wantCaps {
		at = at.Add(time.Minute)
		state = run(at, state)
		if got := state.Decisions[0].Cap; got != want {
			t.Fatalf("run %d after recovery: cap %d, want %d (%s)", i+1, got, want, state.Decisions[0].Reason)
		}
	}
	if _, err := os.Stat(CooldownPath(dir, "claude", "fable")); !os.IsNotExist(err) {
		t.Fatal("a fully recovered surface is still cooled")
	}
}

// A human hold sits on the SAME file the supervisor would write to block. Two
// ordinary ticks must not be able to destroy it: tick one would restamp it as
// ours (shortening a 6h hold to our clamped ttl), and tick two would lift it,
// because the supervisor skips its own cools when grading.
func TestActNeverRestampsOrLiftsALiveForeignHold(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", dir)
	s := Surface{Provider: "claude", Pool: "fable"}
	path := CooldownPath(dir, "claude", "fable")

	body, _ := json.Marshal(map[string]any{
		"provider": "claude", "pool": "fable",
		"expiresAt": fakeNow.Add(6 * time.Hour).Unix(),
		"reason":    "manual hold: incident 4471",
	})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	// Tick 1: the hold makes the surface blocked; Act enforces the block.
	e1 := Observation{Surface: s, SourceAt: fakeNow, Cooldown: SurfaceCooldown(fakeNow, s)}.
		Grade(fakeNow, DefaultWarnRunwayMinutes, 0)
	d1 := Decide(s, e1, State{Cap: 2, Streak: 5})
	if d1.Posture != PostureBlocked {
		t.Fatalf("held surface posture = %s", d1.Posture)
	}
	if _, err := Act(fakeNow, MaxActCooldown, []Decision{d1}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("hold file gone after tick 1: %v", err)
	}
	var got struct {
		Reason    string `json:"reason"`
		Source    string `json:"source"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Reason != "manual hold: incident 4471" || got.Source != "" {
		t.Fatalf("tick 1 rewrote the human hold: %+v", got)
	}

	// Tick 2: if tick 1 had stamped it as ours, this lifts it entirely.
	later := fakeNow.Add(time.Minute)
	e2 := Observation{Surface: s, SourceAt: later, Cooldown: SurfaceCooldown(later, s)}.
		Grade(later, DefaultWarnRunwayMinutes, 0)
	if _, err := Act(later, MaxActCooldown, []Decision{Decide(s, e2, d1.State())}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the supervisor destroyed a live human hold: %v", err)
	}
}

// The block path must also refuse to overwrite a foreign hold, and must still
// write when the file at that path is NOT actually gating the surface —
// deferring to an inert file would leave an exhausted surface open.
func TestActBlockPathDefersToLiveHoldsOnlyNotInertFiles(t *testing.T) {
	blocked := Decision{
		Surface: Surface{Provider: "claude", Pool: "fable"},
		Posture: PostureBlocked, Reason: "exhausted",
	}
	cases := []struct {
		name       string
		entry      map[string]any
		wantOurs   bool
		wantChange string
	}{
		{
			name: "live foreign hold is left alone",
			entry: map[string]any{"provider": "claude", "pool": "fable",
				"expiresAt": fakeNow.Add(time.Hour).Unix(), "reason": "manual hold"},
			wantOurs: false, wantChange: "deferred",
		},
		{
			// Expired: the router already ignores it, so it gates nothing.
			name: "expired foreign entry is replaced",
			entry: map[string]any{"provider": "claude", "pool": "fable",
				"expiresAt": fakeNow.Add(-time.Hour).Unix(), "reason": "old hold"},
			wantOurs: true, wantChange: "cooled",
		},
		{
			// Scoped to another provider: never gated this surface.
			name: "foreign entry for a different provider is replaced",
			entry: map[string]any{"provider": "grok", "pool": "fable",
				"expiresAt": fakeNow.Add(time.Hour).Unix(), "reason": "grok hold"},
			wantOurs: true, wantChange: "cooled",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HERDR_ROUTE_STATE_DIR", dir)
			path := CooldownPath(dir, "claude", "fable")
			body, _ := json.Marshal(c.entry)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			changes, err := Act(fakeNow, MaxActCooldown, []Decision{blocked})
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != 1 || !strings.HasPrefix(changes[0], c.wantChange) {
				t.Fatalf("changes = %v, want one %q line", changes, c.wantChange)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Source string `json:"source"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if ours := got.Source == ActSource; ours != c.wantOurs {
				t.Fatalf("entry after Act = %+v; ours=%v want ours=%v", got, ours, c.wantOurs)
			}
			// Whatever happened, the surface must end up gated.
			if router.CooldownFor(fakeNow, "claude", "", "fable") == nil {
				t.Fatal("blocked surface left ungated")
			}
		})
	}
}
