package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewack"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// TestReviewRetirementCLIActingDrainTwice exercises the public cleanup command
// against a real Git worktree and a fake Herdr CLI. The second acting drain is
// the crash/retry boundary: it must replay the exact completion receipt without
// inspecting or touching a recycled resource.
func TestReviewRetirementCLIActingDrainTwice(t *testing.T) {
	root := t.TempDir()
	ownedChild := exec.Command("tail", "-f", "/dev/null")
	if err := ownedChild.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ownedChild.Process.Kill()
		_ = ownedChild.Wait()
	})
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte("version: \"1\"\nproject:\n  name: fixture\ntask_provider:\n  type: memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("protected source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "source.txt")
	run("-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "source")
	run("remote", "add", "origin", "https://example.invalid/fixture.git")
	shaOut, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))
	repository, err := dispatch.AuthenticatedRepositoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	poolRoot := filepath.Join(root, ".herd", "pool-fac708")
	slotPath := filepath.Join(poolRoot, "pool-01")
	if err := os.MkdirAll(poolRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", slotPath, "HEAD").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	lease, leaseGeneration := "cli-lease-1", int64(7)
	state := []byte(`{"version":1,"slots":[{"name":"pool-01","path":".herd/pool-fac708/pool-01","lease_id":"` + lease + `","leased_at":"1970-01-01T00:00:00.000000007Z","base":"HEAD"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolRoot, "pool.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	ref := "refs/herd/reviews/fac-708-" + sha[:12]
	run("update-ref", ref, sha)
	promptRel, manifestRel := ".herd/review/prompts/fac-708.md", ".herd/review/manifests/cli-1.json"
	if err := os.MkdirAll(filepath.Join(root, ".herd", "review", "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	prompt := []byte("review prompt owned by this launch\n")
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(promptRel)), prompt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd", "reviews"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(slotPath, filepath.Join(root, ".herd", "reviews", "fac-708")); err != nil {
		t.Fatal(err)
	}
	reviewer := "forge-mender-fac708-nat-d4b3b8dc"
	m := herdr.NewReviewRetirementManifest(time.Now(), herdr.ReviewRetirementManifest{Repository: repository, TaskRef: "FAC-708", TaskID: "task-708", CandidateSHA: sha, BaseSHA: sha, Branch: ref, Worktree: ".herd/pool-fac708/pool-01", Pool: ".herd/pool-fac708", Slot: "pool-01", LeaseGeneration: leaseGeneration, Workspace: "wK", TabID: "wK:t15T", PaneID: "wK:p15T", TerminalID: "term-fixture", SessionID: "session-fixture", Reviewer: reviewer, ReviewerFamily: "openai", ReviewerModel: "gpt-5.6-luna", PromptArtifact: promptRel, PromptDigest: reviewack.ArtifactDigest(prompt), Surface: ".herd/reviews/fac-708", ReviewRef: ref, ManifestArtifact: manifestRel, Generation: "cli-1", Nonce: lease})
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(manifestRel))), 0o700); err != nil {
		t.Fatal(err)
	}
	manifestBody, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifestRel)), append(manifestBody, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(root, ".herd", "review", "ledger.jsonl")
	rows := []string{`{"event":"record","sha":"` + sha + `","reviewer":"` + reviewer + `","lease":"` + lease + `","branch":"FAC-708"}`, `{"event":"verdict","sha":"` + sha + `","candidate_sha":"` + sha + `","reviewer":"` + reviewer + `","verdict":"PASS","artifact_digest":"artifact"}`}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reviewack.Emit(root, reviewack.Ack{SHA: sha, Reviewer: reviewer, LaunchIdentity: reviewer, ArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	registry := herdr.ReviewRetirementRegistry{Path: filepath.Join(root, ".herd", "review", "retirement-manifests.jsonl")}
	if err := registry.Record(m); err != nil {
		t.Fatal(err)
	}
	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "herdr")
	fakeGit := filepath.Join(fakeDir, "git")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is unavailable in this hermetic environment: %v", err)
	}
	gitWrapper := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "update-ref" ] && [ "${FAC708_FAIL_REF:-}" = "1" ]; then
    printf '%s\n' 'fixture injected update-ref failure' >&2
    exit 42
  fi
done
exec "${FAC708_REAL_GIT:?missing git}" "$@"
`
	if err := os.WriteFile(fakeGit, []byte(gitWrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	herdrState := filepath.Join(fakeDir, "herdr-state")
	herdrLog := filepath.Join(fakeDir, "herdr-log")
	closeCount := filepath.Join(fakeDir, "close-count")
	if err := os.WriteFile(herdrState, []byte("0\n0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
state="${FAC708_FAKE_STATE:?missing state file}"
	closed="$(sed -n '1p' "$state")"
	profile="${FAC708_FAKE_PROFILE:-dry}"
case "$1 $2" in
  "agent list") if [ "$profile" = "noop" ] || [ "$closed" = "1" ]; then printf '%s\n' '{"result":{"agents":[],"type":"agents"}}'; else printf '%s\n' '{"result":{"agents":[{"name":"forge-mender-fac708-nat-d4b3b8dc","agent_status":"idle","pane_id":"wK:p15T","tab_id":"wK:t15T","workspace_id":"wK","terminal_id":"term-fixture","focused":false,"agent_session":{"value":"session-fixture"}}],"type":"agents"}}'; fi ;;
  "workspace list") printf '%s\n' '{"result":{"workspaces":[{"workspace_id":"w","label":"fixture"}]}}' ;;
  "tab list") if [ "$closed" = "1" ]; then printf '%s\n' '{"result":{"tabs":[]}}'; else printf '%s\n' '{"result":{"tabs":[{"tab_id":"wK:t15T","workspace_id":"w","number":15,"pane_count":1,"focused":false}]}}'; fi ;;
  "pane process-info")
    if [ "${FAC708_FAKE_HERDR_ERROR:-}" = "1" ]; then printf '%s\n' '{"error":{"code":"transport_failed","message":"fixture tool failure"}}'; exit 1; fi
	    if [ "$closed" = "1" ]; then printf '%s\n' '{"error":{"code":"pane_not_found","message":"pane not found"}}'; exit 1; fi
	    count="$(sed -n '2p' "$state")"; count=$((count + 1)); printf '%s\n%s\n' "$closed" "$count" > "$state.tmp" && mv "$state.tmp" "$state"
	    printf 'live profile=%s count=%s\n' "$profile" "$count" >> "${FAC708_FAKE_LOG}"
    if [ "$profile" = "act1" ] && [ "$count" -ge 3 ]; then printf 'snapshot pid=%s\\n' "${FAC708_FAKE_PID}" >> "${FAC708_FAKE_LOG}"; printf '%s\n' '{"result":{"process_info":{"pane_id":"wK:p15T","shell_pid":'"${FAC708_FAKE_PID}"',"foreground_processes":[]}}}'; else printf '%s\n' '{"result":{"process_info":{"pane_id":"wK:p15T","shell_pid":0,"foreground_processes":[]}}}'; fi ;;
  "tab compare-close") printf '%s\n' 'unknown command: compare-close'; exit 1 ;;
  "tab close") closes="$(sed -n '1p' "${FAC708_FAKE_CLOSE_COUNT}")"; closes=$((closes + 1)); printf '%s\n' "$closes" > "${FAC708_FAKE_CLOSE_COUNT}"; printf '1\nclose\n' > "$state"; printf '%s\n' '{"result":{}}' ;;
  *) printf '%s\n' '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(closeCount, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), herdr.BinaryEnv+"="+fake, herdr.NoLiveEnv+"=1", "HERD_ROOT="+root, "HERD_REVIEW_LEDGER="+ledgerPath, "HERD_WORKSPACE=w", "FAC708_FAKE_STATE="+herdrState, "FAC708_FAKE_LOG="+herdrLog, "FAC708_FAKE_CLOSE_COUNT="+closeCount, "FAC708_FAKE_PID="+fmt.Sprint(ownedChild.Process.Pid), "PATH=/usr/bin:/bin")
	actingCalls := 0
	toolErrorRun := false
	runCleanup := func(args ...string) ([]byte, error) {
		cmd := exec.Command(buildHerd(t), append([]string{"cleanup"}, args...)...)
		cmd.Dir = root
		runEnv := append([]string(nil), env...)
		if len(args) > 0 && args[0] == "--act" && !toolErrorRun {
			profile := "act1"
			if actingCalls > 0 {
				profile = "act2"
			}
			actingCalls++
			runEnv = append(runEnv, "FAC708_FAKE_PROFILE="+profile)
		}
		cmd.Env = runEnv
		return cmd.CombinedOutput()
	}
	promptBefore, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(promptRel)))
	poolBefore, _ := os.ReadFile(filepath.Join(poolRoot, "pool.json"))
	if out, err := runCleanup("--json"); err != nil {
		t.Fatalf("default dry-run: %v\n%s", err, out)
	}
	if promptAfter, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(promptRel))); string(promptAfter) != string(promptBefore) {
		t.Fatal("default dry-run changed prompt")
	}
	if poolAfter, _ := os.ReadFile(filepath.Join(poolRoot, "pool.json")); string(poolAfter) != string(poolBefore) {
		t.Fatal("default dry-run changed pool lease")
	}
	env = append(env, "FAC708_FAKE_HERDR_ERROR=1")
	toolErrorRun = true
	if out, err := runCleanup("--act", "--json"); err == nil || !strings.Contains(string(out), "review retirement") {
		t.Fatalf("tool error must fail closed: err=%v out=%s", err, out)
	}
	toolErrorRun = false
	env = env[:len(env)-1]
	if out, err := func() ([]byte, error) {
		cmd := exec.Command(buildHerd(t), "drain", "--act", "--json", "--max-review", "0", "--max-harvest", "0")
		cmd.Dir = root
		cmd.Env = append(append([]string(nil), env...), "FAC708_FAKE_PROFILE=noop")
		return cmd.CombinedOutput()
	}(); err != nil {
		t.Fatalf("acting drain subprocess: %v\n%s", err, out)
	}
	// Inject a real Git boundary after close/lease/worktree mutation. The
	// following acting cleanup must resume from the durable ref-intent journal,
	// not inspect or delete a replacement pool incarnation.
	env = append(env, "FAC708_FAIL_REF=1", "FAC708_REAL_GIT="+gitPath, "PATH="+fakeDir+":/usr/bin:/bin")
	if out, err := runCleanup("--act", "--json"); err == nil || !strings.Contains(string(out), "update-ref") {
		t.Fatalf("injected ref boundary must fail publicly: err=%v out=%s", err, out)
	} else {
		t.Logf("injected ref boundary: %s", out)
	}
	env = env[:len(env)-3]
	for i := 0; i < 2; i++ {
		out, err := runCleanup("--act", "--json")
		if err != nil {
			state, _ := os.ReadFile(herdrState)
			log, _ := os.ReadFile(herdrLog)
			journal, _ := os.ReadFile(filepath.Join(root, ".herd", "review", "retirement-phases.jsonl"))
			t.Fatalf("acting cleanup %d: %v state=%q log=%q journal=%q\n%s", i+1, err, state, log, journal, out)
		}
		var packet map[string]any
		if err := json.Unmarshal(out, &packet); err != nil {
			t.Fatalf("cleanup %d json: %v\n%s", i+1, err, out)
		}
	}
	if got, _ := os.ReadFile(closeCount); strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("expected exactly one native close, got %q", got)
	}
	for _, rel := range []string{"source.txt", ".herd/review/ledger.jsonl", ".herd/review/acks/" + sha[:12] + "-" + reviewer + ".json", ".herd/review/retirement-manifests.jsonl", ".herd/review/retirement-phases.jsonl"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("preserved evidence %s: %v", rel, err)
		}
	}
	for _, rel := range []string{promptRel, manifestRel, ".herd/reviews/fac-708", ".herd/pool-fac708/pool-01", ".herd/pool-fac708"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("owned artifact %s remains or failed unexpectedly: %v", rel, err)
		}
	}
	if out, err := exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", ref).CombinedOutput(); err == nil {
		t.Fatalf("owned ref remains: %s", out)
	}
}

