package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/review"
)

func drainTestReport() *review.DrainReport {
	return &review.DrainReport{KaneoOK: true, KaneoInReview: 0, Cap: 2, StandingLanes: []string{"lane", "lane-a", "lane-b"}, Shas: review.DrainShas{NeedReview: []string{"sha-review"}}}
}

func drainTestHooks(calls *map[string]int) drainActionHooks {
	return drainActionHooks{
		launchReview: func(context.Context, drainActionEvidence) error { (*calls)["review"]++; return nil },
		dryRun:       func(context.Context, drainActionEvidence) error { (*calls)["dry-run"]++; return nil },
		harvest:      func(context.Context, drainActionEvidence) error { (*calls)["harvest"]++; return nil },
	}
}

func TestDrainCLI_ExitContracts(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	binary := buildHerd(t)
	repo := t.TempDir()
	runGitT(t, repo, "init", "-q", "-b", "main")
	runGitT(t, repo, "config", "user.email", "drain@test")
	runGitT(t, repo, "config", "user.name", "drain")
	runGitT(t, repo, "commit", "--allow-empty", "-q", "-m", "base")
	runGitT(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	ledger := filepath.Join(repo, "ledger.jsonl")
	if err := os.WriteFile(ledger, nil, 0600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HERD_REVIEW_LEDGER="+ledger, "HERD_STATE_DIR="+filepath.Join(repo, "state"))

	cmd := exec.Command(binary, "drain", "--json")
	cmd.Dir, cmd.Env = repo, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("json must exit 0: %v\n%s", err, out)
	}
	var packet map[string]json.RawMessage
	if err := json.Unmarshal(out, &packet); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(packet) == 0 || packet["kaneo_ok"] == nil {
		t.Fatalf("missing fixed packet keys: %s", out)
	}
	actCmd := exec.Command(binary, "drain", "--act")
	actCmd.Dir, actCmd.Env = repo, env
	actOut, actErr := actCmd.CombinedOutput()
	if actErr == nil || !strings.Contains(string(actOut), "FAC-184") || strings.Contains(string(actOut), "launched_reviews=1") {
		t.Fatalf("--act did not fail closed without side effects: %v\n%s", actErr, actOut)
	}

	cmd = exec.Command(binary, "drain", "--selftest")
	cmd.Dir = repo
	cmd.Env = env
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "FAC-184") || !strings.Contains(string(out), "FAC-182") {
		t.Fatalf("selftest did not fail closed on blocked adapters: %v\n%s", err, out)
	}

	cmd = exec.Command(binary, "drain", "--max-review", "-1")
	cmd.Dir = repo
	if out, err = cmd.CombinedOutput(); err == nil {
		t.Fatalf("negative bounds must fail: %s", out)
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("negative bound exit=%v output=%s", err, out)
	}
}

func TestDrainOutput_UsesEvidenceAndActiveExitWork(t *testing.T) {
	r := &review.DrainReport{
		KaneoOK:          true,
		KaneoInReview:    0,
		Cap:              2,
		RefactoringCount: 1,
		Harvestable:      1,
		Shas:             review.DrainShas{Harvestable: []string{"harvest-sha"}, HarvestReady: []string{"harvest-sha"}, NeedReview: []string{"veto-sha", "review-sha"}},
		Pins:             []review.PinFreshness{{SHA: "other-sha", Branch: "task/other"}, {SHA: "harvest-sha", Branch: "task/FAC-1", Note: "clean"}},
		ActionEvidence: []review.DrainActionEvidence{
			{SHA: "harvest-sha", Lane: "lane-1", BuilderFamily: "anthropic", Tier: "R1", TierRecorded: true},
			{SHA: "veto-sha", Branch: "task/FAC-2", Vetoed: true},
			{SHA: "review-sha", Branch: "task/FAC-3", BuilderFamily: "google"},
		},
	}
	var out bytes.Buffer
	printDrainReportTo(&out, r)
	if strings.Contains(out.String(), "other-sha") || !strings.Contains(out.String(), "harvest-sha") {
		t.Fatalf("harvest output was not projected from harvestable SHAs: %s", out.String())
	}
	out.Reset()
	printDrainCommandsTo(&out, r)
	text := out.String()
	if !strings.Contains(text, "# REFUSED review veto-sha: vetoed SHA") || !strings.Contains(text, "FAC-184 compiled adapter unavailable") || !strings.Contains(text, "pin=review-sha") {
		t.Fatalf("commands lost evidence gates: %s", text)
	}
	if strings.Contains(text, "<branch>") || strings.Contains(text, "bin/herd-review") || strings.Contains(text, "bin/herd-harvest-merge") || !strings.Contains(text, "FAC-184 compiled adapter unavailable (lane=lane-1") {
		t.Fatalf("commands contain fabricated placeholders: %s", text)
	}
	if drainExitCode(r) != 1 {
		t.Fatal("active actionable work incorrectly exits success")
	}
}

func TestDrainStandingLaneWhitespaceAndDisabledRebase(t *testing.T) {
	r := drainTestReport()
	r.StandingLanes = []string{"standing-worker"}
	var out bytes.Buffer
	result := executeDrainActions(context.Background(), r, []drainActionEvidence{{SHA: "sha", Lane: "standing-worker", RebaseNeeded: true}}, 0, 0, 1, "", &out, drainTestHooks(&map[string]int{}))
	if !result.Failed || strings.Contains(out.String(), "invalid standing lane") || !strings.Contains(out.String(), "FAC-182") {
		t.Fatalf("configured standing-worker was mishandled: %+v output=%s", result, out.String())
	}
	t.Setenv("HERD_DRAIN_REBASE_MAIL", "0")
	out.Reset()
	result = executeDrainActions(context.Background(), r, []drainActionEvidence{{SHA: "sha", Lane: "not-a-standing-lane", RebaseNeeded: true}}, 0, 0, 1, "", &out, drainTestHooks(&map[string]int{}))
	if result.Failed || result.Refusals != 0 || strings.Contains(out.String(), "rebase-mail") {
		t.Fatalf("disabled rebase produced refusal or side effect: %+v output=%s", result, out.String())
	}
}

