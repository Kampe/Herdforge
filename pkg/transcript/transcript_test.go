package transcript

import (
	"strings"
	"testing"
)

// A verbatim capture of a finished codex lane, which is the shape a
// coordinator was previously forced to read via raw herdr.
const finishedLanePane = `    ## herd/fac-547
────────────────────────────────────────────────────────

• Ran 3 commands · ctrl + t to view transcript

• BUILD COMPLETE FAC-547

  Implemented bounded pulse task lookup and regression
  coverage.

  Commit: 6669566c65de7dbab56a19ff1756a67c2e8dc1b3

  Build passed; targeted tests passed.

─ Worked for 15m 19s ───────────────────────────────────

› Ask Codex to do anything
`

func TestExtractHandoffReturnsTheFinalReport(t *testing.T) {
	got := ExtractHandoff(finishedLanePane)
	if !strings.Contains(got, "BUILD COMPLETE FAC-547") {
		t.Fatalf("handoff must contain the final report, got %q", got)
	}
	// The earlier bullet is a progress line, not the report.
	if strings.Contains(got, "Ran 3 commands") {
		t.Fatalf("handoff must start at the LAST report block, got %q", got)
	}
	// The harness input affordance is not part of the report.
	if strings.Contains(got, "Ask Codex") {
		t.Fatalf("handoff must not include the harness prompt, got %q", got)
	}
}

func TestExtractCommitsFindsCandidateObjects(t *testing.T) {
	got := ExtractCommits(finishedLanePane)
	found := false
	for _, c := range got {
		if c == "6669566c65de7dbab56a19ff1756a67c2e8dc1b3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want the reported commit among candidates, got %v", got)
	}
}

func TestExtractCommitsDeduplicates(t *testing.T) {
	text := "sha 6669566 again 6669566 and 6669566"
	if got := ExtractCommits(text); len(got) != 1 {
		t.Fatalf("want one unique object, got %v", got)
	}
}

// A pane with no report must yield an empty handoff, never a fabricated one.
func TestExtractHandoffEmptyWithoutAReport(t *testing.T) {
	if got := ExtractHandoff("plain shell output\n$ ls\n"); got != "" {
		t.Fatalf("want no handoff when the lane reported none, got %q", got)
	}
	if got := ExtractHandoff(""); got != "" {
		t.Fatalf("empty pane must yield empty handoff, got %q", got)
	}
}

func TestReadRequiresAgentName(t *testing.T) {
	if _, err := Read("   ", 0); err == nil {
		t.Fatal("an empty agent name must fail closed")
	}
}

// A closed tab has no readable pane. That must be reported as unavailable, not
// as an empty-but-successful transcript, or a coordinator reads silence as a
// healthy quiet lane.
func TestNoSuchAgentErrorNamesResidentAgents(t *testing.T) {
	err := &ErrNoSuchAgent{Name: "task-fac-999", Known: []string{"forge-mender", "task-fac-1"}}
	msg := err.Error()
	for _, want := range []string{"task-fac-999", "forge-mender", "closed tab"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must mention %q, got %q", want, msg)
		}
	}
	empty := (&ErrNoSuchAgent{Name: "x"}).Error()
	if !strings.Contains(empty, "no agents are resident") {
		t.Fatalf("empty roster must be stated explicitly, got %q", empty)
	}
}

// A verbatim tail from a live coordinator pane: the LAST bullet is harness
// chrome, and returning it instead of the report is what the first
// implementation did.
const noisyTailPane = `• Two more cards have moved to Done in this beat: CHA-2145 and CHA-2131. All six
  completed standing panes were re-armed through Herdforge with fresh work.

• Ran bin/herdforge attention
  └ herd-attention: active task authority: unknown provider status "archived"

• Working (2m 36s • esc to interrupt) · 1 background terminal running
`

func TestExtractHandoffSkipsHarnessChrome(t *testing.T) {
	got := ExtractHandoff(noisyTailPane)
	if strings.HasPrefix(got, "• Working") || strings.Contains(got, "esc to interrupt") {
		t.Fatalf("handoff must not be the harness activity line, got %q", got)
	}
	if !strings.Contains(got, "Two more cards have moved to Done") {
		t.Fatalf("handoff must be the lane's substantive report, got %q", got)
	}
}

func TestIsHarnessNoiseClassifiesActivityLines(t *testing.T) {
	noise := []string{
		"• Working (2m 36s • esc to interrupt)",
		"• Ran bin/herdforge attention",
		"• Added /tmp/x.md (+1 -0)",
		"• Waiting for background terminal",
	}
	for _, n := range noise {
		if !isHarnessNoise(n) {
			t.Fatalf("expected harness noise: %q", n)
		}
	}
	// A real report that happens to mention running something must survive.
	report := "• BUILD COMPLETE FAC-1\n\n  Ran the focused suite; all green."
	if isHarnessNoise(report) {
		t.Fatalf("a real report must not be classified as noise: %q", report)
	}
}

// When a pane contains only chrome the newest line is still returned, so a
// caller can tell the lane is mid-command rather than seeing nothing.
func TestExtractHandoffFallsBackToNewestWhenAllNoise(t *testing.T) {
	got := ExtractHandoff("• Ran something\n\n• Working (1s)\n")
	if got == "" {
		t.Fatal("an all-chrome pane must still report the newest line")
	}
}
