package lifecycle

import (
	"context"
	"testing"
)

func TestReleaseAndRearmRestoresConfiguredLoopAtomically(t *testing.T) {
	a, _, _ := holdFixture(t)
	id := HoldIdentity{Repository: "repo", Owner: "owner", Lane: "lane", Scope: "lane"}
	if err := a.ConfigureLoop(context.Background(), id, "standing goal", "standing wakeup"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Hold(context.Background(), id, "actor", "maintenance", "operator_hold", 1, nil); err != nil {
		t.Fatal(err)
	}
	state, err := a.ReleaseAndRearm(context.Background(), id, "actor", "resume", 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != LoopRunning || state.Goal != "standing goal" || state.Wakeup != "standing wakeup" {
		t.Fatalf("rearmed state = %+v", state)
	}
}
