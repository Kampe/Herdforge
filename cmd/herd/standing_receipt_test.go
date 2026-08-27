package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/standing"
)

// FAC-620: FAC-615 gave standing lanes provider fallthrough, and nothing
// recorded the route that actually resolved.
//
// Chainseer evidence: CHA-2582 9d76009de5, CHA-3455 ac1ffa7321,
// CHA-3454 6f6c250d5 and CHA-3456 2ca09828d all have a Claude/Anthropic builder
// and NO launch row. The one lane that did have a row got it because a worker
// appended it by hand AFTER committing, which proves nothing about what the
// launcher resolved.
//
// These drive recordResolvedLaunchReceipt -- the function the standing
// launcher's StartAgent callback calls before kickoff delivery -- with the
// decision shape a FALLTHROUGH produces: lane configured codex, decision
// resolved claude. A helper-only test that fed it matching config and decision
// would pass while the wrong-pin bug shipped, which is the exact failure mode
// that produced four independent FAILs in this repository today.

func gitCmdForTest(dir string, args ...string) ([]byte, error) {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	return c.CombinedOutput()
}

func receiptFixture(t *testing.T) (root string, lane *config.LaneDef) {
	t.Helper()
	root = t.TempDir()

	// A real git worktree: a receipt with no branch cannot be joined to a
	// commit, and the writer refuses without one.
	runGit := func(args ...string) {
		t.Helper()
		if out, err := gitCmdForTest(root, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q", "-b", "wt/defi-crusader")
	runGit("commit", "-q", "--allow-empty", "-m", "base")

	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REPO_ROOT", root)
	t.Chdir(root)

	return root, &config.LaneDef{
		Name:     "defi-crusader",
		Role:     "worker",
		Provider: "codex",        // configured pin
		Model:    "gpt-5.6-luna", // configured model
		Harness:  "codex",
		Standing: true,
		Worktree: ".",
	}
}

func readReceipts(t *testing.T, root string) []launch.Receipt {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".herd", "launch-receipts.jsonl"))
	if err != nil {
		t.Fatalf("no launch receipt was written at all: %v", err)
	}
	var out []launch.Receipt
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r launch.Receipt
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("receipt line is not valid JSON: %v\n%s", err, line)
		}
		out = append(out, r)
	}
	return out
}

// THE regression. A standing lane configured for codex that RESOLVED to claude
// must record claude -- never the configured pin.
func TestStandingFallthroughRecordsTheResolvedRouteNotTheConfiguredPin(t *testing.T) {
	root, lane := receiptFixture(t)

	// Exactly what FAC-615's fallthrough produces.
	decision := &router.LaunchDecision{
		Provider: "claude",
		Model:    "claude-sonnet-5",
		Effort:   "medium",
		Harness:  "claude",
		Shape:    "implementation",
	}

	if err := recordResolvedLaunchReceipt(decision, lane, "forge-defi-crusader-2918de97b5", ".", "chainseer", "wB:t1", "wB:p1"); err != nil {
		t.Fatalf("recording provenance for a rerouted standing lane failed: %v", err)
	}

	got := readReceipts(t, root)
	if len(got) != 1 {
		t.Fatalf("want exactly one receipt, got %d", len(got))
	}
	r := got[0]

	if r.Provider != "claude" {
		t.Fatalf("provider = %q, want claude. The receipt kept the CONFIGURED pin after a reroute; "+
			"a wrong family is worse than none because independence would be computed against a family "+
			"that never wrote the code.", r.Provider)
	}
	if r.Model != "claude-sonnet-5" {
		t.Fatalf("model = %q, want claude-sonnet-5", r.Model)
	}
	if r.BuilderFamily != "anthropic" {
		t.Fatalf("builder_family = %q, want anthropic derived from the RESOLVED route", r.BuilderFamily)
	}
	if strings.EqualFold(r.Provider, lane.Provider) {
		t.Fatal("receipt provider equals the lane's configured provider after a fallthrough")
	}
	// Joinable to a commit: without a branch the row cannot be tied to a SHA.
	if strings.TrimSpace(r.Branch) == "" {
		t.Fatal("receipt has no branch; it cannot be joined to any commit")
	}
	if got, want := r.AdmittedBase, strings.TrimSpace(gitCandidateOutputAt(t, root, "rev-parse", "HEAD")); got != want {
		t.Fatalf("admitted_base = %q, want current admitted worktree base %q", got, want)
	}
	if strings.TrimSpace(r.Lane) == "" || strings.TrimSpace(r.Name) == "" {
		t.Fatalf("receipt does not identify the lane/agent: lane=%q name=%q", r.Lane, r.Name)
	}
	if r.CreatedAt.IsZero() {
		t.Fatal("receipt has no timestamp")
	}
	if !r.Accepted {
		t.Fatal("receipt does not record the route as accepted")
	}
}

