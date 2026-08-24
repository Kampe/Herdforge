package herdr

import (
	"testing"
	"time"
)

// FAC-601: absent and false are different facts. An older herdr omits
// interactive_ready entirely, and treating absence as "not ready" would block
// every delivery forever on those hosts. Absence means unknown, and unknown
// proceeds — which is exactly what those hosts do today.
func TestAwaitInteractiveReadyRequiresAName(t *testing.T) {
	if _, err := AwaitInteractiveReady("   ", time.Second); err == nil {
		t.Fatal("a blank agent name must be refused rather than polled")
	}
}

// A pointer field is the whole mechanism, so assert the three states are
// distinguishable. If this ever becomes a plain bool, absence silently becomes
// false and the FAC-601 regression returns.
func TestInteractiveReadyIsThreeStated(t *testing.T) {
	yes, no := true, false
	cases := map[string]*bool{"absent": nil, "ready": &yes, "not-ready": &no}
	if cases["absent"] != nil {
		t.Error("absent must be nil, not a zero value")
	}
	if cases["ready"] == nil || !*cases["ready"] {
		t.Error("ready must be a non-nil true")
	}
	if cases["not-ready"] == nil || *cases["not-ready"] {
		t.Error("not-ready must be a non-nil false, distinct from absent")
	}
	var e AgentEntry
	if e.InteractiveReady != nil {
		t.Error("a zero AgentEntry must report readiness as unknown, not false")
	}
}
