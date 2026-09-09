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

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/reviewack"
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
	state := []byte(`{"version":1,"slots":[{"name":"pool-01","path":"` + slotPath + `","lease_id":"` + lease + `","leased_at":"1970-01-01T00:00:00.000000007Z","base":"HEAD"}]}` + "\n")
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
		t.Fatal(err)
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
