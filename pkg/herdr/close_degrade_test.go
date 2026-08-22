package herdr

import (
	"errors"
	"os"
	"strings"
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

// TestNoGenerationDegradesInsteadOfStranding is the FAC-579 gate.
//
// The FAC-577 missing-verb degradation was UNREACHABLE for the build that
// needed it: a herdr with no compare-close verb also reports no immutable
// generation, and the generation check returned first. So every failed launch on
// such a build stranded its own tab as an orphan for an operator to close by
// hand — exactly the outcome the degradation existed to prevent.
//
// The gate is structural because the surrounding loop needs a live herdr: assert
// that the generation branch closes and continues rather than returning.
func TestNoGenerationDegradesInsteadOfStranding(t *testing.T) {
	src, err := readSourceFile("live_harness_proof.go")
	if err != nil {
		t.Fatal(err)
	}
	idx := strings.Index(src, "a.StateChangeSeq == 0")
	if idx < 0 {
		t.Fatal("cannot locate the generation check")
	}
	// Look at the branch body only.
	branch := src[idx:]
	if end := strings.Index(branch, "\n\t\t\t\t\t}"); end > 0 {
		branch = branch[:end]
	}
	if strings.Contains(branch, "return fmt.Errorf(\"FAC-133 cleanup: tab") {
		t.Error("a missing generation must not strand the tab; degrade to close plus absence readback")
	}
	if !strings.Contains(branch, "tabCloseRaw") {
		t.Error("the no-generation branch must still attempt a real close")
	}
	if !strings.Contains(branch, "closeAttempted = true") {
		t.Error("the degraded close must be recorded so the absence readback runs")
	}
}

func readSourceFile(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}
