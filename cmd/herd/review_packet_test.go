package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func packetBody(ref, sha, surface, verdictPath, supervisor, builderFamily, workspace string) string {
	return reviewPacketBody(ref, sha, surface, verdictPath, supervisor, builderFamily, workspace, ref)
}

// FAC-583: the packet used to ask for "the required verdict artifact" without
// stating its shape. review-ingest refuses anything whose front matter is not
// the leading block, so a reviewer that opened with prose had its finished
// verdict discarded. A real Opus review of PR-3115 that caught a fabricated
// eth_call response (a 1e12 money error) was refused for exactly this reason.
//
// This asserts the packet carries every key the ingest gate requires, so a
// reviewer that follows it is ingestible by construction.
func TestReviewPacketCarriesIngestibleFrontMatterContract(t *testing.T) {
	body := packetBody("PR-3115", "8867353f0ba9fe569feeb28989c10d0fefdc6ca1",
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
	for _, prefilled := range []string{
		"sha: 8867353f0ba9fe569feeb28989c10d0fefdc6ca1",
		"builder-family: openai",
	} {
		if !strings.Contains(body, prefilled) {
			t.Errorf("packet fails to prefill %q", prefilled)
		}
	}

	// And the placeheld ones must be explicitly marked, not blank, so a
	// reviewer knows what to fill.
	for _, placeheld := range []string{
		"branch: <the branch this candidate lives on>",
		"reviewer: <your lane name — never a coordinator>",
		"reviewer-family: <your VENDOR family — see the exact list below>",
		"reviewed-head: <output of git rev-parse HEAD in the tree you actually read>",
	} {
		if !strings.Contains(body, placeheld) {
			t.Errorf("packet fails to mark placeholder %q", placeheld)
		}
	}
}

func TestReviewPacketBindsCloseableTaskIdentity(t *testing.T) {
	tests := []struct {
		selector string
		card     string
	}{
		{selector: "herd/fac-755", card: "FAC-755"},
		{selector: "herd/fac-734", card: "FAC-734"},
		{selector: "FAC-654", card: "FAC-654"},
	}
	for _, tt := range tests {
		t.Run(tt.selector, func(t *testing.T) {
			body := reviewPacketBody(tt.selector, "a0a0704dde900000000000000000000000000000",
				".herd/review-surfaces/review-a0a0704dde90",
				"/repo/.herd/review/inbox/a0a0704dde90-review.md",
				"forge-review-supervisor-4922de28", "xai", "wK", tt.card)
			if tt.selector != tt.card && strings.Contains(body, "\ntask: "+tt.selector+"\n") {
				t.Fatalf("old branch-prefill path is live: packet task is %s; ingest cannot close that", tt.selector)
			}
			if !strings.Contains(body, "\ntask: "+tt.card+"\n") {
				t.Fatalf("packet launched for %s must prefill task %s", tt.selector, tt.card)
			}
			if !strings.Contains(body, "REVIEW "+tt.selector) {
				t.Fatal("candidate selector must remain the review title")
			}
		})
	}
}

func TestReviewPacketNamesRepositoryOwnedContractPaths(t *testing.T) {
	body := reviewPacketBody("FAC-668", strings.Repeat("a", 40), "surface", "/repo/.herd/review/inbox/v.md", "review-supervisor", "openai", "w2")
	for _, path := range []string{".herd/prompts/reviewer.md", ".herd/prompts/review-verdict.template.md"} {
		if !strings.Contains(body, path) {
			t.Errorf("packet must name candidate-owned contract path %q", path)
		}
	}
	obsolete := strings.Join([]string{"docs/prompts", "review-contract.md"}, "/")
	if strings.Contains(body, obsolete) {
		t.Error("packet must not name the obsolete shared/nonexistent contract")
	}
}

// The contract the packet advertises must be the contract the parser accepts.
// If these two drift, reviewers follow instructions and still get refused.
func TestPacketContractMatchesParserAcceptedKeys(t *testing.T) {
	body := packetBody("CHA-1", strings.Repeat("a", 40), "surface", "/repo/.herd/review/inbox/a-review-cha-1.md", "review-supervisor", "openai", "w2")
	if !strings.Contains(body, "sha:") {
		t.Fatal("packet missing sha:")
	}

	// Build a minimal artifact the way a compliant reviewer would, using the
	// packet's own block, and confirm the parser extracts what we expect.
	sample := `sha: 8867353f0ba9fe569feeb28989c10d0fefdc6ca1
branch: feature/some-work
task: CHA-1
reviewer: test-reviewer
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-head: 8867353f0ba9fe569feeb28989c10d0fefdc6ca1
---
Evidence here.`

	meta := reviewingest.Parse(sample)

	if meta.SHA != "8867353f0ba9fe569feeb28989c10d0fefdc6ca1" ||
		meta.TaskRef != "CHA-1" ||
		meta.Reviewer != "test-reviewer" ||
		meta.ReviewerFamily != "anthropic" ||
		meta.BuilderFamily != "openai" ||
		meta.Verdict != "PASS" ||
		meta.ReadHead != "8867353f0ba9fe569feeb28989c10d0fefdc6ca1" {
		t.Fatalf("parsed metadata does not match sample: %+v", meta)
	}
}

// FAC-597: the verdict destination MUST be written as an absolute path in the
// packet body. Two inbox directories exist (.herd/review/inbox and
// .herd/reviews/<host>/verdicts) and only the supervisor's inbox is watched for
// ingest. A reviewer writing to a relative "inbox/" wrote to the surface local
// directory, where review-ingest never looks, and the verdict was lost with no
// error anywhere.
func TestReviewPacketNamesAnAbsoluteVerdictDestination(t *testing.T) {
	dest := "/repo/.herd/review/inbox/8867353f0ba9-review-cha-2255-8867353f0ba9.md"
	body := packetBody("CHA-2255", "8867353f0ba9fe569feeb28989c10d0fefdc6ca1",
		"/repo/.herd/review-surfaces/review-cha-2255", dest, "review-supervisor", "openai", "w2")

	if !strings.Contains(body, dest) {
		t.Errorf("packet omits absolute verdict path %q", dest)
	}
	if !filepath.IsAbs(dest) {
		t.Errorf("dest path %q is not absolute", dest)
	}
}

// FAC-601: the packet must name the exact live supervisor pane or durable identity
// to report to. Without an explicit target, reviewers report to no one, and
// completion is discoverable only by polling every pane, which is how 89
// finished reviews ended up sitting unowned in one inbox.
func TestReviewPacketNamesTheSupervisorToReportTo(t *testing.T) {
	body := packetBody("PR-3115", "8867353f0ba9fe569feeb28989c10d0fefdc6ca1",
		".herd/review-surfaces/review-pr-3115", "/repo/.herd/review/inbox/v.md",
		"review-harvest-supervisor", "openai", "w2")

	if !strings.Contains(body, "review-harvest-supervisor") {
		t.Errorf("packet fails to name the target supervisor 'review-harvest-supervisor'")
	}
}

// FAC-610: the packet must enumerate valid vendor family values explicitly. A
// codex-harness reviewer wrote reviewer-family "codex", which is not in
// FamilyAllowlist, so ingest refused the verdict and the review was lost.
func TestReviewPacketEnumeratesFamiliesAndRejectsHarnessNames(t *testing.T) {
	body := packetBody("CHA-9", strings.Repeat("b", 40), "surface",
		"/repo/.herd/review/inbox/v.md", "review-supervisor", "openai", "w2")

	for family := range reviewledger.FamilyAllowlist {
		if !strings.Contains(body, family) {
			t.Errorf("packet omits valid family %q from the allowed list", family)
		}
	}

	for _, invalid := range []string{"codex", "claude", "grok", "opencode"} {
		if strings.Contains(body, "  "+invalid+"  ") || strings.Contains(body, "  "+invalid+"\n") {
			t.Errorf("packet accidentally advertises harness %q as a family value", invalid)
		}
	}
}

// FAC-612: the packet must prefill builder-family from candidate resolution.
// Leaving it as a placeholder is what made honest reviewers write "unknown",
// which admission then refused -- 25 discarded reviews in one inbox.
func TestReviewPacketPrefillsBuilderFamily(t *testing.T) {
	body := packetBody("CHA-7", strings.Repeat("c", 40), "surface",
		"/repo/.herd/review/inbox/v.md", "review-supervisor", "xai", "w2")

	if !strings.Contains(body, "builder-family: xai") {
		t.Error("packet must prefill builder-family with the resolved value")
	}
	if strings.Contains(body, "builder-family: <") {
		t.Error("builder-family must not remain a placeholder")
	}
}

// A blank family must never render as an empty header, which a reviewer would
// fill in by guessing. It says "unproven" so the reviewer reports it honestly.
func TestReviewPacketMarksUnprovenBuilderFamilyExplicitly(t *testing.T) {
	body := packetBody("CHA-8", strings.Repeat("d", 40), "surface",
		"/repo/.herd/review/inbox/v.md", "review-supervisor", "", "w2")

	if !strings.Contains(body, "builder-family: unrecorded") {
		t.Error("an absent family must render the canonical unrecorded sentinel, never blank")
	}
}

// FAC-617: mail is host-local -- herd mail send appends to a file in the local
// checkout. A reviewer on the second host, where no supervisor is reachable,
// must be told to push the verdicts branch instead of composing a message the
// ledger host will never read.
func TestReviewPacketUsesBranchTransportWhenNoSupervisorIsReachable(t *testing.T) {
	body := packetBody("CHA-5", strings.Repeat("e", 40), "surface",
		"/repo/.herd/review/inbox/v.md", "", "xai", "w2")

	// Assert on the COMMAND, not the phrase: the warning prose necessarily says
	// "herd mail send writes a file in this checkout", so a bare substring check
	// fails on the very sentence that explains the problem.
	if strings.Contains(body, "herd mail send --from") {
		t.Error("with no reachable supervisor the packet must not instruct a mail send that cannot cross hosts")
	}
	for _, want := range []string{"verdict-push --artifact", "MAIL WILL NOT WORK"} {
		if !strings.Contains(body, want) {
			t.Errorf("packet must make verdict-push the primary report (%q missing)", want)
		}
	}
}

// On the ledger host, where the supervisor IS reachable, mail remains the direct
// signal and must still be named.
func TestReviewPacketUsesMailWhenSupervisorIsReachable(t *testing.T) {
	body := packetBody("CHA-6", strings.Repeat("f", 40), "surface",
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
	body := packetBody("CHA-1", strings.Repeat("a", 40), "s",
		"/repo/.herd/review/inbox/v.md", "", "xai", "w2")

	if strings.Contains(body, "herd config workspace") {
		t.Error("packet must not interpolate a subcommand that does not exist")
	}
	if !strings.Contains(body, "--workspace w2") {
		t.Error("transport command must name the resolved workspace literally")
	}
	// FAC-622 made this an absolute binary path, so match the subcommand rather
	// than a bare "herd verdict-push" that no longer appears.
	if !strings.Contains(body, "verdict-push --artifact") {
		t.Error("packet must instruct the command that works, not a hand-rolled git recipe")
	}
	// Assert on the INSTRUCTION, not the word: the warning prose necessarily says
	// "git add stages nothing", which is the sentence explaining the trap.
	if strings.Contains(body, "  git add ") {
		t.Error("git add of a verdict silently no-ops under .gitignore /.herd/*; the packet must not instruct it")
	}

}

// An unresolvable workspace must leave a visible placeholder, not an empty ref
// that looks valid and fails at push time.
func TestReviewPacketBranchLinePlaceholderWhenWorkspaceUnknown(t *testing.T) {
	body := packetBody("CHA-1", strings.Repeat("a", 40), "s",
		"/repo/.herd/review/inbox/v.md", "", "xai", "")

	// With no workspace the flag is omitted entirely, so verdict-push resolves it
	// itself and refuses loudly if it cannot -- strictly better than emitting a
	// ref that looks valid and fails only at push time.
	if strings.Contains(body, "--workspace \n") || strings.Contains(body, "--workspace  ") {
		t.Fatal("an unknown workspace must omit the flag, never pass it empty")
	}
}

// FAC-622: `herd` is not on a reviewer's PATH. Verified on the review host:
// `command -v herd` finds nothing in a pool worktree under a non-login shell,
// and only the absolute path resolves. A blocked reviewer reported it as "herd
// isn't installed here" while holding a finished PASS verdict it could not
// transport. The packet must name a command the reviewer can execute.
func TestReviewPacketNamesAnExecutableBinaryPath(t *testing.T) {
	body := packetBody("CHA-1", strings.Repeat("a", 40), "s",
		"/repo/.herd/review/inbox/v.md", "", "xai", "w2")

	if !strings.Contains(body, "verdict-push") {
		t.Fatal("packet must instruct verdict-push")
	}
	// The transport line must not begin with a bare `herd`, which resolves only
	// in a shell that already has it on PATH -- i.e. the operator's, not the
	// reviewer's.
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "verdict-push") {
			t.Fatalf("transport line lost its binary entirely: %q", trimmed)
		}
		if strings.HasPrefix(trimmed, "herd verdict-push") {
			t.Fatalf("transport line names a bare `herd`, which is not on a reviewer's PATH: %q", trimmed)
		}
	}
}

