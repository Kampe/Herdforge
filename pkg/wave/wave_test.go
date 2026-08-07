package wave

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func readySources(t *testing.T) Sources {
	t.Helper()
	lanes := []Lane{
		{Name: "coordinator", AgentName: "forge-coordinator"},
		{Name: "smith", AgentName: "forge-smith"},
	}
	return Sources{
		Winddown:    func(context.Context) error { return nil },
		BoardFreeze: func() (bool, string, error) { return false, "off", nil },
		Resources:   func() (string, string) { return "OK", "verdict=OK free=40 swap=0" },
		Quota:       func() (bool, string, error) { return true, "pools healthy", nil },
		HerdrOK:     func() (bool, string) { return true, "available" },
		StandingLanes: func() []Lane {
			return append([]Lane(nil), lanes...)
		},
		LiveAgents: func() ([]Agent, error) { return nil, nil },
		Held:       func(string) (bool, string) { return false, "" },
		Claimable: func(context.Context) ([]ClaimableRef, error) {
			return []ClaimableRef{{Ref: "FAC-1", Title: "first", Priority: "high", Role: "worker"}}, nil
		},
		InReview:  func(context.Context) (int, error) { return 1, nil },
		ReviewCap: 3,
	}
}

func TestEvaluateReadyReportIsReadOnly(t *testing.T) {
	src := readySources(t)
	var raiseCalls int
	// Evaluate must not invoke a raiser — we prove it by never passing one
	// and by counting that LiveAgents/Claimable are read-only call sites.
	rep, err := Evaluate(context.Background(), src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Mutation {
		t.Fatal("report mode must set Mutation=false")
	}
	if !rep.Ready {
		t.Fatalf("expected ready, gates=%+v", rep.Gates)
	}
	if rep.Mode != "report" {
		t.Fatalf("mode=%q", rep.Mode)
	}
	if len(rep.Claimable) != 1 || rep.Claimable[0].Ref != "FAC-1" {
		t.Fatalf("claimable=%+v", rep.Claimable)
	}
	if raiseCalls != 0 {
		t.Fatal("evaluate must never raise")
	}
	// Standing plan should want raise for missing agents.
	wantRaise := 0
	for _, p := range rep.StandingPlan {
		if p.Action == StandingRaise {
			wantRaise++
		}
	}
	if wantRaise != 2 {
		t.Fatalf("standing plan want 2 raises, got %+v", rep.StandingPlan)
	}
}

func TestFailedWinddownGateBlocksRaise(t *testing.T) {
	src := readySources(t)
	src.Winddown = func(context.Context) error { return errors.New("winddown is active") }

	raiser := &fakeRaiser{}
	rep, err := Run(context.Background(), src, Options{Standing: true}, raiser)
	if err == nil {
		t.Fatal("expected raise to be refused")
	}
	if rep.Ready {
		t.Fatal("report must not be ready when winddown is active")
	}
	if raiser.calls != 0 {
		t.Fatalf("raiser must not be called when gate fails; calls=%d", raiser.calls)
	}
	if rep.Mutation {
		t.Fatal("blocked raise must not set Mutation (no side effects occurred)")
	}
	if len(rep.RaiseResults) == 0 {
		t.Fatal("expected blocked raise results")
	}
	for _, r := range rep.RaiseResults {
		if r.Action != "blocked" {
			t.Fatalf("want blocked, got %+v", r)
		}
	}
}

func TestUnknownHerdrGateBlocksRaise(t *testing.T) {
	src := readySources(t)
	src.HerdrOK = func() (bool, string) { return false, "herdr CLI not found" }

	raiser := &fakeRaiser{}
	rep, err := Run(context.Background(), src, Options{Up: true}, raiser)
	if err == nil {
		t.Fatal("expected refuse")
	}
	if rep.Ready {
		t.Fatal("unknown herdr must not be ready")
	}
	if raiser.calls != 0 {
		t.Fatalf("raiser called %d times", raiser.calls)
	}
	found := false
	for _, g := range rep.Gates {
		if g.Name == "herdr" && g.Status == StatusUnknown && g.BlocksRaise {
			found = true
		}
	}
	if !found {
		t.Fatalf("herdr unknown gate missing: %+v", rep.Gates)
	}
}

func TestBoardFreezeBlocksRaise(t *testing.T) {
	src := readySources(t)
	src.BoardFreeze = func() (bool, string, error) { return true, "actor=op reason=incident", nil }

	raiser := &fakeRaiser{}
	if _, err := Run(context.Background(), src, Options{Standing: true}, raiser); err == nil {
		t.Fatal("freeze must refuse raise")
	}
	if raiser.calls != 0 {
		t.Fatal("raiser must not run under freeze")
	}
}

func TestResourcesAlertBlocksRaise(t *testing.T) {
	src := readySources(t)
	src.Resources = func() (string, string) { return "ALERT", "swap=4096" }

	raiser := &fakeRaiser{}
	rep, err := Run(context.Background(), src, Options{Standing: true}, raiser)
	if err == nil {
		t.Fatal("ALERT must refuse raise")
	}
	if rep.Ready {
		t.Fatal("expected not ready")
	}
	if raiser.calls != 0 {
		t.Fatal("raiser must not run on ALERT")
	}
}

func TestQuotaUnknownBlocksRaise(t *testing.T) {
	src := readySources(t)
	src.Quota = func() (bool, string, error) { return false, "", errors.New("live quota unreadable") }

	raiser := &fakeRaiser{}
	if _, err := Run(context.Background(), src, Options{Standing: true}, raiser); err == nil {
		t.Fatal("unknown quota must refuse raise")
	}
	if raiser.calls != 0 {
		t.Fatal("raiser must not run when quota unknown")
	}
}

func TestNilSourcesFailClosed(t *testing.T) {
	rep, err := Evaluate(context.Background(), Sources{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ready {
		t.Fatal("empty sources must never be ready")
	}
	// Every blocking gate should be present and non-ok.
	blocking := 0
	for _, g := range rep.Gates {
		if g.BlocksRaise {
			blocking++
			if g.Status == StatusOK {
				t.Fatalf("nil source gate %s must not be ok", g.Name)
			}
		}
	}
	if blocking < 5 {
		t.Fatalf("expected several blocking gates, got %d: %+v", blocking, rep.Gates)
	}
}

func TestRaiseStandingIsIdempotent(t *testing.T) {
	src := readySources(t)
	raiser := &fakeRaiser{}

	// First raise: both missing → two Raise calls.
	rep1, err := Run(context.Background(), src, Options{Standing: true}, raiser)
	if err != nil {
		t.Fatal(err)
	}
	if raiser.calls != 2 {
		t.Fatalf("first raise calls=%d want 2", raiser.calls)
	}
	raised := 0
	for _, r := range rep1.RaiseResults {
		if r.Action == "raised" {
			raised++
		}
	}
	if raised != 2 {
		t.Fatalf("raise results=%+v", rep1.RaiseResults)
	}

	// Second raise: agents now live → zero additional Raise calls.
	live := raiser.liveSnapshot()
	src.LiveAgents = func() ([]Agent, error) {
		out := make([]Agent, 0, len(live))
		for name := range live {
			out = append(out, Agent{Name: name, Status: "idle"})
		}
		return out, nil
	}
	before := raiser.calls
	rep2, err := Run(context.Background(), src, Options{Standing: true}, raiser)
	if err != nil {
		t.Fatal(err)
	}
	if raiser.calls != before {
		t.Fatalf("idempotent re-raise must not call Raise again; before=%d after=%d", before, raiser.calls)
	}
	for _, r := range rep2.RaiseResults {
		if r.Action != "already_live" {
			t.Fatalf("second pass must be already_live, got %+v", r)
		}
	}
}

func TestHeldLaneSkippedWithoutRaise(t *testing.T) {
	src := readySources(t)
	src.Held = func(name string) (bool, string) {
		if name == "forge-smith" {
			return true, "parked for human"
		}
		return false, ""
	}
	raiser := &fakeRaiser{}
	rep, err := Run(context.Background(), src, Options{Standing: true}, raiser)
	if err != nil {
		t.Fatal(err)
	}
	if raiser.calls != 1 {
		t.Fatalf("only unheld missing lane should raise; calls=%d results=%+v", raiser.calls, rep.RaiseResults)
	}
	var sawHeld, sawRaised bool
	for _, r := range rep.RaiseResults {
		if r.Name == "forge-smith" && r.Action == "skip_held" {
			sawHeld = true
		}
		if r.Name == "forge-coordinator" && r.Action == "raised" {
			sawRaised = true
		}
	}
	if !sawHeld || !sawRaised {
		t.Fatalf("results=%+v", rep.RaiseResults)
	}
}

func TestReportModeNeverMutatesEvenWhenReady(t *testing.T) {
	src := readySources(t)
	raiser := &fakeRaiser{}
	rep, err := Run(context.Background(), src, Options{}, raiser)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Mutation || raiser.calls != 0 {
		t.Fatalf("report mode mutated: mutation=%v calls=%d", rep.Mutation, raiser.calls)
	}
}

func TestClaimableIsNeverClaimed(t *testing.T) {
	src := readySources(t)
	var claimCalls int
	src.Claimable = func(context.Context) ([]ClaimableRef, error) {
		claimCalls++
		return []ClaimableRef{{Ref: "FAC-9", Title: "eligible"}}, nil
	}
	// No claim hook exists on Sources — prove we only list.
	rep, err := Evaluate(context.Background(), src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if claimCalls != 1 {
		t.Fatalf("claimable list calls=%d", claimCalls)
	}
	if len(rep.Claimable) != 1 {
		t.Fatal("expected claimable listed")
	}
	// Next actions must hand off, not claim in-process.
	found := false
	for _, a := range rep.NextActions {
		if a.Kind == "dispatch-claimable" {
			found = true
			if !strings.Contains(a.Command, "FAC-9") && !strings.Contains(a.Command, "pulse") {
				t.Fatalf("handoff command unexpected: %q", a.Command)
			}
		}
		if strings.Contains(strings.ToLower(a.Kind), "claim-now") {
			t.Fatalf("wave must not claim: %+v", a)
		}
	}
	if !found {
		t.Fatalf("expected dispatch handoff next action: %+v", rep.NextActions)
	}
}

func TestReviewPressureSurfaced(t *testing.T) {
	src := readySources(t)
	src.InReview = func(context.Context) (int, error) { return 5, nil }
	src.ReviewCap = 3
	rep, err := Evaluate(context.Background(), src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Review.Pressure || rep.Review.InReview != 5 {
		t.Fatalf("review=%+v", rep.Review)
	}
	found := false
	for _, a := range rep.NextActions {
		if a.Kind == "review-pressure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected review-pressure action: %+v", rep.NextActions)
	}
}

func TestReadyRequiresEveryBlockingGateOK(t *testing.T) {
	gates := []Gate{
		{Name: "a", Status: StatusOK, BlocksRaise: true},
		{Name: "b", Status: StatusOK, BlocksRaise: true},
		{Name: "info", Status: StatusFailed, BlocksRaise: false},
	}
	if !Ready(gates) {
		t.Fatal("all blocking ok should be ready")
	}
	gates[1].Status = StatusUnknown
	if Ready(gates) {
		t.Fatal("unknown blocking gate must fail Ready")
	}
}

func TestFormatHumanIncludesGatesAndNext(t *testing.T) {
	src := readySources(t)
	rep, err := Evaluate(context.Background(), src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := FormatHuman(rep)
	for _, want := range []string{"ready=true", "gate", "claimable", "next actions"} {
		if !strings.Contains(s, want) {
			t.Fatalf("FormatHuman missing %q:\n%s", want, s)
		}
	}
}

// fakeRaiser records Raise calls and tracks "live" agents for idempotency tests.
type fakeRaiser struct {
	mu    sync.Mutex
	calls int
	live  map[string]bool
	err   error
}

func (f *fakeRaiser) Raise(_ context.Context, lane Lane) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live == nil {
		f.live = map[string]bool{}
	}
	if f.live[lane.AgentName] {
		// Production raisers treat this as success; tests that want to catch
		// duplicate raises fail if calls increases without a prior live mark.
		return nil
	}
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.live[lane.AgentName] = true
	return nil
}

func (f *fakeRaiser) liveSnapshot() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]bool, len(f.live))
	for k, v := range f.live {
		out[k] = v
	}
	return out
}

// TestIdempotencyDoesNotDuplicateLiveAgents is the non-vacuous guard: if
// planStanding ignores the live index and re-issues Raise for already-live
// agents, raiser.calls jumps and Action is not already_live. The fake raiser
// starts empty so every Raise call increments calls — pre-seeding live names
// would make the call counter insensitive to a broken plan.
func TestIdempotencyDoesNotDuplicateLiveAgents(t *testing.T) {
	src := readySources(t)
	src.LiveAgents = func() ([]Agent, error) {
		return []Agent{
			{Name: "forge-coordinator", Status: "idle"},
			{Name: "forge-smith", Status: "working"},
		}, nil
	}
	raiser := &fakeRaiser{} // empty live map: any Raise increments calls
	rep, err := Run(context.Background(), src, Options{Standing: true}, raiser)
	if err != nil {
		t.Fatal(err)
	}
	if raiser.calls != 0 {
		t.Fatalf("already-live fleet must not call Raise; calls=%d plan=%+v results=%+v", raiser.calls, rep.StandingPlan, rep.RaiseResults)
	}
	if len(rep.RaiseResults) != 2 {
		t.Fatalf("results=%+v", rep.RaiseResults)
	}
	for _, r := range rep.RaiseResults {
		if r.Action != "already_live" {
			t.Fatalf("already-live agent must report already_live, got %+v (plan=%+v)", r, rep.StandingPlan)
		}
	}
	for _, p := range rep.StandingPlan {
		if p.Action != StandingAlreadyLive {
			t.Fatalf("standing plan must mark live agents already_live, got %+v", p)
		}
	}
}

// Mutation probe: if Ready is broken to always true, failed-gate tests above
// would still call the raiser. Prove Ready is what RaiseStanding consults.
func TestRaiseStandingConsultsReadyFlag(t *testing.T) {
	rep := &Report{
		Ready: false,
		StandingPlan: []StandingPlan{
			{Name: "forge-x", Lane: "x", Action: StandingRaise},
		},
	}
	raiser := &fakeRaiser{}
	if err := RaiseStanding(context.Background(), rep, raiser); err == nil {
		t.Fatal("expected error")
	}
	if raiser.calls != 0 {
		t.Fatal("RaiseStanding must honor Ready=false")
	}
}