func TestRecordReviewRetirementManifest_RerunSameCandidateNewLeaseDoesNotCollide(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "source.txt")
	run("commit", "-m", "initial")
	shaBytes, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaBytes))
	run("branch", "origin/main", "main")

	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "test-project"},
	}
	task := &provider.Task{ID: "task-767", Ref: "FAC-767"}
	reviewer := poolReviewer{Family: "openai", Model: "gpt-5.6-luna"}
	agent := &herdr.AgentEntry{Session: herdr.AgentSession{Value: "ses_1"}}
	tab1 := herdr.TabInfo{ID: "wK:t1", Generation: "gen-1", Pane: herdr.PaneInfo{ID: "wK:p1", TerminalID: "term_1"}}
	packetPath := filepath.Join(root, ".herd", "review-packets", "p.md")
	_ = os.MkdirAll(filepath.Dir(packetPath), 0o700)
	_ = os.WriteFile(packetPath, []byte("prompt"), 0o600)
	surfacePath := filepath.Join(root, ".herd", "review-surfaces", "fac-767")
	_ = os.MkdirAll(filepath.Dir(surfacePath), 0o700)
	_ = os.WriteFile(surfacePath, []byte("surface"), 0o600)

	lease1 := &worktree.PoolSlot{
		Name: "pool-01", LeaseID: "pool-01-1000", LeasedAt: time.Now(),
		Path: filepath.Join(root, ".herd", "pool", "pool-01"),
	}
	err1 := recordReviewRetirementManifest(root, cfg, task, "FAC-767", sha, lease1, "wK", tab1, "review-fac-767-1", reviewer, packetPath, surfacePath, agent)
	if err1 != nil {
		t.Fatalf("first manifest record failed: %v", err1)
	}

	// Second review run on the same candidate SHA with a new lease (e.g. after rerun / retry)
	lease2 := &worktree.PoolSlot{
		Name: "pool-01", LeaseID: "pool-01-2000", LeasedAt: time.Now().Add(time.Second),
		Path: filepath.Join(root, ".herd", "pool", "pool-01"),
	}
	tab2 := herdr.TabInfo{ID: "wK:t2", Generation: "gen-2", Pane: herdr.PaneInfo{ID: "wK:p2", TerminalID: "term_2"}}
	err2 := recordReviewRetirementManifest(root, cfg, task, "FAC-767", sha, lease2, "wK", tab2, "review-fac-767-2", reviewer, packetPath, surfacePath, agent)
	if err2 != nil {
		t.Fatalf("second manifest record for same candidate with new lease must succeed without ref collision: %v", err2)
	}
}

