package main

import (
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewingest"
)

// FAC-583: the packet used to ask for "the required verdict artifact" without
// stating its shape. review-ingest refuses anything whose front matter is not
// the leading block, so a reviewer that opened with prose had its finished
// verdict discarded. A real Opus review of PR-3115 that caught a fabricated
// eth_call response (a 1e12 money error) was refused for exactly this reason.
//
// This asserts the packet carries every key the ingest gate requires, so a
// reviewer that follows it is ingestible by construction.
func TestReviewPacketCarriesIngestibleFrontMatterContract(t *testing.T) {
	body := reviewPacketBody("PR-3115", "8867353f0ba9fe569feeb28989c10d0fefdc6ca1",
		".herd/review-surfaces/review-pr-3115", "/repo/.herd/review/inbox/8867353f0ba9-review-pr-3115.md",
		"review-supervisor", "openai", "w2")

	for _, key := range []string{
		"sha:", "branch:", "task:", "reviewer:", "reviewer-family:",
		"builder-family:", "verdict:", "reviewed-head:",
	} {
		if !strings.Contains(body, key) {
			t.Errorf("packet omits required front-matter key %q; a reviewer cannot emit what it was never told about", key)
		}
	}

	// The known values must be prefilled, not left as placeholders for the
	// reviewer to retype and get wrong.
	if !strings.Contains(body, "sha: 8867353f0ba9fe569feeb28989c10d0fefdc6ca1") {
		t.Error("packet must prefill the candidate sha it already knows")
	}
	if !strings.Contains(body, "task: PR-3115") {
		t.Error("packet must prefill the card ref so the verdict can be joined to a card (FAC-578)")
	}

	// The leading-block rule is the one that silently discarded real reviews.
	if !strings.Contains(body, "first bytes") && !strings.Contains(body, "very first") {
		t.Error("packet must state that front matter has to be the leading block")
	}
}

// The contract the packet advertises must be the contract the parser accepts.
// If these two drift, reviewers follow instructions and still get refused.
func TestPacketContractMatchesParserAcceptedKeys(t *testing.T) {
	body := reviewPacketBody("CHA-1", strings.Repeat("a", 40), "surface", "/repo/.herd/review/inbox/a-review-cha-1.md", "review-supervisor", "openai", "w2")

	// Build a minimal artifact the way a compliant reviewer would, using the
	// packet's own block, and confirm the parser extracts what we expect.
	artifact := "sha: " + strings.Repeat("a", 40) + "\n" +
		"branch: rescue/x\ntask: CHA-1\nreviewer: review-cha-1-claude\n" +
		"reviewer-family: anthropic\nbuilder-family: openai\n" +
		"verdict: FAIL\nreviewed-head: " + strings.Repeat("a", 40) + "\n---\n" +
		strings.Repeat("evidence ", 40) + "\n"

	a := reviewingest.Parse(artifact)
	if len(a.UnknownHeaders) != 0 {
		t.Errorf("packet advertises keys the parser rejects as unknown: %v", a.UnknownHeaders)
	}
	if a.MalformedHeaderRegion {
		t.Error("a packet-compliant artifact must not be seen as a malformed header region")
	}
	if a.Verdict != "FAIL" {
		t.Errorf("verdict = %q want FAIL", a.Verdict)
	}
	if a.TaskRef != "CHA-1" {
		t.Errorf("task ref = %q want CHA-1", a.TaskRef)
	}
	// Guard against the packet advertising a key the parser silently ignores.
	for _, key := range []string{"branch:", "reviewed-head:", "builder-family:"} {
		if !strings.Contains(body, key) {
			t.Errorf("packet lost key %q", key)
		}
	}
}

