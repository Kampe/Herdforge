package spin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// clock is the fake clock every fixture below drives. Nothing in Assess reads
// the wall clock, so cooldowns and windows are exact.
type clock struct{ t time.Time }

func newClock() *clock { return &clock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func obs(status string, p Progress) Observation {
	return Observation{
		PaneID: "p1", Name: "smith", AgentStatus: status, Progress: p,
		PID: 100, ProcAlive: TriYes, UniqueWork: TriNo, RecoveryAvailable: true,
	}
}

func working(seq int64, sha string, scs uint64) Progress {
	return Progress{LifecycleSeq: seq, LifecycleState: "building", CandidateSHA: sha, StateChangeSeq: scs, Head: "h" + sha}
}

// run drives n samples of the same observation and returns the final pair.
func run(t *testing.T, prev *Sample, o Observation, pol Policy, c *clock, act bool, n int) (Sample, Assessment) {
	t.Helper()
	var s Sample
	var a Assessment
	for i := 0; i < n; i++ {
		if prev == nil {
			s, a = Assess(nil, o, pol, c.now(), act)
		} else {
			s, a = Assess(prev, o, pol, c.now(), act)
		}
		prev = &s
		c.advance(DefaultInterval)
	}
	return s, a
}

// The central claim of FAC-90: identical repeated cycles are no-progress,
// and an agent whose durable evidence advances is never touched — however
// long it takes and however still its terminal looks.
func TestRepeatedIdenticalCyclesAreNoProgressButAdvancingWorkIsNot(t *testing.T) {
	pol := DefaultPolicy()

	c := newClock()
	frozen := obs("working", working(7, "abc123def456", 42))
	_, a := run(t, nil, frozen, pol, c, false, pol.NoProgressCycles+1)
	if a.Cause != CauseNoProgress {
		t.Fatalf("repeated identical cycles must read NO_PROGRESS, got %s (%v)", a.Cause, a.Evidence)
	}
	if a.NextAction != ActionNudge {
		t.Fatalf("first no-progress verdict must recommend a nudge, got %s", a.NextAction)
	}

	// Same wall-clock duration, same silent terminal, but the durable
	// evidence moves every cycle. This must never accumulate.
	c = newClock()
	var prev *Sample
	for i := 0; i < 20; i++ {
		s, a := Assess(prev, obs("working", working(int64(7+i), "abc123def456", uint64(42+i))), pol, c.now(), false)
		if a.Cause != CauseProgressing {
			t.Fatalf("cycle %d: advancing evidence must read PROGRESSING, got %s (%v)", i, a.Cause, a.Evidence)
		}
		if a.NextAction != ActionNone {
			t.Fatalf("cycle %d: progressing agent must not be actioned, got %s", i, a.NextAction)
		}
		prev = &s
		c.advance(DefaultInterval)
	}
}

// Each durable signal must independently count as progress; if only one of
// them were wired the detector would be measuring far less than it claims.
func TestEverySignalIndependentlyCountsAsProgress(t *testing.T) {
	pol := DefaultPolicy()
	base := working(7, "abc123def456", 42)
	for name, next := range map[string]Progress{
		"lifecycle_seq":    {LifecycleSeq: 8, LifecycleState: "building", CandidateSHA: "abc123def456", StateChangeSeq: 42, Head: "habc123def456"},
		"lifecycle_state":  {LifecycleSeq: 7, LifecycleState: "verifying", CandidateSHA: "abc123def456", StateChangeSeq: 42, Head: "habc123def456"},
		"candidate_sha":    {LifecycleSeq: 7, LifecycleState: "building", CandidateSHA: "999999999999", StateChangeSeq: 42, Head: "habc123def456"},
		"state_change_seq": {LifecycleSeq: 7, LifecycleState: "building", CandidateSHA: "abc123def456", StateChangeSeq: 43, Head: "habc123def456"},
		"head":             {LifecycleSeq: 7, LifecycleState: "building", CandidateSHA: "abc123def456", StateChangeSeq: 42, Head: "hzzz"},
		"dirty":            {LifecycleSeq: 7, LifecycleState: "building", CandidateSHA: "abc123def456", StateChangeSeq: 42, Head: "habc123def456", Dirty: 1},
	} {
		c := newClock()
		// Accumulate to one sample below the threshold, then move `name`.
		s, _ := run(t, nil, obs("working", base), pol, c, false, pol.NoProgressCycles)
		_, a := Assess(&s, obs("working", next), pol, c.now(), false)
		if a.Cause != CauseProgressing {
			t.Errorf("%s advancing must read PROGRESSING, got %s (%v)", name, a.Cause, a.Evidence)
		}
		if a.NoProgressCycles != 0 {
			t.Errorf("%s advancing must reset the counter, got %d", name, a.NoProgressCycles)
		}
	}
}

func TestSlowWorkIsNotYetNoProgress(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	_, a := run(t, nil, obs("working", working(7, "abc", 42)), pol, c, false, pol.NoProgressCycles)
	if a.Cause != CauseSlowWork {
		t.Fatalf("below threshold must read SLOW_WORK, got %s", a.Cause)
	}
	if a.NextAction != ActionNone {
		t.Fatalf("slow work must not be actioned, got %s", a.NextAction)
	}
}

func TestStatusSeparatesWaitingBlockedAndUnknown(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	for status, want := range map[string]Cause{
		"idle":     CauseAwaitingInput,
		"done":     CauseAwaitingInput,
		"blocked":  CauseBlocked,
		"starting": CauseSlowWork,
		"zombie":   CauseUnknownState,
	} {
		s, _ := run(t, nil, obs(status, working(7, "abc", 42)), pol, c, false, pol.NoProgressCycles)
		_, a := Assess(&s, obs(status, working(7, "abc", 42)), pol, c.now(), true)
		if a.Cause != want {
			t.Errorf("status %q => %s, want %s", status, a.Cause, want)
		}
		if a.Acted {
			t.Errorf("status %q must never be acted on", status)
		}
	}
}

// Provider exhaustion is a wait, not a stall: nudging burns another turn and
// fixes nothing.
func TestQuotaDiagnosticSuppressesTheNudge(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	o := obs("working", working(7, "abc", 42))
	o.Diagnostic = "QUOTA"
	_, a := run(t, nil, o, pol, c, true, pol.NoProgressCycles+2)
	if a.Cause != CauseRateLimited {
		t.Fatalf("quota diagnostic must read RATE_LIMITED, got %s", a.Cause)
	}
	if a.NextAction != ActionNone || a.Acted {
		t.Fatalf("rate-limited agent must not be actioned: %s acted=%v", a.NextAction, a.Acted)
	}
	// Control: the identical fixture without the diagnostic does fire, so
	// this test cannot pass by the assertion never being reachable.
	c = newClock()
	_, a = run(t, nil, obs("working", working(7, "abc", 42)), pol, c, true, pol.NoProgressCycles+2)
	if a.Cause != CauseNoProgress {
		t.Fatalf("control: same fixture without the quota diagnostic must read NO_PROGRESS, got %s", a.Cause)
	}
}

func TestCrashLoopEscalatesStraightToRecovery(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	p := working(7, "abc", 42)
	var prev *Sample
	var a Assessment
	for i := 0; i < pol.RestartCycles+1; i++ {
		o := obs("working", p)
		o.PID = 100 + i // a new process every cycle, same durable evidence
		s, av := Assess(prev, o, pol, c.now(), false)
		prev, a = &s, av
		c.advance(DefaultInterval)
	}
	if a.Cause != CauseCrashLoop {
		t.Fatalf("repeated restarts without progress must read CRASH_LOOP, got %s (%v)", a.Cause, a.Evidence)
	}
	if a.NextAction != ActionRecover {
		t.Fatalf("crash loop next action = %s, want recover", a.NextAction)
	}
}

// FAC-628: a lane spinning in an empty stop-hook loop reports
// agent_status=working in every census while producing nothing — no
// lifecycle movement, no candidate change, no herdr state-change advance, no
// git delta — and the ONLY thing moving is the continuation counter itself.
// That must be caught immediately, not after Policy.NoProgressCycles quiet
// samples: a climbing continuation count with nothing else moving is direct
// proof of the loop, not mere ambiguous silence.
func TestEmptyContinuationLoopIsCaughtImmediatelyNotAsGenericSilence(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	p := working(7, "abc", 42)
	p.Continuations = 305

	first, a1 := Assess(nil, obs("working", p), pol, c.now(), false)
	if a1.Cause != CauseProgressing {
		t.Fatalf("first observation must not be judged yet, got %s", a1.Cause)
	}
	c.advance(DefaultInterval)

	// Second sample: EVERY durable signal unchanged except the continuation
	// counter, which advanced. A generic no-progress verdict would need
	// pol.NoProgressCycles (3) quiet samples to fire; empty-loop must not
	// wait for that.
	p2 := p
	p2.Continuations = 306
	_, a2 := Assess(&first, obs("working", p2), pol, c.now(), false)

	if a2.Cause != CauseEmptyLoop {
		t.Fatalf("continuation advancing with nothing else moving must read EMPTY_LOOP, got %s (%v)", a2.Cause, a2.Evidence)
	}
	if a2.NextAction != ActionRecover {
		t.Fatalf("empty-loop next action = %s, want recover", a2.NextAction)
	}
	found := false
	for _, e := range a2.Evidence {
		if strings.Contains(e, "305->306") || strings.Contains(e, "continuation") {
			found = true
		}
	}
	if !found {
		t.Fatalf("evidence must name the continuation advance, got %v", a2.Evidence)
	}
}

// A durable signal genuinely advancing alongside the continuation counter is
// real work, not an empty loop — CauseEmptyLoop must not fire on it.
func TestContinuationAdvancingWithRealProgressIsNotAnEmptyLoop(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	p := working(7, "abc", 42)
	p.Continuations = 10
	first, _ := Assess(nil, obs("working", p), pol, c.now(), false)
	c.advance(DefaultInterval)

	p2 := working(8, "abc", 42) // lifecycle_seq advanced: real movement
	p2.Continuations = 11
	_, a2 := Assess(&first, obs("working", p2), pol, c.now(), false)
	if a2.Cause != CauseProgressing {
		t.Fatalf("real durable progress alongside a continuation bump must not be EMPTY_LOOP, got %s", a2.Cause)
	}
}

func TestLiveModelDriftIsAHardStopAndNamesBothModels(t *testing.T) {
	p := working(7, "abc", 42)
	_, a := Assess(nil, Observation{PaneID: "p1", Name: "smith", AgentStatus: "working", Progress: p,
		RecordedModel: "grok-4.6", LiveModel: "Grok 4.5 (high)"}, DefaultPolicy(), time.Now(), false)
	if a.Cause != CauseModelDrift || a.NextAction != ActionOperator {
		t.Fatalf("model drift = %s/%s, want hard stop/operator", a.Cause, a.NextAction)
	}
	evidence := strings.Join(a.Evidence, " ")
	if !strings.Contains(evidence, "grok-4.6") || !strings.Contains(evidence, "Grok 4.5") {
		t.Fatalf("model drift evidence must name both values, got %v", a.Evidence)
	}
}

// "Never kill a session or release a lease while unique work or unknown
// state exists" — the two gates, each proven against a passing control.
func TestUniqueWorkAndUnknownProcessStateFailClosed(t *testing.T) {
	pol := DefaultPolicy()

	crashLoop := func(unique Tri, alive Tri) Assessment {
		c := newClock()
		var prev *Sample
		var a Assessment
		for i := 0; i < pol.RestartCycles+1; i++ {
			o := obs("working", working(7, "abc", 42))
			o.PID, o.UniqueWork, o.ProcAlive = 100+i, unique, alive
			s, av := Assess(prev, o, pol, c.now(), true)
			prev, a = &s, av
			c.advance(DefaultInterval)
		}
		return a
	}

	// Control: the same crash loop with no unique work and a live process
	// really does recover, so the refusals below are not vacuous.
	if a := crashLoop(TriNo, TriYes); a.NextAction != ActionRecover || !a.Acted {
		t.Fatalf("control: clean crash loop must recover and act, got %s acted=%v withheld=%q",
			a.NextAction, a.Acted, a.Withheld)
	}

	for _, unique := range []Tri{TriYes, TriUnknown, ""} {
		a := crashLoop(unique, TriYes)
		if a.NextAction != ActionOperator {
			t.Errorf("unique_work=%q must escalate to operator, got %s", unique, a.NextAction)
		}
		if a.Acted {
			t.Errorf("unique_work=%q must never act", unique)
		}
		if !strings.Contains(a.Withheld, "unique work") {
			t.Errorf("unique_work=%q must name the gate, got %q", unique, a.Withheld)
		}
	}

	for _, alive := range []Tri{TriUnknown, ""} {
		a := crashLoop(TriNo, alive)
		if a.Cause != CauseUnknownState || a.NextAction != ActionObserve || a.Acted {
			t.Errorf("proc_alive=%q must fail closed, got cause=%s action=%s acted=%v",
				alive, a.Cause, a.NextAction, a.Acted)
		}
	}
}

// Without a lifecycle store there is nothing to transition, so booking act
// budget for a recovery would spend the pane's whole hourly allowance on an
// action that cannot happen.
func TestRecoveryWithoutADurableTaskStateEscalatesInsteadOfSpendingBudget(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	var prev *Sample
	var s Sample
	var a Assessment
	for i := 0; i < pol.RestartCycles+1; i++ {
		o := obs("working", working(7, "abc", 42))
		o.PID, o.RecoveryAvailable = 100+i, false
		s, a = Assess(prev, o, pol, c.now(), true)
		prev = &s
		c.advance(DefaultInterval)
	}
	if a.NextAction != ActionOperator || a.Acted {
		t.Fatalf("unrecordable recovery must escalate, got %s acted=%v", a.NextAction, a.Acted)
	}
	if len(s.ActsUnix) != 0 {
		t.Fatalf("no budget may be spent, got %v", s.ActsUnix)
	}
	if !strings.Contains(a.Withheld, "lifecycle task state") {
		t.Fatalf("withheld must name the missing store, got %q", a.Withheld)
	}
}

// With no measurable signal at all, "nothing changed" is indistinguishable
// from "nothing was measured".
func TestNoMeasurableSignalFailsClosed(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	o := obs("working", Progress{Dirty: 0})
	_, a := run(t, nil, o, pol, c, true, pol.NoProgressCycles+2)
	if a.Cause != CauseUnknownState || a.NextAction != ActionObserve || a.Acted {
		t.Fatalf("unmeasurable pane must fail closed, got cause=%s action=%s acted=%v",
			a.Cause, a.NextAction, a.Acted)
	}
}

func TestReportOnlyModeNamesTheActionWithoutTakingIt(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	s, a := run(t, nil, obs("working", working(7, "abc", 42)), pol, c, false, pol.NoProgressCycles+1)
	if a.NextAction != ActionNudge || !a.Permitted {
		t.Fatalf("report-only must still name a permitted action, got %s permitted=%v", a.NextAction, a.Permitted)
	}
	if a.Acted || len(s.ActsUnix) != 0 {
		t.Fatalf("report-only must not book budget: acted=%v acts=%v", a.Acted, s.ActsUnix)
	}
}

// The rate limit is only a rate limit if it survives the process that set it.
func TestActBudgetSurvivesRestart(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	o := obs("working", working(7, "abc", 42))

	s, a := run(t, nil, o, pol, c, true, pol.NoProgressCycles+1)
	if !a.Acted {
		t.Fatalf("setup: expected an act, got %s withheld=%q", a.NextAction, a.Withheld)
	}
	actedAt := time.Unix(s.ActsUnix[len(s.ActsUnix)-1], 0)

	// Simulate the restart: the whole state file round-trips through JSON.
	raw, err := json.Marshal(map[string]Sample{s.PaneID: s})
	if err != nil {
		t.Fatal(err)
	}
	var reloaded map[string]Sample
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}
	restored := reloaded[s.PaneID]
	if len(restored.ActsUnix) != 1 {
		t.Fatalf("act budget lost across restart: %+v", restored.ActsUnix)
	}

	// Still inside the cooldown: a fresh process must refuse.
	c.t = actedAt.Add(pol.ActCooldown - time.Minute)
	next, a := Assess(&restored, o, pol, c.now(), true)
	if a.Acted || a.Permitted {
		t.Fatalf("cooldown must survive restart, got acted=%v permitted=%v", a.Acted, a.Permitted)
	}
	if !strings.Contains(a.Withheld, "cooldown") {
		t.Fatalf("withheld must name the cooldown, got %q", a.Withheld)
	}

	// Past the cooldown the second act is allowed, and it exhausts the
	// window — proving the refusal above was the cooldown, not a dead path.
	c.advance(2 * time.Minute)
	next, a = Assess(&next, o, pol, c.now(), true)
	if !a.Acted {
		t.Fatalf("past the cooldown the act must be permitted, got %q", a.Withheld)
	}
	c.advance(pol.ActCooldown + time.Minute)
	_, a = Assess(&next, o, pol, c.now(), true)
	if a.Acted || !strings.Contains(a.Withheld, "rate limit") {
		t.Fatalf("third act in the window must hit the rate limit, got acted=%v withheld=%q", a.Acted, a.Withheld)
	}
}

