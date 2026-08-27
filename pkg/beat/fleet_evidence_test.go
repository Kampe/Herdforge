package beat

import (
	"strings"
	"testing"
)

// Acceptance tests built from the operator's exact fleet census at
// 2026-08-27T03:52Z, recorded because a synthetic fixture would not have caught
// this: eight standing lanes sat at agent_status=done with eligible domain work
// available, and the review supervisor sat idle. Only DeFi and the coordinator
// were working.
//
// "done" is the trap. It reads as success and it means the lane has STOPPED. A
// utilization count that treats it as productive reports a healthy fleet while
// eight lanes do nothing, which is precisely what happened.

// observedBefore is the census as reported, verbatim.
func observedBefore() []LaneState {
	done := []string{
		"security-sentinel", "nft-data-engineer", "coverage-integrity",
		"platform-ops", "chain-indexer", "api-crusader", "docs-custodian", "qa-sentinel",
	}
	states := make([]LaneState, 0, 12)
	for _, l := range done {
		states = append(states, LaneState{Lane: l, Status: "done", WorkAvailable: true})
	}
	states = append(states,
		LaneState{Lane: "review-supervisor", Status: "idle", WorkAvailable: true},
		LaneState{Lane: "defi-crusader", Status: "working"},
		LaneState{Lane: "orchestrator", Status: "working"},
	)
	return states
}

func TestObservedFleetReportsNineFailedBeats(t *testing.T) {
	u := ProjectUtilization(observedBefore())

	// Eight done + one idle supervisor, all with work available.
	if u.FailedBeats != 9 {
		t.Fatalf("observed census produced %d failed beats, want 9: %s", u.FailedBeats, u.Summarize())
	}
	if u.Working != 2 {
		t.Fatalf("only DeFi and coordinator were working; got %d", u.Working)
	}
	if !strings.HasPrefix(u.Summarize(), "utilization: 9 FAILED BEAT") {
		t.Fatalf("summary does not lead with the failure: %s", u.Summarize())
	}
}

// The specific correction the operator asked for: done is a terminal state, not
// productive capacity. A lane that finished and stopped is exactly as idle as
// one that never started.
func TestDoneIsNotProductiveCapacity(t *testing.T) {
	u := ProjectUtilization([]LaneState{
		{Lane: "docs-custodian", Status: "done", WorkAvailable: true},
	})
	if u.Working != 0 {
		t.Fatal("a done lane was counted as working capacity")
	}
	if u.FailedBeats != 1 {
		t.Fatal("a done lane with eligible work was not a failed beat")
	}
}

// After the operator rearmed every lane and launched the two missing ones, all
// transitioned to working. The beat must report a clean fleet -- a check that
// cannot recognise recovery is as useless as one that cannot see failure.
func TestRearmedFleetReportsNoFailedBeats(t *testing.T) {
	after := make([]LaneState, 0, 14)
	for _, l := range []string{
		"security-sentinel", "nft-data-engineer", "coverage-integrity", "platform-ops",
		"chain-indexer", "api-crusader", "docs-custodian", "qa-sentinel", "perf-cost-guard",
		"scout-planner", "review-supervisor", "ux-comber", "herd-smith", "defi-crusader",
	} {
		after = append(after, LaneState{Lane: l, Status: "working", WorkAvailable: true})
	}
	u := ProjectUtilization(after)
	if u.FailedBeats != 0 {
		t.Fatalf("a fully rearmed fleet still reported failures: %s", u.Summarize())
	}
	if u.Working != 14 {
		t.Fatalf("rearmed lanes not counted as working: %d", u.Working)
	}
	if strings.Contains(u.Summarize(), "FAILED") {
		t.Fatalf("healthy summary still shouts failure: %s", u.Summarize())
	}
}

// Terminal completion must emit a durable disposition and then name the next
// action. A lane that finished and enqueued nothing is the dead handoff.
func TestTerminalCompletionMustEnqueueNextAction(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(WakeQueueEnv, dir+"/wake.jsonl")

	// A completed verdict with no successor is refused at the point of record.
	if err := Enqueue(dir, Wake{Lane: "qa-sentinel", Event: TransitionVerdict}); err == nil {
		t.Fatal("a terminal completion recorded no next action and was accepted")
	}
	// With one, the lane has somewhere to go.
	if err := Enqueue(dir, Wake{
		Lane: "qa-sentinel", Event: TransitionVerdict,
		Action: "herd review CHA-3046 --pool --sha 0f0fd57f", Cause: "verdict ingested",
	}); err != nil {
		t.Fatalf("a completion naming its successor was refused: %v", err)
	}
	pending, _ := PendingWakes(dir)
	if pending["qa-sentinel"].Action == "" {
		t.Fatal("next action did not survive to the consumer")
	}
}