// FAC-597: the packet must NAME its destination. Both .herd/review/inbox and
// .herd/review/outbox exist and MoveToIngestedNamed is location-agnostic, so a
// packet that did not say where to write left reviewers inferring the location
// from nearby files. A pool-01 reviewer for CHA-2255 wrote to outbox because
// other files were already there, and review-ingest never saw it — a completed
// review lost with no error anywhere.
func TestReviewPacketNamesAnAbsoluteVerdictDestination(t *testing.T) {
	dest := "/repo/.herd/review/inbox/8867353f0ba9-review-cha-2255-8867353f0ba9.md"
	body := reviewPacketBody("CHA-2255", "8867353f0ba9fe569feeb28989c10d0fefdc6ca1",
		"/repo/.herd/review-surfaces/review-cha-2255", dest, "review-supervisor", "openai", "w2")

	if !strings.Contains(body, dest) {
		t.Errorf("packet must state the exact destination path, got:\n%s", body)
	}
	if !filepath.IsAbs(dest) {
		t.Fatal("fixture destination must be absolute")
	}
	// The instruction has to forbid inference explicitly, because the observed
	// failure was a reviewer reasonably copying its neighbours.
	if !strings.Contains(strings.ToLower(body), "do not infer") {
		t.Error("packet must forbid inferring the output location")
	}
	// And it must say why, so the reviewer treats it as load-bearing rather
	// than as boilerplate it can improve on.
	if !strings.Contains(strings.ToLower(body), "never read") {
		t.Error("packet should state the consequence: a verdict written elsewhere is never read")
	}
}

// FAC-603: a reviewer that finishes must be told who to tell. Without a named
// owner, completion is discoverable only by polling every pane, which is how 89
// finished reviews ended up sitting unowned in one inbox.
func TestReviewPacketNamesTheSupervisorToReportTo(t *testing.T) {
	body := reviewPacketBody("PR-3115", "8867353f0ba9fe569feeb28989c10d0fefdc6ca1",
		".herd/review-surfaces/review-pr-3115", "/repo/.herd/review/inbox/v.md",
		"review-harvest-supervisor", "openai", "w2")

	if !strings.Contains(body, "review-harvest-supervisor") {
		t.Error("packet must name the supervisor the reviewer reports to")
	}
	// A negative verdict is a result the supervisor needs in order to release the
	// slot; silence on FAIL is the one outcome that helps nobody.
	for _, want := range []string{"FAIL", "BLOCKED"} {
		if !strings.Contains(body, want) {
			t.Errorf("packet must require reporting home on %s too", want)
		}
	}
}

// A harness name is not a vendor family. The packet used to say
// "anthropic|openai|google|xai|..." and the trailing ellipsis invited a guess: a
// codex-harness reviewer wrote reviewer-family "codex", which is not in
// FamilyAllowlist, so ingest refused the verdict and the review was lost.
func TestReviewPacketEnumeratesFamiliesAndRejectsHarnessNames(t *testing.T) {
	body := reviewPacketBody("CHA-9", strings.Repeat("b", 40), "surface",
		"/repo/.herd/review/inbox/v.md", "review-supervisor", "openai", "w2")

	for family := range reviewledger.FamilyAllowlist {
		if !strings.Contains(body, family) {
			t.Errorf("packet must enumerate allowed family %q; a reviewer cannot pick from a set it was never shown", family)
		}
	}
	// The harness-to-family mapping must be explicit, since the harness name is
	// the intuitive-but-wrong answer.
	for _, pair := range []string{"codex", "grok", "agy"} {
		if !strings.Contains(body, pair) {
			t.Errorf("packet must map harness %q to its vendor family", pair)
		}
	}
	if strings.Contains(body, "anthropic|openai|google|xai|...") {
		t.Error("the open-ended family list is what caused the bad guess; it must be gone")
	}
}

// FAC-608: the packet must PREFILL the builder family from the launch record.
// Leaving it as a placeholder is what made honest reviewers write "unknown",
// which admission then refused -- 25 discarded reviews in one inbox.
func TestReviewPacketPrefillsBuilderFamily(t *testing.T) {
	body := reviewPacketBody("CHA-7", strings.Repeat("c", 40), "surface",
		"/repo/.herd/review/inbox/v.md", "review-supervisor", "xai", "w2")

	if !strings.Contains(body, "builder-family: xai") {
		t.Error("packet must prefill the recorded builder family, not ask the reviewer to derive it")
	}
	if strings.Contains(body, "builder-family: <") {
		t.Error("builder-family must not remain a placeholder")
	}
}

