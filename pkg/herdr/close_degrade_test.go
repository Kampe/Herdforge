package herdr

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestCapabilityGapDistinguishedFromRefusal is the FAC-577/579/569 gate.
//
// A failed reviewer launch could not compensate its own tab, because the
// installed herdr has no `tab compare-close` verb and the cleanup path treated
// that identically to a refused close. An orphan tab an operator must close by
// hand is strictly worse than the race compare-and-close guards against.
//
// The distinction must hold in both directions: a missing verb degrades, a real
// conflict keeps refusing.
func TestCapabilityGapDistinguishedFromRefusal(t *testing.T) {
	missing := []string{
		"herdr tab compare-close: unknown command \"compare-close\"",
		"unrecognized command: compare-close",
		"error: unknown subcommand",
		"usage: herdr tab [create|close|list]",
		"herdr: command not found",
		"unknown flag: --generation",
		"FAC-133 cleanup: tab wB:t3FF has no immutable generation",
		"generation evidence is unavailable",
		"tab wK:tMF: BLOCKED close unavailable: tab generation is required",
	}
	for _, msg := range missing {
		if !CapabilityGapReason(errors.New(msg)) {
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
		if CapabilityGapReason(errors.New(msg)) {
			t.Errorf("real conflict must keep refusing: %q", msg)
		}
	}
	if CapabilityGapReason(nil) {
		t.Error("nil error is not a capability gap")
	}
}

// A conflict phrase must win even when the message also contains usage text,
// since a CLI commonly prints usage alongside a rejection.
func TestConflictWinsOverUsageNoise(t *testing.T) {
	err := errors.New("compare-and-close: stale-generation\nusage: herdr tab compare-close ...")
	if CapabilityGapReason(err) {
		t.Error("a stale generation reported with usage text is still a refusal")
	}
}

// TestOneExactTabCloseDefinition is the FAC-569 gate.
//
// Closing one exact tab we own was implemented three times, and the copies
// disagreed on the only question that matters: whether a refusal may be
// downgraded to a plain close. defaultCleanupClose delegated even on a stale
// generation, which is a real conflict; hardCloseTab refused even when the
// installed herdr has no compare-close support at all, stranding the tab it had
// just created. Every path now routes through CloseExactTab.
func TestOneExactTabCloseDefinition(t *testing.T) {
	for _, f := range []string{"live_harness_proof.go", "cleanup.go", "close_own_tab.go"} {
		src, err := readSourceFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Only the single definition may call the fenced primitive directly.
		if strings.Contains(src, "TabCloseCAS(") {
			t.Errorf("%s calls TabCloseCAS directly; route through CloseExactTab so the degrade rule cannot fork", f)
		}
		// And only it may fall back to an unfenced close.
		if strings.Contains(src, "tabCloseRaw(") {
			t.Errorf("%s closes unfenced directly; that decision belongs to CloseExactTab alone", f)
		}
	}
	src, err := readSourceFile("close_exact_tab.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TabCloseCAS(", "tabCloseRaw(", "AgentList()"} {
		if !strings.Contains(src, want) {
			t.Errorf("the single definition must contain %s", want)
		}
	}
}

// A missing generation must degrade rather than strand: a herdr with no
// compare-close verb also reports no generation, which is what made the earlier
// degradation unreachable for exactly the build that needed it.
func TestMissingGenerationIsACapabilityGap(t *testing.T) {
	if !CapabilityGapReason(errors.New("FAC-133 cleanup: tab wB:t3FF has no immutable generation")) {
		t.Error("no immutable generation is a capability gap, not a refusal")
	}
	// The identity projection must leave Generation empty rather than sending a
	// literal zero, which the server would read as a real generation claim.
	id := exactIdentityFor(AgentEntry{TabID: "wB:t1", Name: "n", PaneID: "p", StateChangeSeq: 0})
	if id.Generation != "" {
		t.Errorf("absent generation must stay empty, got %q", id.Generation)
	}
	id2 := exactIdentityFor(AgentEntry{TabID: "wB:t1", Name: "n", PaneID: "p", StateChangeSeq: 7})
	if id2.Generation != "7" {
		t.Errorf("a real generation must be carried, got %q", id2.Generation)
	}
}

func readSourceFile(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}

// TestAbsenceProvenAgainstTabsNotOnlyAgents guards the readback's blind spot.
//
// The agent list only reports tabs that HAVE an agent, so a leaked tab with no
// agent attached — exactly the orphan a failed launch leaves — reads as absent.
// When the workspace is known the tab list must be consulted as well.
func TestAbsenceProvenAgainstTabsNotOnlyAgents(t *testing.T) {
	restore := SetRunHerdrForTest(func(args ...string) (string, error) {
		switch {
		case len(args) > 1 && args[0] == "agent" && args[1] == "list":
			return `{"result":{"type":"agents","agents":[]}}`, nil
		case len(args) > 1 && args[0] == "tab" && args[1] == "list":
			// The tab survives with no agent attached.
			return `{"result":{"type":"tabs","tabs":[{"tab_id":"wK:tSURV","workspace_id":"wK"}]}}`, nil
		}
		return `{"result":{"type":"ok"}}`, nil
	})
	defer restore()

	out, err := CloseExactTab(ExactTabIdentity{Workspace: "wK", TabID: "wK:tSURV"})
	if err == nil {
		t.Fatal("a tab surviving with no agent must not be reported closed")
	}
	if out.Closed {
		t.Error("outcome must not claim closed when the tab is still listed")
	}
	if !strings.Contains(err.Error(), "still present") {
		t.Errorf("failure should name the surviving tab, got: %v", err)
	}
}

// TestObservedUsageBannerIsACapabilityGap is the FAC-576 correction.
//
// The installed herdr answers an unknown `tab` subcommand by printing its
// subcommand banner, which says "herdr tab commands:" and never the word
// "usage". Matching only "usage:" meant the gap went undetected on the exact
// build that HAS the gap, so a failed launch still stranded its tab — observed
// live, verbatim.
func TestObservedUsageBannerIsACapabilityGap(t *testing.T) {
	observed := errors.New("herdr tab compare-close: herdr tab commands:\n  herdr tab list [--workspace <workspace_id>]\n  herdr tab create ...")
	if !CapabilityGapReason(observed) {
		t.Error("a subcommand banner in place of the verb is a capability gap")
	}
	// A conflict reported alongside a banner is still a conflict.
	conflict := errors.New("compare-and-close: stale-generation\nherdr tab commands:\n  herdr tab list")
	if CapabilityGapReason(conflict) {
		t.Error("a stale generation reported with a banner is still a refusal")
	}
}