// A route whose family cannot be derived must REFUSE, not record "unknown".
// Writing unprovable authorship down as if it were provenance is worse than
// writing nothing -- the manual recorder already refuses for this reason.
func TestAnUnmappableFamilyRefusesRatherThanRecordingUnknown(t *testing.T) {
	_, lane := receiptFixture(t)

	decision := &router.LaunchDecision{
		Provider: "not-a-real-provider",
		Model:    "not-a-real-model",
		Harness:  "claude",
		Shape:    "implementation",
	}

	err := recordResolvedLaunchReceipt(decision, lane, "agent", ".", "chainseer", "t", "p")
	if err == nil {
		t.Fatal("an unmappable route recorded a receipt; unprovable authorship must be refused")
	}
	if !strings.Contains(err.Error(), "unprovable authorship") {
		t.Fatalf("refusal does not name the risk: %v", err)
	}
}

// A lane that did NOT fall through must still record its own provider. The fix
// must not turn every receipt into a claude receipt.
func TestANonReroutedLaneRecordsItsConfiguredProvider(t *testing.T) {
	root, lane := receiptFixture(t)

	decision := &router.LaunchDecision{
		Provider: "codex",
		Model:    "gpt-5.6-luna",
		Effort:   "medium",
		Harness:  "codex",
		Shape:    "implementation",
	}

	if err := recordResolvedLaunchReceipt(decision, lane, "forge-defi-crusader", ".", "chainseer", "t", "p"); err != nil {
		t.Fatal(err)
	}
	r := readReceipts(t, root)[0]
	if r.Provider != "codex" || r.BuilderFamily != "openai" {
		t.Fatalf("non-rerouted lane recorded provider=%q family=%q, want codex/openai", r.Provider, r.BuilderFamily)
	}
}

// THE live-path regression the operator required: it must FAIL when the
// production write is deleted.
//
// This drives startStandingAgent -- the function the standing launcher's
// StartAgent callback calls -- rather than the writer directly. Removing the
// recordResolvedLaunchReceipt call from that function turns this red, which the
// three tests above would not.
func TestTheStandingLauncherItselfWritesTheReceiptBeforeKickoff(t *testing.T) {
	root, lane := receiptFixture(t)

	decision := &router.LaunchDecision{
		Provider: "claude", // resolved by FAC-615 fallthrough
		Model:    "claude-sonnet-5",
		Effort:   "medium",
		Harness:  "claude",
		Shape:    "implementation",
	}

	var started bool
	fakeStart := func(tabID, name, harness, paneID string, req launch.Request) error {
		started = true
		return nil
	}

	err := startStandingAgent(
		standing.Tab{ID: "wB:t1", PaneID: "wB:p1", Cwd: "."},
		"forge-defi-crusader-2918de97b5",
		standing.Route{Decision: decision},
		lane, "chainseer", fakeStart,
	)
	if err != nil {
		t.Fatalf("standing launch failed: %v", err)
	}
	if !started {
		t.Fatal("the agent was never started")
	}

	got := readReceipts(t, root)
	if len(got) == 0 {
		t.Fatal("the standing launcher started an agent and wrote NO launch receipt; " +
			"every commit it produces would be provenance_unrecorded, which is the reported defect")
	}
	if got[0].Provider != "claude" || got[0].BuilderFamily != "anthropic" {
		t.Fatalf("launcher recorded provider=%q family=%q, want the RESOLVED claude/anthropic",
			got[0].Provider, got[0].BuilderFamily)
	}
}

// A launch whose provenance cannot be recorded must FAIL the launch, not
// proceed silently into unprovable work.
func TestAStandingLaunchFailsWhenProvenanceCannotBeRecorded(t *testing.T) {
	_, lane := receiptFixture(t)

	decision := &router.LaunchDecision{
		Provider: "not-a-real-provider",
		Model:    "not-a-real-model",
		Harness:  "claude",
		Shape:    "implementation",
	}

	err := startStandingAgent(
		standing.Tab{ID: "t", PaneID: "p", Cwd: "."},
		"agent", standing.Route{Decision: decision}, lane, "chainseer",
		func(string, string, string, string, launch.Request) error { return nil },
	)
	if err == nil {
		t.Fatal("a lane launched with unrecordable provenance; its commits could never prove independence")
	}
	if !strings.Contains(err.Error(), "provenance could not be recorded") {
		t.Fatalf("failure does not name the cause: %v", err)
	}
}
