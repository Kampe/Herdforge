package main

import (
	"encoding/json"
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
	script := `#!/bin/sh
case "$1 $2" in
  "agent list") printf '%s\n' '{"result":{"agents":[],"type":"agents"}}' ;;
  "workspace list") printf '%s\n' '{"result":{"workspaces":[{"workspace_id":"w","label":"fixture"}]}}' ;;
  "tab list") printf '%s\n' '{"result":{"tabs":[]}}' ;;
  "pane process-info") if [ "${FAC708_FAKE_HERDR_ERROR:-}" = "1" ]; then printf '%s\n' '{"error":{"code":"transport_failed","message":"fixture tool failure"}}'; else printf '%s\n' '{"error":{"code":"pane_not_found","message":"pane not found"}}'; fi; exit 1 ;;
  *) printf '%s\n' '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), herdr.BinaryEnv+"="+fake, herdr.NoLiveEnv+"=1", "HERD_ROOT="+root, "HERD_REVIEW_LEDGER="+ledgerPath, "HERD_WORKSPACE=w", "PATH=/usr/bin:/bin")
	runCleanup := func(args ...string) ([]byte, error) {
		cmd := exec.Command(buildHerd(t), append([]string{"cleanup"}, args...)...)
		cmd.Dir = root
		cmd.Env = env
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
	if out, err := runCleanup("--act", "--json"); err == nil || !strings.Contains(string(out), "review retirement") {
		t.Fatalf("tool error must fail closed: err=%v out=%s", err, out)
	}
	env = env[:len(env)-1]
	if out, err := func() ([]byte, error) {
		cmd := exec.Command(buildHerd(t), "drain", "--act", "--json", "--max-review", "0", "--max-harvest", "0")
		cmd.Dir = root
		cmd.Env = env
		return cmd.CombinedOutput()
	}(); err != nil {
		t.Fatalf("acting drain subprocess: %v\n%s", err, out)
	}
	for i := 0; i < 2; i++ {
		out, err := runCleanup("--act", "--json")
		if err != nil {
			t.Fatalf("acting cleanup %d: %v\n%s", i+1, err, out)
		}
		var packet map[string]any
		if err := json.Unmarshal(out, &packet); err != nil {
			t.Fatalf("cleanup %d json: %v\n%s", i+1, err, out)
		}
	}
	for _, rel := range []string{"source.txt", ".herd/review/ledger.jsonl", ".herd/review/acks/" + sha[:12] + "-" + reviewer + ".json", ".herd/review/retirement-manifests.jsonl", ".herd/review/retirement-phases.jsonl"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("preserved evidence %s: %v", rel, err)
		}
	}
	for _, rel := range []string{promptRel, manifestRel, ".herd/reviews/fac-708"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("owned artifact %s remains or failed unexpectedly: %v", rel, err)
		}
	}
	if out, err := exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", ref).CombinedOutput(); err == nil {
		t.Fatalf("owned ref remains: %s", out)
	}
}