func TestDrainActions_GatesAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, branch, family, want string
		veto, pending              bool
		wantReview                 int
	}{
		{name: "known", branch: "task/FAC-1", family: "anthropic", wantReview: 1},
		{name: "veto", branch: "task/FAC-1", family: "anthropic", veto: true, want: "vetoed SHA"},
		{name: "pending", branch: "task/FAC-1", family: "anthropic", pending: true, want: "duplicate pending"},
		{name: "unknown family", branch: "task/FAC-1", want: "unknown builder family"},
		{name: "forbidden", branch: "park/FAC-1", family: "anthropic", want: "forbidden branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := map[string]int{}
			var out bytes.Buffer
			e := drainActionEvidence{SHA: "sha-review", Branch: tc.branch, BuilderFamily: tc.family, Vetoed: tc.veto, Pending: tc.pending}
			result := executeDrainActions(context.Background(), drainTestReport(), []drainActionEvidence{e}, 1, 0, 0, "", &out, drainTestHooks(&calls))
			if result.Failed {
				t.Fatalf("unexpected action failure: %+v %s", result, out.String())
			}
			if calls["review"] != tc.wantReview {
				t.Fatalf("review calls=%d output=%s", calls["review"], out.String())
			}
			if tc.want != "" && !strings.Contains(out.String(), tc.want) {
				t.Fatalf("output=%s missing %q", out.String(), tc.want)
			}
		})
	}
}

func TestDrainActions_HarvestDryRunBeforeTierGate(t *testing.T) {
	calls := map[string]int{}
	var out bytes.Buffer
	r := &review.DrainReport{KaneoOK: true, Cap: 2, Shas: review.DrainShas{HarvestReady: []string{"sha"}}, HarvestReady: 1}
	e := drainActionEvidence{SHA: "sha", Lane: "lane", HarvestReady: true, Tier: "R1", TierRecorded: true, BuilderFamily: "anthropic"}
	result := executeDrainActions(context.Background(), r, []drainActionEvidence{e}, 0, 1, 0, "", &out, drainTestHooks(&calls))
	if result.Harvests != 0 || calls["dry-run"] != 1 || calls["harvest"] != 0 || !strings.Contains(out.String(), "auto-harvest tier") {
		t.Fatalf("unallowed tier was not dry-run-only: %+v calls=%v output=%s", result, calls, out.String())
	}
}

func TestDrainActions_ReviewHookUsesExactPinAndFailsClosed(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	hooks := drainTestHooks(&map[string]int{})
	hooks.launchReview = func(_ context.Context, e drainActionEvidence) error {
		calls++
		if e.Branch != "task/FAC-3" || e.BuilderFamily != "google" || e.SHA != "0123456789abcdef" {
			t.Fatalf("wrong review evidence: %+v", e)
		}
		return nil
	}
	r := drainTestReport()
	r.Shas.NeedReview = []string{"0123456789abcdef"}
	result := executeDrainActions(context.Background(), r, []drainActionEvidence{{SHA: "0123456789abcdef", Branch: "task/FAC-3", BuilderFamily: "google"}}, 1, 0, 0, "", &out, hooks)
	if result.Reviews != 1 || calls != 1 {
		t.Fatalf("review not launched: %+v calls=%d", result, calls)
	}
	hooks.launchReview = func(context.Context, drainActionEvidence) error { return errors.New("wrong ref") }
	result = executeDrainActions(context.Background(), r, []drainActionEvidence{{SHA: "0123456789abcdef", Branch: "task/FAC-3", BuilderFamily: "google"}}, 1, 0, 0, "", &out, hooks)
	if result.Reviews != 0 || !result.Failed {
		t.Fatalf("failed hook was counted: %+v", result)
	}
}

func TestDrainActions_FAC182RefusalNoSideEffects(t *testing.T) {
	state := t.TempDir()
	t.Setenv("HERD_STATE_DIR", state)
	var out bytes.Buffer
	calls := map[string]int{}
	r := drainTestReport()
	result := executeDrainActions(context.Background(), r, []drainActionEvidence{{SHA: "sha-rebase", Lane: "lane", RebaseNeeded: true}}, 0, 0, 1, "", &out, drainTestHooks(&calls))
	if !result.Failed || calls["mail"] != 0 || !strings.Contains(out.String(), "FAC-182") {
		t.Fatalf("rebase was not refused: %+v calls=%v output=%s", result, calls, out.String())
	}
	if entries, err := os.ReadDir(state); err != nil && !os.IsNotExist(err) {
		t.Fatalf("side-effect inspection failed: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("FAC-182 refusal created local state: %v", entries)
	}
}
func TestDrainDefaultActionsRefuseUntilFAC184(t *testing.T) {
	hooks := defaultDrainActionHooks()
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{name: "review", call: func() error { return hooks.launchReview(context.Background(), drainActionEvidence{SHA: "sha"}) }},
		{name: "dry-run", call: func() error { return hooks.dryRun(context.Background(), drainActionEvidence{SHA: "sha"}) }},
		{name: "harvest", call: func() error { return hooks.harvest(context.Background(), drainActionEvidence{SHA: "sha"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil || !strings.Contains(err.Error(), "FAC-184") {
				t.Fatalf("adapter did not fail closed: %v", err)
			}
		})
	}
}
