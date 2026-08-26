package kick

import (
	"os"
	"path/filepath"
	"testing"
)

// FAC-660: roster membership was decided by exact string equality and the two
// sides never spelled a lane the same way. StandingIDs() returns "forge-<lane>"
// with no digest; a live agent is "forge-<lane>-<digest>" or "standing-<lane>".
// So an exact lookup could not match a running lane, and consumers reported a
// fleet that was not there: herd status said working=1 capacity=14 while pulse
// said busy=9, and attention returned state=UNKNOWN with a full fleet running.
func TestLaneForAgentMatchesEverySpellingALaneAppearsUnder(t *testing.T) {
	lanes := []string{"forge-herd-smith", "forge-platform-ops", "qa-sentinel"}
	for name, want := range map[string]string{
		"forge-herd-smith-2918de97b5": "herd-smith",   // repository-qualified
		"forge-herd-smith":            "herd-smith",   // bare forge form
		"standing-herd-smith":         "herd-smith",   // standing raiser form
		"standing-qa-sentinel":        "qa-sentinel",  // lane listed without prefix
		"forge-platform-ops-abc123":   "platform-ops", // digest suffix
	} {
		if got := LaneForAgent(name, lanes); got != want {
			t.Errorf("LaneForAgent(%q) = %q, want %q", name, got, want)
		}
	}
}

// An agent belonging to no configured lane must not be claimed by one, or the
// roster would over-report coverage.
func TestLaneForAgentClaimsNothingItDoesNotOwn(t *testing.T) {
	lanes := []string{"forge-herd-smith"}
	for _, name := range []string{"review-cha-2796-abc", "forge-other-lane", "", "herd-smith"} {
		if got := LaneForAgent(name, lanes); got != "" {
			t.Errorf("LaneForAgent(%q) = %q, want no match", name, got)
		}
	}
}

// A lane whose name PREFIXES another must not swallow the other's agents. This
// is why matching requires a hyphen boundary and takes the longest lane.
func TestLaneForAgentPrefersTheLongestLaneAndRespectsBoundaries(t *testing.T) {
	lanes := []string{"forge-review", "forge-review-harvest"}
	if got := LaneForAgent("forge-review-harvest-9f2a", lanes); got != "review-harvest" {
		t.Errorf("the longer lane must win, got %q", got)
	}
	if got := LaneForAgent("forge-review-1a2b", lanes); got != "review" {
		t.Errorf("a digest suffix must resolve to the short lane, got %q", got)
	}
}

// Roster coverage must be reportable as identities, not a bare count that cannot
// be reconciled against anything.
func TestLiveLaneIDsReportsWhichLanesAreActuallyRunning(t *testing.T) {
	lanes := []string{"forge-a", "forge-b", "forge-c"}
	live := LiveLaneIDs([]string{"forge-a-111", "standing-b", "review-unrelated"}, lanes)
	if len(live) != 2 {
		t.Fatalf("expected two covered lanes, got %v", live)
	}
	if live[0] != "a" || live[1] != "b" {
		t.Errorf("coverage must name the lanes, got %v", live)
	}
}

// FAC-660: a registry that lists lanes but marks NONE standing has not said the
// roster is empty -- it has said it does not carry standing flags. Treating the
// second as the first meant the config beside it was never read. Measured live:
// docs/agent/lane-registry.json held 14 lanes with 0 standing while
// .herd/herd.yaml marked 14 standing, so the roster came back empty, attention
// reported UNKNOWN with a full fleet running, and the reaper saw no lanes to
// protect.
func TestStandingIDsFallsThroughWhenARegistryCarriesNoStandingFlags(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "lane-registry.json")
	// Lanes present, none marked standing: the live shape.
	if err := os.WriteFile(reg, []byte(`{"lanes":[{"id":"a"},{"id":"b"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := registryPaths
	t.Cleanup(func() { registryPaths = prev })
	registryPaths = []string{reg}

	// With no config to fall through to, the answer is genuinely empty rather
	// than a fabricated roster.
	if got := StandingIDs(); len(got) != 0 {
		t.Fatalf("with no standing flags anywhere the roster is empty, got %v", got)
	}

	// A registry that DOES mark standing lanes stays authoritative.
	if err := os.WriteFile(reg, []byte(`{"lanes":[{"id":"a","standing":true},{"id":"b"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := StandingIDs()
	if len(got) != 1 || got[0] != ForgePrefix+"a" {
		t.Fatalf("a registry with standing flags must remain authoritative, got %v", got)
	}
}

// FAC-699: the launcher truncates an agent name to a fixed width without
// respecting hyphen boundaries. Lane "review-harvest-supervisor" launched as
// forge-review-harvest-su-467b70d7, so the live segment is a strict prefix cut
// INSIDE "supervisor". Boundary matching can never succeed against that, which
// is why attention reported the lane missing while its agent was live.
func TestTruncatedLaneNameStillResolves(t *testing.T) {
	lanes := []string{"forge-review-harvest-supervisor", "forge-orchestrator", "forge-harvest"}
	if got := LaneForAgent("forge-review-harvest-su-467b70d7", lanes); got != "review-harvest-supervisor" {
		t.Fatalf("truncated lane did not resolve: got %q", got)
	}
}

func TestAmbiguousTruncationRefusesToGuess(t *testing.T) {
	// Two lanes share the surviving prefix, so the truncation destroyed the
	// distinction. Binding to the wrong lane is worse than no binding: the
	// wrong lane looks healthy while the real one looks missing, and a reaper
	// acting on that closes the wrong pane.
	lanes := []string{"forge-review-harvest-supervisor", "forge-review-harvest-superviser-two"}
	if got := LaneForAgent("forge-review-harvest-su-467b70d7", lanes); got != "" {
		t.Fatalf("an ambiguous truncation was guessed: got %q", got)
	}
}

func TestShortPrefixDoesNotClaimALane(t *testing.T) {
	// A short surviving prefix is ambiguous by nature.
	lanes := []string{"forge-review-harvest-supervisor"}
	if got := LaneForAgent("forge-rev-467b70d7", lanes); got != "" {
		t.Fatalf("a short prefix claimed a lane: got %q", got)
	}
}

func TestExactMatchStillWinsOverTruncation(t *testing.T) {
	// The truncation path is a FALLBACK only. An agent that matches a lane
	// exactly must never be reassigned to a longer lane it merely prefixes.
	lanes := []string{"forge-harvest", "forge-harvest-supervisor"}
	if got := LaneForAgent("forge-harvest-467b70d7", lanes); got != "harvest" {
		t.Fatalf("exact/boundary match lost to truncation fallback: got %q", got)
	}
}