func TestActBudgetExpiresWithTheWindow(t *testing.T) {
	pol := DefaultPolicy()
	spent := Sample{PaneID: "p1", ActsUnix: []int64{1, 2}}
	c := newClock()
	_, a := Assess(&spent, obs("working", working(7, "abc", 42)), pol, c.now(), true)
	// Ancient stamps are outside the window, so the pane starts clean and
	// simply has not yet accumulated enough quiet samples.
	if a.Cause != CauseProgressing && a.Cause != CauseSlowWork {
		t.Fatalf("aged-out budget must not linger, got %s withheld=%q", a.Cause, a.Withheld)
	}
}

// A state file carrying a future timestamp must not be able to mute spin.
func TestFutureActStampsAreDiscarded(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	forged := c.now().Add(24 * time.Hour).Unix()
	s, _ := run(t, &Sample{PaneID: "p1", ActsUnix: []int64{forged, forged}},
		obs("working", working(7, "abc", 42)), pol, c, true, pol.NoProgressCycles+1)
	if len(s.ActsUnix) > 1 {
		t.Fatalf("future stamps must be discarded, got %v", s.ActsUnix)
	}
}

// One nudge, then recovery — spin does not nudge the same dead pane forever.
func TestSecondStallEscalatesFromNudgeToRecovery(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	o := obs("working", working(7, "abc", 42))
	s, a := run(t, nil, o, pol, c, true, pol.NoProgressCycles+1)
	if a.NextAction != ActionNudge || !a.Acted {
		t.Fatalf("setup: want a delivered nudge, got %s acted=%v", a.NextAction, a.Acted)
	}
	c.advance(pol.ActCooldown + time.Minute)
	_, a = Assess(&s, o, pol, c.now(), true)
	if a.NextAction != ActionRecover {
		t.Fatalf("a nudge that did not restore progress must escalate, got %s", a.NextAction)
	}
	if !a.Acted {
		t.Fatalf("escalation was blocked: %q", a.Withheld)
	}
}