// FAC-630: the packet emitted the bare literal "unproven" while the ledger's
// canonical sentinel is reviewledger.FamilyUnrecorded ("unrecorded"). Two
// spellings for one concept survived only because ingest happened to accept both.
// Pin them together so a future rename of either cannot silently desynchronise
// what reviewers are told to write from what admission recognises.
func TestPacketUnrecordedSentinelMatchesTheLedgerConstant(t *testing.T) {
	got := builderFamilyOrUnrecorded("")
	if got != reviewledger.FamilyUnrecorded {
		t.Fatalf("packet emits %q but the ledger sentinel is %q; reviewers would write a value admission does not canonically recognise",
			got, reviewledger.FamilyUnrecorded)
	}
	body := packetBody("CHA-1", strings.Repeat("a", 40), "s",
		"/repo/.herd/review/inbox/v.md", "", "", "w2")
	if !strings.Contains(body, "builder-family: "+reviewledger.FamilyUnrecorded) {
		t.Errorf("packet must instruct the canonical sentinel; body lacks it")
	}
	if strings.Contains(body, "builder-family: unproven") {
		t.Error("the stale 'unproven' spelling must not reappear in the packet")
	}
}

// Seq 5423 / review-cha-3214-b7606267567a: a reviewer saw the surface symlink
// resolve to its own leased pool cwd and called that "non-isolated". The pool
// lease is exclusive; the symlink aliasing that cwd is the designed surface.
// The packet must say so, and must tell reviewers that show-toplevel resolving
// under .herd/pool/ is correct while the shared checkout root is the fail-closed
// case. Prefer /tmp or untracked scratch for non-vacuity swaps.
func TestReviewPacketExplainsSurfaceAliasAndPoolToplevel(t *testing.T) {
	body := packetBody("CHA-3214", "b7606267567ab149a427d7b7c142790a23141141",
		".herd/review-surfaces/review-cha-3214-b7606267567a",
		"/repo/.herd/review/inbox/b7606267567a-review-cha-3214-b7606267567a.md",
		"forge-review-harvest-su", "openai", "wB")

	for _, want := range []string{
		"SYMLINK into your exclusive leased warm-pool slot",
		"NOT a broken isolation",
		".herd/pool/",
		"false positive",
		"/tmp or an untracked scratch",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("packet must explain surface-alias contract; missing %q", want)
		}
	}
	// The old "MUST be your surface" string compared poorly to resolved toplevel
	// and produced the false non-isolated report. Keep the shared-checkout stop.
	if !strings.Contains(body, "shared checkout root") {
		t.Error("packet must still fail closed when toplevel is the shared checkout")
	}
}
