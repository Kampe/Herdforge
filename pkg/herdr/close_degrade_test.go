package herdr

import (
	"errors"
	"testing"
)

// TestCloseVerbUnsupportedDistinguishesGapFromRefusal is the FAC-577 gate.
//
// A failed reviewer launch could not compensate its own tab, because the
// installed herdr has no `tab compare-close` verb and the cleanup path treated
// that identically to a refused close. An orphan tab an operator must close by
// hand is strictly worse than the race compare-and-close guards against.
//
// The distinction must hold in both directions: a missing verb degrades, a real
// conflict keeps refusing.
func TestCloseVerbUnsupportedDistinguishesGapFromRefusal(t *testing.T) {
	missing := []string{
		"herdr tab compare-close: unknown command \"compare-close\"",
		"unrecognized command: compare-close",
		"error: unknown subcommand",
		"usage: herdr tab [create|close|list]",
		"herdr: command not found",
		"unknown flag: --generation",
	}
	for _, msg := range missing {
		if !closeVerbUnsupported(errors.New(msg)) {
			t.Errorf("missing verb must degrade, not refuse: %q", msg)
		}
	}
	// Real CAS conflicts are genuine refusals: degrading on these would let a
	// close race recycle-kill a tab that gained a new agent.
	conflicts := []string{
		"stale-generation", "attachment-changed", "active-mutation", "protected",
		"unresolved intent is not a close",
		"closed outcome without resulting absence",
	}
	for _, msg := range conflicts {
		if closeVerbUnsupported(errors.New(msg)) {
			t.Errorf("real conflict must keep refusing: %q", msg)
		}
	}
	if closeVerbUnsupported(nil) {
		t.Error("nil error is not a capability gap")
	}
}

// A conflict phrase must win even when the message also contains usage text,
// since a CLI commonly prints usage alongside a rejection.
func TestConflictWinsOverUsageNoise(t *testing.T) {
	err := errors.New("compare-and-close: stale-generation\nusage: herdr tab compare-close ...")
	if closeVerbUnsupported(err) {
		t.Error("a stale generation reported with usage text is still a refusal")
	}
}
