package beat

import (
	"path/filepath"
	"strings"
	"testing"
)

// FAC-707: a builder reported PASS and stopped. A reviewer ingested a verdict
// and stopped. A merge landed and nothing consumed it. Each is a completed
// handoff with no next edge, and the lane then sat resident and idle until an
// operator noticed by hand -- eleven lanes at once, most recently.

func TestEventWithNoNextActionIsRefused(t *testing.T) {
	// The whole point. An event that ends a lane's work and names no successor
	// is the dead handoff this exists to prevent, and it must be impossible to
	// RECORD rather than merely discouraged.
	dir := t.TempDir()
	t.Setenv(WakeQueueEnv, filepath.Join(dir, "wake.jsonl"))

	err := Enqueue(dir, Wake{Lane: "docs-custodian", Event: TransitionVerdict})
	if err == nil {
		t.Fatal("an event that named no next action was recorded")
	}
	if !strings.Contains(err.Error(), "goes idle holding a completed handoff") {
		t.Fatalf("refusal does not explain the consequence: %v", err)
	}
}

func TestEnqueueAndReadBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(WakeQueueEnv, filepath.Join(dir, "wake.jsonl"))

	if err := Enqueue(dir, Wake{
		Lane:   "docs-custodian",
		Event:  TransitionMerge,
		Action: "herd review CHA-3103 --pool --sha abc123",
		Cause:  "PR 3438 merged",
	}); err != nil {
		t.Fatal(err)
	}
	pending, problems := PendingWakes(dir)
	if len(problems) != 0 {
		t.Fatalf("clean queue reported problems: %v", problems)
	}
	w, ok := pending["docs-custodian"]
	if !ok || !strings.Contains(w.Action, "CHA-3103") {
		t.Fatalf("wake did not survive the round trip: %+v", pending)
	}
	if w.RecordedAt == "" {
		t.Fatal("wake carries no timestamp, so a stale wake cannot be told from a current one")
	}
}

func TestLatestWakePerLaneWins(t *testing.T) {
	// The queue is append-only history; a lane's CURRENT next action is
	// whatever the latest event said. Replaying older wakes would resurrect
	// superseded work -- the review-ingest polarity defect in a new costume.
	dir := t.TempDir()
	t.Setenv(WakeQueueEnv, filepath.Join(dir, "wake.jsonl"))

	for _, action := range []string{"old action", "current action"} {
		if err := Enqueue(dir, Wake{Lane: "qa-sentinel", Event: TransitionVerdict, Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	pending, _ := PendingWakes(dir)
	if pending["qa-sentinel"].Action != "current action" {
		t.Fatalf("a superseded wake won: %+v", pending["qa-sentinel"])
	}
}

func TestOneBadLineDoesNotHideEveryOtherLane(t *testing.T) {
	// A malformed record must not blind the reader to every other lane's next
	// action -- that is a partial failure rendered as a total one.
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.jsonl")
	t.Setenv(WakeQueueEnv, path)

	if err := Enqueue(dir, Wake{Lane: "good-lane", Event: TransitionMerge, Action: "do the thing"}); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, path, "{not json\n")

	pending, problems := PendingWakes(dir)
	if len(problems) == 0 {
		t.Fatal("a malformed record was silently dropped")
	}
	if _, ok := pending["good-lane"]; !ok {
		t.Fatal("one bad line hid a healthy lane's wake")
	}
}

func TestMissingQueueIsEmptyNotAnError(t *testing.T) {
	// Most lanes have never been enqueued. Treating absence as failure would
	// fence a fleet that simply has not started.
	dir := t.TempDir()
	t.Setenv(WakeQueueEnv, filepath.Join(dir, "absent.jsonl"))
	pending, problems := PendingWakes(dir)
	if len(problems) != 0 || len(pending) != 0 {
		t.Fatalf("absent queue was not read as empty: %v %v", pending, problems)
	}
}

func appendRaw(t *testing.T, path, line string) {
	t.Helper()
	f, err := openAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}
