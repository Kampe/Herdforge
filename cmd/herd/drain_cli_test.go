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
	"time"

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

func TestDrainJSONBoundsHangingScan(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	binary := buildHerd(t)
	repo := t.TempDir()
	binDir := t.TempDir()
	gitStub := filepath.Join(binDir, "git")
	if err := os.WriteFile(gitStub, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_DRAIN_TIMEOUT", "50ms")
	t.Setenv("HERD_REVIEW_LEDGER", filepath.Join(repo, "ledger.jsonl"))
	t.Setenv("HERD_STATE_DIR", filepath.Join(repo, "state"))
	if err := os.WriteFile(filepath.Join(repo, "ledger.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "drain", "--json")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("drain escaped its configured bound and hung: %v", err)
	}
	if err == nil || !strings.Contains(string(out), "bounded scan exceeded") {
		t.Fatalf("expected bounded scan error, err=%v output=%s", err, out)
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		t.Fatalf("json must exit 0: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	var packet map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &packet); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if len(packet) == 0 || packet["kaneo_ok"] == nil {
		t.Fatalf("missing fixed packet keys: %s", stdout.String())
	}
	for _, phase := range []string{"phase=harvest-scan", "phase=review-scan"} {
		if !strings.Contains(stderr.String(), phase) {
			t.Fatalf("stderr missing %q: %s", phase, stderr.String())
		}
	}
	// --act in a repo with no compiled authority (no .herd/herd.yaml, no board
	// provider) must refuse and exit non-zero without launching anything.
	actCmd := exec.Command(binary, "drain", "--act")
	actCmd.Dir, actCmd.Env = repo, env
	actOut, actErr := actCmd.CombinedOutput()
	if actErr == nil || !strings.Contains(string(actOut), "REFUSED --act") || strings.Contains(string(actOut), "act_reviews=1") {
		t.Fatalf("--act did not fail closed without side effects: %v\n%s", actErr, actOut)
	}

	// The selftest exercises the compiled adapters hermetically: it passes in a
	// checkout with an empty bin directory and no fleet, and still reports the
	// FAC-182 rebase-delivery block honestly.
	cmd = exec.Command(binary, "drain", "--selftest")
	cmd.Dir = repo
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "selftest: PASS") || !strings.Contains(string(out), "FAC-182") {
		t.Fatalf("selftest did not complete against compiled adapters: %v\n%s", err, out)
	}
	for _, banned := range []string{"bin/herd-review", "bin/herd-harvest-merge", "chainseer"} {
		if strings.Contains(string(out), banned) {
			t.Fatalf("selftest depends on %q: %s", banned, out)
		}
	}

	cmd = exec.Command(binary, "drain", "--max-review", "-1")
	cmd.Dir = repo
	if out, err = cmd.CombinedOutput(); err == nil {
		t.Fatalf("negative bounds must fail: %s", out)
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("negative bound exit=%v output=%s", err, out)
	}
}

func TestDrainCLI_ProgressUsesStderrAndQuietSuppressesIt(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	binary := buildHerd(t)
	repo := t.TempDir()
	runGitT(t, repo, "init", "-q", "-b", "main")
	runGitT(t, repo, "config", "user.email", "drain-progress@test")
	runGitT(t, repo, "config", "user.name", "drain-progress")
	runGitT(t, repo, "commit", "--allow-empty", "-q", "-m", "base")
	runGitT(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	ledger := filepath.Join(repo, "ledger.jsonl")
	if err := os.WriteFile(ledger, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HERD_REVIEW_LEDGER="+ledger, "HERD_STATE_DIR="+filepath.Join(repo, "state"))

	run := func(args ...string) (string, string, error) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir, cmd.Env = repo, env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	for _, tc := range []struct {
		name  string
		args  []string
		check func(t *testing.T, stdout string)
	}{
		{
			name: "normal",
			args: []string{"drain"},
			check: func(t *testing.T, stdout string) {
				t.Helper()
				if !strings.Contains(stdout, "=== server faster: review pile ===") {
					t.Fatalf("normal report missing from stdout: %s", stdout)
				}
			},
		},
		{
			name: "json",
			args: []string{"drain", "--json"},
			check: func(t *testing.T, stdout string) {
				t.Helper()
				var packet map[string]json.RawMessage
				if err := json.Unmarshal([]byte(stdout), &packet); err != nil || packet["kaneo_ok"] == nil {
					t.Fatalf("stdout is not the fixed JSON packet: %v\n%s", err, stdout)
				}
			},
		},
		{
			name: "commands",
			args: []string{"drain", "--commands"},
			check: func(t *testing.T, stdout string) {
				t.Helper()
				if !strings.Contains(stdout, "=== server faster: review pile ===") {
					t.Fatalf("commands report missing from stdout: %s", stdout)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := run(tc.args...)
			if tc.name == "json" && err != nil {
				t.Fatalf("JSON drain must exit 0: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
			}
			tc.check(t, stdout)
			if strings.Contains(stdout, "phase=") {
				t.Fatalf("phase progress leaked to stdout: %s", stdout)
			}
			for _, phase := range []string{"phase=harvest-scan", "phase=review-scan"} {
				if !strings.Contains(stderr, phase) {
					t.Fatalf("stderr missing %q: %s", phase, stderr)
				}
			}
		})
	}

	stdout, stderr, _ := run("drain", "--quiet")
	if !strings.Contains(stdout, "herd-drain: pressure=") {
		t.Fatalf("quiet summary missing from stdout: %s", stdout)
	}
	if strings.Contains(stderr, "phase=") {
		t.Fatalf("quiet output must suppress phase progress: %s", stderr)
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
	if !strings.Contains(text, "# REFUSED review veto-sha: vetoed SHA") || !strings.Contains(text, "herd drain --act --max-review 1") || !strings.Contains(text, "pin=review-sha") {
		t.Fatalf("commands lost evidence gates: %s", text)
	}
	if strings.Contains(text, "<branch>") || strings.Contains(text, "bin/herd-review") || strings.Contains(text, "bin/herd-harvest-merge") || strings.Contains(text, "zsh") || !strings.Contains(text, "harvest lane=lane-1") {
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
func TestDrainDefaultActionsRefuseWithoutAuthority(t *testing.T) {
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
			if err := tc.call(); err == nil || !strings.Contains(err.Error(), "no compiled") {
				t.Fatalf("unwired hook did not fail closed: %v", err)
			}
		})
	}
}

// FAC-645: rebase-mail delivery is unimplemented -- the branch printed the
// FAC-182 refusal without consulting any delivery path, and there is no success
// path in executeDrainActions at all. Reported per candidate, one missing
// capability inflated a beat that already carried 905 refusals, which is how a
// 1327-tip scan read as a fleet-wide failure instead of one unbuilt feature.
func TestDrainRebaseMailCollapsesToOneRefusalForManyCandidates(t *testing.T) {
	r := drainTestReport()
	r.StandingLanes = []string{"standing-worker"}
	ev := make([]drainActionEvidence, 0, 7)
	for _, sha := range []string{"aaaaaaaaaaaa1", "aaaaaaaaaaaa2", "aaaaaaaaaaaa3", "aaaaaaaaaaaa4", "aaaaaaaaaaaa5", "aaaaaaaaaaaa6", "aaaaaaaaaaaa7"} {
		ev = append(ev, drainActionEvidence{SHA: sha, Lane: "standing-worker", RebaseNeeded: true})
	}
	var out bytes.Buffer
	result := executeDrainActions(context.Background(), r, ev, 0, 0, 1, "", &out, drainTestHooks(&map[string]int{}))
	text := out.String()

	if result.Refusals != 1 {
		t.Errorf("7 stranded candidates must be one refusal for one missing capability, got %d", result.Refusals)
	}
	if !result.Failed {
		t.Error("the run must still fail: rebase-needed candidates stay stuck until delivery exists")
	}
	if strings.Count(text, "REFUSED rebase-mail") != 1 {
		t.Errorf("expected exactly one rebase-mail refusal line:\n%s", text)
	}
	if !strings.Contains(text, "x7 candidates") || !strings.Contains(text, "not 7 bad candidates") {
		t.Errorf("the line must report the affected count and exonerate the candidates:\n%s", text)
	}
	if !strings.Contains(text, "rebase_blocked=7") {
		t.Errorf("the summary must carry the stranded count so it is not invisible:\n%s", text)
	}
	// The relaunch bound was silently truncating which candidates were even
	// mentioned; say so rather than letting it hide the true total.
	if !strings.Contains(text, "relaunch bound 1 would have truncated") {
		t.Errorf("a truncating bound must be stated:\n%s", text)
	}
}