// A blank family must never render as an empty header, which a reviewer would
// fill in by guessing. It says "unproven" so the reviewer reports it honestly.
func TestReviewPacketMarksUnprovenBuilderFamilyExplicitly(t *testing.T) {
	body := reviewPacketBody("CHA-8", strings.Repeat("d", 40), "surface",
		"/repo/.herd/review/inbox/v.md", "review-supervisor", "", "w2")

	if !strings.Contains(body, "builder-family: unproven") {
		t.Error("an absent family must render as an explicit 'unproven', never blank")
	}
}

// FAC-617: mail is host-local -- herd mail send appends to a file in the local
// checkout. A reviewer on the second host, where no supervisor is reachable,
// must be told to push the verdicts branch instead of composing a message the
// ledger host will never read.
func TestReviewPacketUsesBranchTransportWhenNoSupervisorIsReachable(t *testing.T) {
	body := reviewPacketBody("CHA-5", strings.Repeat("e", 40), "surface",
		"/repo/.herd/review/inbox/v.md", "", "xai", "w2")

	// Assert on the COMMAND, not the phrase: the warning prose necessarily says
	// "herd mail send writes a file in this checkout", so a bare substring check
	// fails on the very sentence that explains the problem.
	if strings.Contains(body, "herd mail send --from") {
		t.Error("with no reachable supervisor the packet must not instruct a mail send that cannot cross hosts")
	}
	for _, want := range []string{"verdicts/", "git push", "MAIL WILL NOT WORK"} {
		if !strings.Contains(body, want) {
			t.Errorf("packet must make the branch push the primary report (%q missing)", want)
		}
	}
}

// On the ledger host, where the supervisor IS reachable, mail remains the direct
// signal and must still be named.
func TestReviewPacketUsesMailWhenSupervisorIsReachable(t *testing.T) {
	body := reviewPacketBody("CHA-6", strings.Repeat("f", 40), "surface",
		"/repo/.herd/review/inbox/v.md", "forge-review-harvest-su-467b70d7", "xai", "w2")

	if !strings.Contains(body, "herd mail send --from") {
		t.Error("a reachable supervisor must still get a direct mail report")
	}
	if !strings.Contains(body, "forge-review-harvest-su-467b70d7") {
		t.Error("packet must name the live supervisor it resolved")
	}
	if strings.Contains(body, "MAIL WILL NOT WORK") {
		t.Error("the unreachable-host warning must not appear when mail works")
	}
}

// FAC-618: the branch line used to interpolate $(herd config workspace) -- a
// subcommand that does not exist -- so it expanded to nothing and instructed a
// push to refs/heads/verdicts/, an invalid ref that always fails. The third
// consecutive report-home mechanism that could not work.
func TestReviewPacketBranchLineNamesARealWorkspace(t *testing.T) {
	body := reviewPacketBody("CHA-1", strings.Repeat("a", 40), "s",
		"/repo/.herd/review/inbox/v.md", "", "xai", "w2")

	if strings.Contains(body, "herd config workspace") {
		t.Error("packet must not interpolate a subcommand that does not exist")
	}
	if !strings.Contains(body, "refs/heads/verdicts/w2") {
		t.Error("branch line must name the resolved workspace literally")
	}
	if strings.Contains(body, "verdicts/\n") || strings.Contains(body, "verdicts/ ") {
		t.Error("branch line must never render an empty ref")
	}
}

// An unresolvable workspace must leave a visible placeholder, not an empty ref
// that looks valid and fails at push time.
func TestReviewPacketBranchLinePlaceholderWhenWorkspaceUnknown(t *testing.T) {
	body := reviewPacketBody("CHA-1", strings.Repeat("a", 40), "s",
		"/repo/.herd/review/inbox/v.md", "", "xai", "")

	if strings.Contains(body, "verdicts/\n") {
		t.Fatal("an unknown workspace must not render an empty ref")
	}
	if !strings.Contains(body, "workspace-id") {
		t.Error("an unknown workspace must render a visible placeholder")
	}
}