// TestReviewRetirementCLI_ScopedReviewerOnlySparesUnrelatedCanaryAndForeignReviewer
// asserts that --only-reviewers with an exact --reviewer selector retires exclusively
// the target manifest-bound reviewer while leaving unrelated builder canaries (e.g. task-fac703c-1)
// and other foreign reviewer lanes completely untouched.
func TestReviewRetirementCLI_ScopedReviewerOnlySparesUnrelatedCanaryAndForeignReviewer(t *testing.T) {
	root := t.TempDir()
	ownedChild := exec.Command("tail", "-f", "/dev/null")
	if err := ownedChild.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ownedChild.Process.Kill()
		_ = ownedChild.Wait()
	})
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte("version: \"1\"\nproject:\n  name: fixture\ntask_provider:\n  type: memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("protected source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "source.txt")
	run("-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "source")
	run("remote", "add", "origin", "https://example.invalid/fixture.git")
	shaOut, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))
	repository, err := dispatch.AuthenticatedRepositoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Target Reviewer A: forge-mender-fac708-nat-d4b3b8dc (Slot 1)
	poolRoot := filepath.Join(root, ".herd", "pool-fac708")
	slotPathA := filepath.Join(poolRoot, "pool-01")
	slotPathB := filepath.Join(poolRoot, "pool-02")
	if err := os.MkdirAll(poolRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", slotPathA, "HEAD").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", slotPathB, "HEAD").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	leaseA, leaseGenerationA := "cli-lease-1", int64(7)
	leaseB, leaseGenerationB := "cli-lease-2", int64(8)
	state := []byte(`{"version":1,"slots":[{"name":"pool-01","path":".herd/pool-fac708/pool-01","lease_id":"` + leaseA + `","leased_at":"1970-01-01T00:00:00.000000007Z","base":"HEAD"},{"name":"pool-02","path":".herd/pool-fac708/pool-02","lease_id":"` + leaseB + `","leased_at":"1970-01-01T00:00:00.000000008Z","base":"HEAD"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolRoot, "pool.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}

	refA := "refs/herd/reviews/fac-708-" + sha[:12]
	refB := "refs/herd/reviews/fac-799-" + sha[:12]
	run("update-ref", refA, sha)
	run("update-ref", refB, sha)

	promptRelA, manifestRelA := ".herd/review/prompts/fac-708.md", ".herd/review/manifests/cli-1.json"
	promptRelB, manifestRelB := ".herd/review/prompts/fac-799.md", ".herd/review/manifests/cli-2.json"
	if err := os.MkdirAll(filepath.Join(root, ".herd", "review", "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	promptA := []byte("review prompt for A\n")
	promptB := []byte("review prompt for B\n")
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(promptRelA)), promptA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(promptRelB)), promptB, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd", "reviews"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(slotPathA, filepath.Join(root, ".herd", "reviews", "fac-708")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(slotPathB, filepath.Join(root, ".herd", "reviews", "fac-799")); err != nil {
		t.Fatal(err)
	}

	reviewerA := "forge-mender-fac708-nat-d4b3b8dc"
	reviewerB := "forge-reviewer-fac799-foreign"
	mA := herdr.NewReviewRetirementManifest(time.Now(), herdr.ReviewRetirementManifest{Repository: repository, TaskRef: "FAC-708", TaskID: "task-708", CandidateSHA: sha, BaseSHA: sha, Branch: refA, Worktree: ".herd/pool-fac708/pool-01", Pool: ".herd/pool-fac708", Slot: "pool-01", LeaseGeneration: leaseGenerationA, Workspace: "w", TabID: "w:t15T", PaneID: "w:p15T", TerminalID: "term-fixture-A", SessionID: "session-fixture-A", Reviewer: reviewerA, ReviewerFamily: "openai", ReviewerModel: "gpt-5.6-luna", PromptArtifact: promptRelA, PromptDigest: reviewack.ArtifactDigest(promptA), Surface: ".herd/reviews/fac-708", ReviewRef: refA, ManifestArtifact: manifestRelA, Generation: "cli-1", Nonce: leaseA})
	mB := herdr.NewReviewRetirementManifest(time.Now(), herdr.ReviewRetirementManifest{Repository: repository, TaskRef: "FAC-799", TaskID: "task-799", CandidateSHA: sha, BaseSHA: sha, Branch: refB, Worktree: ".herd/pool-fac708/pool-02", Pool: ".herd/pool-fac708", Slot: "pool-02", LeaseGeneration: leaseGenerationB, Workspace: "w", TabID: "w:t15B", PaneID: "w:p15B", TerminalID: "term-fixture-B", SessionID: "session-fixture-B", Reviewer: reviewerB, ReviewerFamily: "anthropic", ReviewerModel: "claude-3-7-sonnet", PromptArtifact: promptRelB, PromptDigest: reviewack.ArtifactDigest(promptB), Surface: ".herd/reviews/fac-799", ReviewRef: refB, ManifestArtifact: manifestRelB, Generation: "cli-2", Nonce: leaseB})

	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(manifestRelA))), 0o700); err != nil {
		t.Fatal(err)
	}
	bodyA, _ := json.Marshal(mA)
	bodyB, _ := json.Marshal(mB)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifestRelA)), append(bodyA, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifestRelB)), append(bodyB, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	ledgerPath := filepath.Join(root, ".herd", "review", "ledger.jsonl")
	rows := []string{
		`{"event":"record","sha":"` + sha + `","reviewer":"` + reviewerA + `","lease":"` + leaseA + `","branch":"FAC-708"}`,
		`{"event":"verdict","sha":"` + sha + `","candidate_sha":"` + sha + `","reviewer":"` + reviewerA + `","verdict":"PASS","artifact_digest":"artifact_a"}`,
		`{"event":"record","sha":"` + sha + `","reviewer":"` + reviewerB + `","lease":"` + leaseB + `","branch":"FAC-799"}`,
		`{"event":"verdict","sha":"` + sha + `","candidate_sha":"` + sha + `","reviewer":"` + reviewerB + `","verdict":"PASS","artifact_digest":"artifact_b"}`,
	}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reviewack.Emit(root, reviewack.Ack{SHA: sha, Reviewer: reviewerA, LaunchIdentity: reviewerA, ArtifactDigest: "artifact_a"}); err != nil {
		t.Fatal(err)
	}
	if err := reviewack.Emit(root, reviewack.Ack{SHA: sha, Reviewer: reviewerB, LaunchIdentity: reviewerB, ArtifactDigest: "artifact_b"}); err != nil {
		t.Fatal(err)
	}
	registry := herdr.ReviewRetirementRegistry{Path: filepath.Join(root, ".herd", "review", "retirement-manifests.jsonl")}
	if err := registry.Record(mA); err != nil {
		t.Fatal(err)
	}
	if err := registry.Record(mB); err != nil {
		t.Fatal(err)
	}

	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "herdr")
	closedTabsFile := filepath.Join(fakeDir, "closed-tabs.log")
	script := `#!/bin/sh
closed_log="${FAC708_CLOSED_LOG:?missing log}"
fake_pid="${FAC708_FAKE_PID:-0}"
tab_is_closed() {
  if [ -f "$closed_log" ] && grep -Fqx "$1" "$closed_log" 2>/dev/null; then
    return 0
  fi
  return 1
}

case "$1 $2" in
  "agent list")
    printf '%s\n' '{"result":{"agents":['
    first=1
    if ! tab_is_closed "w:t15A"; then
      printf '%s' '{"name":"task-fac703c-1","agent_status":"idle","pane_id":"w:p15A","tab_id":"w:t15A","workspace_id":"w","terminal_id":"term-canary","focused":false}'
      first=0
    fi
    if ! tab_is_closed "w:t15T"; then
      if [ "$first" -eq 0 ]; then printf ',\n'; fi
      printf '%s' '{"name":"forge-mender-fac708-nat-d4b3b8dc","agent_status":"idle","pane_id":"w:p15T","tab_id":"w:t15T","workspace_id":"w","terminal_id":"term-fixture-A","focused":false,"agent_session":{"value":"session-fixture-A"}}'
      first=0
    fi
    if ! tab_is_closed "w:t15B"; then
      if [ "$first" -eq 0 ]; then printf ',\n'; fi
      printf '%s' '{"name":"forge-reviewer-fac799-foreign","agent_status":"idle","pane_id":"w:p15B","tab_id":"w:t15B","workspace_id":"w","terminal_id":"term-fixture-B","focused":false,"agent_session":{"value":"session-fixture-B"}}'
      first=0
    fi
    printf '\n%s\n' '],"type":"agents"}}'
    ;;
  "workspace list") printf '%s\n' '{"result":{"workspaces":[{"workspace_id":"w","label":"fixture"}]}}' ;;
  "tab list")
    printf '%s\n' '{"result":{"tabs":['
    first=1
    for tab in "w:t15A" "w:t15T" "w:t15B"; do
      if ! tab_is_closed "$tab"; then
        if [ "$first" -eq 0 ]; then printf ',\n'; fi
        printf '{"tab_id":"%s"}' "$tab"
        first=0
      fi
    done
    printf '\n%s\n' ']}}'
    ;;
  "pane process-info")
    pane="$3"
    if [ "$pane" = "--pane" ]; then
      pane="$4"
    fi
    tab=""
    case "$pane" in
      "w:p15A") tab="w:t15A" ;;
      "w:p15T") tab="w:t15T" ;;
      "w:p15B") tab="w:t15B" ;;
    esac
    if [ -n "$tab" ] && tab_is_closed "$tab"; then
      printf '%s\n' '{"error":{"code":"pane_not_found","message":"pane not found"}}'
      exit 1
    fi
    pid=0
    if [ "$pane" = "w:p15T" ]; then
      pid="$fake_pid"
    fi
    printf '%s\n' '{"result":{"process_info":{"pane_id":"'"$pane"'","shell_pid":'"$pid"',"foreground_processes":[]}}}'
    ;;
  "tab compare-close")
    printf '%s\n' "$3" >> "$closed_log"
    printf '%s\n' '{"result":{"closed":true}}'
    ;;
  "tab close")
    printf '%s\n' "$3" >> "$closed_log"
    printf '%s\n' '{"result":{}}'
    ;;
  *) printf '%s\n' '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(closedTabsFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	env := append(os.Environ(),
		herdr.BinaryEnv+"="+fake,
		herdr.NoLiveEnv+"=1",
		"HERD_ROOT="+root,
		"HERD_REVIEW_LEDGER="+ledgerPath,
		"HERD_WORKSPACE=w",
		"FAC708_CLOSED_LOG="+closedTabsFile,
		"FAC708_FAKE_PID="+fmt.Sprint(ownedChild.Process.Pid),
		"PATH=/usr/bin:/bin",
	)

	runCleanup := func(args ...string) ([]byte, error) {
		cmd := exec.Command(buildHerd(t), append([]string{"cleanup"}, args...)...)
		cmd.Dir = root
		cmd.Env = env
		return cmd.CombinedOutput()
	}

	// 1. Dry run with --only-reviewers --reviewer forge-mender-fac708-nat-d4b3b8dc
	outDry, err := runCleanup("--dry-run", "--json", "--only-reviewers", "--reviewer", reviewerA)
	if err != nil {
		t.Fatalf("dry-run with reviewer selector: %v\n%s", err, outDry)
	}
	var pktDry map[string]interface{}
	if err := json.Unmarshal(outDry, &pktDry); err != nil {
		t.Fatalf("unmarshal dry-run: %v\n%s", err, outDry)
	}
	candsDry, _ := pktDry["candidates"].([]interface{})
	if len(candsDry) != 0 {
		t.Fatalf("broad candidates must be empty in reviewer-scoped cleanup, got %d: %v", len(candsDry), candsDry)
	}
	revPktDry, _ := pktDry["review_retirement"].(map[string]interface{})
	revCandsDry, _ := revPktDry["candidates"].([]interface{})
	if len(revCandsDry) != 1 {
		t.Fatalf("expected exactly 1 review retirement candidate in scoped dry run, got %d: %s", len(revCandsDry), outDry)
	}
	if gotClosed, _ := os.ReadFile(closedTabsFile); len(strings.TrimSpace(string(gotClosed))) != 0 {
		t.Fatalf("dry run must not close any tabs: %q", gotClosed)
	}

	// 2. Acting run with --act --only-reviewers --reviewer forge-mender-fac708-nat-d4b3b8dc
	outAct, err := runCleanup("--act", "--json", "--only-reviewers", "--reviewer", reviewerA)
	if err != nil {
		t.Fatalf("acting scoped cleanup: %v\n%s", err, outAct)
	}
	var pktAct map[string]interface{}
	if err := json.Unmarshal(outAct, &pktAct); err != nil {
		t.Fatalf("unmarshal acting output: %v\n%s", err, outAct)
	}
	attemptsAct, _ := pktAct["attempts"].([]interface{})
	if len(attemptsAct) != 0 {
		t.Fatalf("broad cleanup attempts must be empty in reviewer-scoped cleanup, got %d: %v", len(attemptsAct), attemptsAct)
	}
	if closedVal, ok := pktAct["closed"].(float64); ok && closedVal != 0 {
		t.Fatalf("broad closed count must be 0, got %v", closedVal)
	}
	revPktAct, _ := pktAct["review_retirement"].(map[string]interface{})
	if revPktAct["retired"].(float64) != 1 || revPktAct["failed"].(float64) != 0 {
		t.Fatalf("expected 1 retired, 0 failed in review retirement: %s", outAct)
	}

	// Verify tab closes: ONLY reviewer A tab (w:t15T) was closed; canary w:t15A and foreign reviewer w:t15B were SPARED.
	closedContent, _ := os.ReadFile(closedTabsFile)
	closedLines := strings.Fields(string(closedContent))
	if len(closedLines) != 1 || closedLines[0] != "w:t15T" {
		t.Fatalf("expected only tab w:t15T closed, got: %v (canary and foreign reviewer must not be closed)", closedLines)
	}

	// Verify filesystem: Reviewer A artifacts/worktree/ref removed, Reviewer B and source intact.
	for _, rel := range []string{"source.txt", promptRelB, manifestRelB, ".herd/reviews/fac-799", ".herd/pool-fac708/pool-02"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("spared foreign reviewer artifact %s missing: %v", rel, err)
		}
	}
	for _, rel := range []string{promptRelA, manifestRelA, ".herd/reviews/fac-708", ".herd/pool-fac708/pool-01"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("retired reviewer A artifact %s still exists: %v", rel, err)
		}
	}
	if out, err := exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", refA).CombinedOutput(); err == nil {
		t.Fatalf("retired reviewer A ref %s remains: %s", refA, out)
	}
	if out, err := exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", refB).CombinedOutput(); err != nil {
		t.Fatalf("spared foreign reviewer B ref %s was unexpectedly removed: %s", refB, out)
	}
}

func TestReviewRetirementCLI_UnknownSelectorFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "herdr")
	script := `#!/bin/sh
case "$1 $2" in
  "agent list") printf '%s\n' '{"result":{"agents":[{"name":"task-canary","agent_status":"idle","tab_id":"wK:t1"}],"type":"agents"}}' ;;
  "workspace list") printf '%s\n' '{"result":{"workspaces":[{"workspace_id":"w","label":"fixture"}]}}' ;;
  "tab close") printf 'closed\n' >&2; exit 1 ;;
  *) printf '%s\n' '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		herdr.BinaryEnv+"="+fake,
		herdr.NoLiveEnv+"=1",
		"HERD_ROOT="+root,
		"HERD_WORKSPACE=w",
		"PATH=/usr/bin:/bin",
	)

	cmd := exec.Command(buildHerd(t), "cleanup", "--act", "--reviewer", "non-existent-reviewer-1234")
	cmd.Dir = root
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("unknown reviewer selector must fail closed: %s", out)
	}
	if !strings.Contains(string(out), "no manifest matches selector") {
		t.Fatalf("error must explain missing selector match: %s", out)
	}
}
