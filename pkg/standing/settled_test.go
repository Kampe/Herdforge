package standing

import "testing"

// TestSettledLaneIsRecycledNotSkipped is the FAC-578 gate.
//
// A raise recycled a live standing lane only when its status was "done". A
// goal-driven agent that pauses or finishes its goal reports "idle", so it was
// classified "already live" and skipped — forever. The review supervisor paused
// with 43 queued candidates and 0 reviewed, and every later raise left it there.
//
// A standing lane exists only while it is working. Idle is not rest for such a
// lane, it is a stopped engine.
func TestSettledLaneIsRecycledNotSkipped(t *testing.T) {
	for _, status := range []string{"done", "idle", "DONE", "Idle"} {
		if !SettledStandingLane(Agent{Status: status, Kind: "codex", LoopMode: LoopRunning}, true) {
			t.Errorf("status %q is settled and must be recycled", status)
		}
	}
	// A lane that is genuinely working must never be recycled.
	for _, status := range []string{"working", "starting", "blocked"} {
		if SettledStandingLane(Agent{Status: status, Kind: "codex", LoopMode: LoopRunning}, true) {
			t.Errorf("status %q is not settled and must be left alone", status)
		}
	}
}

// A held or one-shot lane is idle ON PURPOSE. Recycling it would fight the
// operator's own hold.
func TestHeldLaneIsNotRecycled(t *testing.T) {
	for _, mode := range []LoopMode{LoopHeld, LoopOneShot} {
		if SettledStandingLane(Agent{Status: "idle", Kind: "codex", LoopMode: mode}, true) {
			t.Errorf("loop mode %q is deliberately idle and must not be recycled", mode)
		}
	}
	// Held does not protect a lane whose harness reported done: that agent is
	// gone regardless of the hold.
	if !SettledStandingLane(Agent{Status: "done", Kind: "codex", LoopMode: LoopHeld}, true) {
		t.Error("a done agent is gone; the hold does not resurrect it")
	}
}

// FAC-418 asymmetry: herdr reports idle for an actively-working OpenCode pane.
// Leaving such a lane idle costs a beat; recycling it costs the work.
func TestUnreliableIdleHarnessIsNotRecycledOnIdle(t *testing.T) {
	for _, kind := range []string{"opencode", "ollama", "lazer"} {
		if SettledStandingLane(Agent{Status: "idle", Kind: kind, LoopMode: LoopRunning}, true) {
			t.Errorf("%s misreports idle while working; recycling on idle would kill live work", kind)
		}
		// done is explicit from any harness.
		if !SettledStandingLane(Agent{Status: "done", Kind: kind, LoopMode: LoopRunning}, true) {
			t.Errorf("%s reporting done is settled regardless of idle reliability", kind)
		}
	}
	// A trustworthy harness IS recycled on idle — otherwise the fix is inert.
	for _, kind := range []string{"codex", "claude", "agy", "grok", ""} {
		if !SettledStandingLane(Agent{Status: "idle", Kind: kind, LoopMode: LoopRunning}, true) {
			t.Errorf("kind %q has trustworthy idle and must be recycled", kind)
		}
	}
}

// An ordinary raise must keep the historical skip-if-live contract: a freshly
// started agent is briefly idle, and killing it would thrash.
func TestIdleRecycleIsOptIn(t *testing.T) {
	a := Agent{Status: "idle", Kind: "codex", LoopMode: LoopRunning}
	if SettledStandingLane(a, false) {
		t.Error("a plain raise must not recycle an idle lane; it may have just started")
	}
	if !SettledStandingLane(a, true) {
		t.Error("the keep-alive path must recycle it")
	}
	// done is settled either way — that agent is gone, not starting.
	if !SettledStandingLane(Agent{Status: "done", Kind: "codex"}, false) {
		t.Error("done is settled regardless of the opt-in")
	}
}