// Recovered progress must clear the escalation ladder, or the next unrelated
// quiet patch would skip the nudge and go straight to a recovery transition.
func TestProgressResetsTheEscalationLadder(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	o := obs("working", working(7, "abc", 42))
	s, a := run(t, nil, o, pol, c, true, pol.NoProgressCycles+1)
	if a.NextAction != ActionNudge {
		t.Fatalf("setup: %s", a.NextAction)
	}
	// The nudge works: evidence advances.
	s, _ = Assess(&s, obs("working", working(8, "abc", 43)), pol, c.now(), true)
	if s.LastActionTaken != "" {
		t.Fatalf("progress must clear the ladder, got %q", s.LastActionTaken)
	}
	c.advance(pol.ActCooldown + time.Minute)
	later := obs("working", working(8, "abc", 43))
	// s already carries a zeroed streak, so the threshold is reached in
	// exactly NoProgressCycles further samples.
	_, a = run(t, &s, later, pol, c, true, pol.NoProgressCycles)
	if a.NextAction != ActionNudge {
		t.Fatalf("a fresh stall must start from a nudge again, got %s", a.NextAction)
	}
}

// Structured output must name the evidence and the next action; an operator
// should never have to trust the label alone.
func TestAssessmentNamesEvidenceAndNextAction(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	o := obs("working", working(7, "abc123def456", 42))
	o.Findings = []Finding{Stall}
	_, a := run(t, nil, o, pol, c, false, pol.NoProgressCycles+1)

	body, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"cause":"NO_PROGRESS"`, `"next_action":"nudge"`, `"evidence":`,
		"lifecycle_seq=7", "candidate_sha=abc123def456", "state_change_seq=42",
		`"diagnostics":["STALL"]`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("assessment JSON missing %s:\n%s", want, body)
		}
	}
}

// Terminal text is diagnostic only: a STALL finding must not, by itself,
// produce a no-progress verdict when the durable evidence is advancing.
func TestTerminalFindingsNeverDriveTheVerdict(t *testing.T) {
	pol := DefaultPolicy()
	c := newClock()
	var prev *Sample
	for i := 0; i < pol.NoProgressCycles+3; i++ {
		o := obs("working", working(int64(7+i), "abc", uint64(42+i)))
		o.Findings = []Finding{Stall, Spin, Long}
		s, a := Assess(prev, o, pol, c.now(), true)
		if a.Cause != CauseProgressing || a.Acted {
			t.Fatalf("cycle %d: text findings must not drive the verdict, got %s acted=%v", i, a.Cause, a.Acted)
		}
		prev = &s
		c.advance(DefaultInterval)
	}
}
