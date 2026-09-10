package main

import (
	"context"
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
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestDrainReviewRetirementDefaultRefusal(t *testing.T) {
	hooks := defaultDrainActionHooks()
	if hooks.retireReviews == nil {
		t.Fatal("retireReviews is not set in defaultDrainActionHooks")
	}
	_, err := hooks.retireReviews(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no compiled review retirement authority is configured") {
		t.Fatalf("expected refusal error, got %v", err)
	}
}

func TestDrainReviewRetirementWiredInDrainAdapters(t *testing.T) {
	a := &drainAdapters{}
	hooks := a.hooks()
	if hooks.retireReviews == nil {
		t.Fatal("retireReviews is not wired in drainAdapters.hooks()")
	}
	_, err := hooks.retireReviews(context.Background())
	if err == nil || !strings.Contains(err.Error(), "review retirement authority is unavailable") {
		t.Fatalf("expected authority refusal, got %v", err)
	}
}

func TestDrainAdaptersRetireReviews_MalformedManifestFailsClosed(t *testing.T) {
	root := t.TempDir()
	herdDir := filepath.Join(root, ".herd", "review")
	if err := os.MkdirAll(herdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestsPath := filepath.Join(herdDir, "retirement-manifests.jsonl")
	if err := os.WriteFile(manifestsPath, []byte(`{"generation":"invalid-manifest"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &drainAdapters{
		root:       root,
		repository: "fixture-repo",
	}
	_, err := a.retireReviews(context.Background())
	if err == nil || !strings.Contains(err.Error(), "review manifest generation") {
		t.Fatalf("expected malformed manifest to fail closed, got %v", err)
	}
}

func TestDrainAdaptersRetireReviews_ActingDrainTwiceIdempotentAndSparesCanaryAndActive(t *testing.T) {
	root := t.TempDir()
	ownedChild := exec.Command("tail", "-f", "/dev/null")
	if err := ownedChild.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ownedChild.Process.Kill()
		_ = ownedChild.Wait()
	})

	if err := os.MkdirAll(filepath.Join(root, ".herd", "review", "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd", "review", "manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd", "reviews"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfgContent := "version: \"1\"\nproject:\n  name: fixture\ntask_provider:\n  type: memory\n"
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}

	runGit := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("protected source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "source.txt")
	runGit("-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "source")
	runGit("remote", "add", "origin", "https://example.invalid/fixture.git")

	shaOut, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))
	repository, err := dispatch.AuthenticatedRepositoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Setup Manifest A: Completed eligible review lane
	poolA := filepath.Join(root, ".herd", "pool-target")
	slotPathA := filepath.Join(poolA, "pool-01")
	if err := os.MkdirAll(poolA, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", slotPathA, "HEAD").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	leaseA := "lease-target-01"
	poolStateA := []byte(`{"version":1,"slots":[{"name":"pool-01","path":".herd/pool-target/pool-01","lease_id":"` + leaseA + `","leased_at":"1970-01-01T00:00:00.000000001Z","base":"HEAD"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolA, "pool.json"), poolStateA, 0o600); err != nil {
		t.Fatal(err)
	}
	refA := "refs/herd/reviews/fac-708-target"
	runGit("update-ref", refA, sha)
	promptRelA := ".herd/review/prompts/fac-708-target.md"
	promptBodyA := []byte("review prompt for target\n")
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(promptRelA)), promptBodyA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(slotPathA, filepath.Join(root, ".herd", "reviews", "fac-708-target")); err != nil {
		t.Fatal(err)
	}
	reviewerA := "forge-reviewer-target-a"
	manifestRelA := ".herd/review/manifests/target-a.json"
	mA := herdr.NewReviewRetirementManifest(time.Now(), herdr.ReviewRetirementManifest{
		Repository: repository, TaskRef: "FAC-708", TaskID: "task-target",
		CandidateSHA: sha, BaseSHA: sha, Branch: refA,
		Worktree: ".herd/pool-target/pool-01", Pool: ".herd/pool-target", Slot: "pool-01",
		LeaseGeneration: 1, Workspace: "wK", TabID: "wK:tTarget", PaneID: "wK:pTarget",
		TerminalID: "term-target", SessionID: "session-target",
		Reviewer: reviewerA, ReviewerFamily: "openai", ReviewerModel: "gpt-5.6-luna",
		PromptArtifact: promptRelA, PromptDigest: reviewack.ArtifactDigest(promptBodyA),
		Surface: ".herd/reviews/fac-708-target", ReviewRef: refA,
		ManifestArtifact: manifestRelA, Generation: "gen-target-a", Nonce: leaseA,
	})
	mBodyA, _ := json.Marshal(mA)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifestRelA)), append(mBodyA, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. Setup Manifest B: Active / Live review lane (working)
	poolB := filepath.Join(root, ".herd", "pool-active")
	slotPathB := filepath.Join(poolB, "pool-01")
	if err := os.MkdirAll(poolB, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", slotPathB, "HEAD").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	leaseB := "lease-active-01"
	poolStateB := []byte(`{"version":1,"slots":[{"name":"pool-01","path":".herd/pool-active/pool-01","lease_id":"` + leaseB + `","leased_at":"1970-01-01T00:00:00.000000002Z","base":"HEAD"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolB, "pool.json"), poolStateB, 0o600); err != nil {
		t.Fatal(err)
	}
	refB := "refs/herd/reviews/fac-708-active"
	runGit("update-ref", refB, sha)
	promptRelB := ".herd/review/prompts/fac-708-active.md"
	promptBodyB := []byte("review prompt for active\n")
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(promptRelB)), promptBodyB, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(slotPathB, filepath.Join(root, ".herd", "reviews", "fac-708-active")); err != nil {
		t.Fatal(err)
	}
	reviewerB := "forge-reviewer-active-b"
	manifestRelB := ".herd/review/manifests/active-b.json"
	mB := herdr.NewReviewRetirementManifest(time.Now(), herdr.ReviewRetirementManifest{
		Repository: repository, TaskRef: "FAC-708", TaskID: "task-active",
		CandidateSHA: sha, BaseSHA: sha, Branch: refB,
		Worktree: ".herd/pool-active/pool-01", Pool: ".herd/pool-active", Slot: "pool-01",
		LeaseGeneration: 2, Workspace: "wK", TabID: "wK:tActive", PaneID: "wK:pActive",
		TerminalID: "term-active", SessionID: "session-active",
		Reviewer: reviewerB, ReviewerFamily: "anthropic", ReviewerModel: "claude-sonnet-5",
		PromptArtifact: promptRelB, PromptDigest: reviewack.ArtifactDigest(promptBodyB),
		Surface: ".herd/reviews/fac-708-active", ReviewRef: refB,
		ManifestArtifact: manifestRelB, Generation: "gen-active-b", Nonce: leaseB,
	})
	mBodyB, _ := json.Marshal(mB)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifestRelB)), append(mBodyB, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. Setup Manifest C: Canary reviewer
	poolC := filepath.Join(root, ".herd", "pool-canary")
	slotPathC := filepath.Join(poolC, "pool-01")
	if err := os.MkdirAll(poolC, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", slotPathC, "HEAD").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	leaseC := "lease-canary-01"
	poolStateC := []byte(`{"version":1,"slots":[{"name":"pool-01","path":".herd/pool-canary/pool-01","lease_id":"` + leaseC + `","leased_at":"1970-01-01T00:00:00.000000003Z","base":"HEAD"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolC, "pool.json"), poolStateC, 0o600); err != nil {
		t.Fatal(err)
	}
	refC := "refs/herd/reviews/fac-703-canary"
	runGit("update-ref", refC, sha)
	promptRelC := ".herd/review/prompts/fac-703-canary.md"
	promptBodyC := []byte("review prompt for canary\n")
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(promptRelC)), promptBodyC, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(slotPathC, filepath.Join(root, ".herd", "reviews", "fac-703-canary")); err != nil {
		t.Fatal(err)
	}
	reviewerC := "task-fac703c-1"
	manifestRelC := ".herd/review/manifests/canary-c.json"
	mC := herdr.NewReviewRetirementManifest(time.Now(), herdr.ReviewRetirementManifest{
		Repository: repository, TaskRef: "FAC-703", TaskID: "task-fac703c-1",
		CandidateSHA: sha, BaseSHA: sha, Branch: refC,
		Worktree: ".herd/pool-canary/pool-01", Pool: ".herd/pool-canary", Slot: "pool-01",
		LeaseGeneration: 3, Workspace: "wK", TabID: "wK:tCanary", PaneID: "wK:pCanary",
		TerminalID: "term-canary", SessionID: "session-canary",
		Reviewer: reviewerC, ReviewerFamily: "google", ReviewerModel: "gemini-flash",
		PromptArtifact: promptRelC, PromptDigest: reviewack.ArtifactDigest(promptBodyC),
		Surface: ".herd/reviews/fac-703-canary", ReviewRef: refC,
		ManifestArtifact: manifestRelC, Generation: "gen-canary-c", Nonce: leaseC,
	})
	mBodyC, _ := json.Marshal(mC)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifestRelC)), append(mBodyC, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// Ledger & Acks: Record launches for all, but only Manifest A has terminal PASS + valid Ack
	ledgerPath := filepath.Join(root, ".herd", "review", "ledger.jsonl")
	rows := []string{
		`{"event":"record","sha":"` + sha + `","reviewer":"` + reviewerA + `","lease":"` + leaseA + `","branch":"FAC-708"}`,
		`{"event":"record","sha":"` + sha + `","reviewer":"` + reviewerB + `","lease":"` + leaseB + `","branch":"FAC-708"}`,
		`{"event":"record","sha":"` + sha + `","reviewer":"` + reviewerC + `","lease":"` + leaseC + `","branch":"FAC-703"}`,
		`{"event":"verdict","sha":"` + sha + `","candidate_sha":"` + sha + `","reviewer":"` + reviewerA + `","verdict":"PASS","artifact_digest":"target-artifact-digest"}`,
	}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reviewack.Emit(root, reviewack.Ack{SHA: sha, Reviewer: reviewerA, LaunchIdentity: reviewerA, ArtifactDigest: "target-artifact-digest"}); err != nil {
		t.Fatal(err)
	}

	// Record all manifests in retirement registry
	registry := herdr.ReviewRetirementRegistry{Path: filepath.Join(root, ".herd", "review", "retirement-manifests.jsonl")}
	if err := registry.Record(mA); err != nil {
		t.Fatal(err)
	}
	if err := registry.Record(mB); err != nil {
		t.Fatal(err)
	}
	if err := registry.Record(mC); err != nil {
		t.Fatal(err)
	}

	// Setup fake herdr CLI
	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "herdr")
	herdrState := filepath.Join(fakeDir, "herdr-state")
	closeCount := filepath.Join(fakeDir, "close-count")
	if err := os.WriteFile(herdrState, []byte("0\n0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(closeCount, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	pidStr := fmt.Sprint(ownedChild.Process.Pid)
	script := `#!/bin/sh
state="${FAC708_FAKE_STATE:?missing state file}"
closed="$(sed -n '1p' "$state")"
case "$1 $2" in
  "agent list")
    if [ "$closed" = "1" ]; then
      printf '%s\n' '{"result":{"agents":[{"name":"forge-reviewer-active-b","agent_status":"working","pane_id":"wK:pActive","tab_id":"wK:tActive","workspace_id":"wK","terminal_id":"term-active","focused":true,"agent_session":{"value":"session-active"}},{"name":"task-fac703c-1","agent_status":"idle","pane_id":"wK:pCanary","tab_id":"wK:tCanary","workspace_id":"wK","terminal_id":"term-canary","focused":false,"agent_session":{"value":"session-canary"}}],"type":"agents"}}'
    else
      printf '%s\n' '{"result":{"agents":[{"name":"forge-reviewer-target-a","agent_status":"idle","pane_id":"wK:pTarget","tab_id":"wK:tTarget","workspace_id":"wK","terminal_id":"term-target","focused":false,"agent_session":{"value":"session-target"}},{"name":"forge-reviewer-active-b","agent_status":"working","pane_id":"wK:pActive","tab_id":"wK:tActive","workspace_id":"wK","terminal_id":"term-active","focused":true,"agent_session":{"value":"session-active"}},{"name":"task-fac703c-1","agent_status":"idle","pane_id":"wK:pCanary","tab_id":"wK:tCanary","workspace_id":"wK","terminal_id":"term-canary","focused":false,"agent_session":{"value":"session-canary"}}],"type":"agents"}}'
    fi ;;
  "workspace list") printf '%s\n' '{"result":{"workspaces":[{"workspace_id":"wK","label":"fixture"}]}}' ;;
  "tab list")
    if [ "$closed" = "1" ]; then
      printf '%s\n' '{"result":{"tabs":[{"tab_id":"wK:tActive","workspace_id":"wK","number":16,"pane_count":1,"focused":true},{"tab_id":"wK:tCanary","workspace_id":"wK","number":17,"pane_count":1,"focused":false}]}}'
    else
      printf '%s\n' '{"result":{"tabs":[{"tab_id":"wK:tTarget","workspace_id":"wK","number":15,"pane_count":1,"focused":false},{"tab_id":"wK:tActive","workspace_id":"wK","number":16,"pane_count":1,"focused":true},{"tab_id":"wK:tCanary","workspace_id":"wK","number":17,"pane_count":1,"focused":false}]}}'
    fi ;;
  "pane process-info")
    if [ "$closed" = "1" ]; then
      printf '%s\n' '{"error":{"code":"pane_not_found","message":"pane not found"}}'; exit 1
    fi
    count="$(sed -n '2p' "$state")"
    count=$((count + 1))
    printf '%s\n%s\n' "$closed" "$count" > "$state.tmp" && mv "$state.tmp" "$state"
    if [ "$count" -ge 2 ]; then
      printf '{"result":{"process_info":{"pane_id":"wK:pTarget","shell_pid":%s,"foreground_processes":[]}}}\n' "${FAC708_FAKE_PID}"
    else
      printf '{"result":{"process_info":{"pane_id":"wK:pTarget","shell_pid":0,"foreground_processes":[]}}}\n'
    fi ;;
  "tab close")
    closes="$(sed -n '1p' "${FAC708_FAKE_CLOSE_COUNT}")"
    closes=$((closes + 1))
    printf '%s\n' "$closes" > "${FAC708_FAKE_CLOSE_COUNT}"
    printf '1\nclose\n' > "$state"
    printf '%s\n' '{"result":{}}' ;;
  *) printf '%s\n' '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv(herdr.BinaryEnv, fake)
	t.Setenv(herdr.NoLiveEnv, "1")
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REVIEW_LEDGER", ledgerPath)
	t.Setenv("HERD_WORKSPACE", "wK")
	t.Setenv("FAC708_FAKE_STATE", herdrState)
	t.Setenv("FAC708_FAKE_CLOSE_COUNT", closeCount)
	t.Setenv("FAC708_FAKE_PID", pidStr)

	ledger, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	adapters := &drainAdapters{
		root:       root,
		repository: repository,
		ledger:     ledger,
	}

	// TICK 1: First acting drain tick
	rep1, err := adapters.retireReviews(context.Background())
	if err != nil {
		t.Fatalf("first drain tick retireReviews failed: %v", err)
	}
	if rep1.Retired != 1 {
		t.Fatalf("expected 1 retired review in tick 1, got %d", rep1.Retired)
	}
	if rep1.Blocked != 2 {
		t.Fatalf("expected 2 blocked reviews (active + canary) in tick 1, got %d", rep1.Blocked)
	}

	// Assert Manifest A was cleanly retired:
	// 1. Tab closed exactly once
	closes1, _ := os.ReadFile(closeCount)
	if strings.TrimSpace(string(closes1)) != "1" {
		t.Fatalf("expected 1 tab close, got %s", closes1)
	}
	// 2. Lease released
	poolAState, _ := os.ReadFile(filepath.Join(poolA, "pool.json"))
	var pA struct {
		Slots []struct {
			LeaseID string `json:"lease_id"`
		} `json:"slots"`
	}
	_ = json.Unmarshal(poolAState, &pA)
	if len(pA.Slots) > 0 && pA.Slots[0].LeaseID != "" {
		t.Fatalf("expected leaseA released, got %s", pA.Slots[0].LeaseID)
	}
	// 3. Worktree removed
	if _, err := os.Stat(slotPathA); !os.IsNotExist(err) {
		t.Fatalf("expected worktreeA removed, err: %v", err)
	}
	// 4. Ref removed
	if out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", refA).CombinedOutput(); err == nil {
		t.Fatalf("expected refA deleted, got success (%s)", out)
	}
	// 5. Prompt removed
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(promptRelA))); !os.IsNotExist(err) {
		t.Fatalf("expected promptA removed, err: %v", err)
	}
	// 6. Retirement phases complete entry written
	phasesPath := filepath.Join(root, ".herd", "review", "retirement-phases.jsonl")
	phasesBytes, err := os.ReadFile(phasesPath)
	if err != nil {
		t.Fatalf("expected retirement phases file created: %v", err)
	}
	if !strings.Contains(string(phasesBytes), "gen-target-a") || !strings.Contains(string(phasesBytes), `"Phase":"complete"`) {
		t.Fatalf("expected gen-target-a complete entry in phases, got %s", phasesBytes)
	}

	// Assert Manifest B (active) and Manifest C (canary) survived intact!
	if _, err := os.Stat(slotPathB); err != nil {
		t.Fatalf("expected active worktreeB to survive, got %v", err)
	}
	if _, err := os.Stat(slotPathC); err != nil {
		t.Fatalf("expected canary worktreeC to survive, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(promptRelB))); err != nil {
		t.Fatalf("expected active promptB to survive, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(promptRelC))); err != nil {
		t.Fatalf("expected canary promptC to survive, got %v", err)
	}

	// TICK 2: Second acting drain tick (idempotence / replay check)
	rep2, err := adapters.retireReviews(context.Background())
	if err != nil {
		t.Fatalf("second drain tick retireReviews failed: %v", err)
	}
	if rep2.Retired != 0 {
		t.Fatalf("expected 0 newly retired reviews in tick 2 replay, got %d", rep2.Retired)
	}
	if rep2.Blocked != 2 {
		t.Fatalf("expected 2 blocked reviews (active + canary) in tick 2 replay, got %d", rep2.Blocked)
	}

	// Assert close count did NOT increase (remains 1)
	closes2, _ := os.ReadFile(closeCount)
	if strings.TrimSpace(string(closes2)) != "1" {
		t.Fatalf("expected second tick to not close additional tabs, got %s", closes2)
	}
	// Assert canary and active still intact
	if _, err := os.Stat(slotPathB); err != nil {
		t.Fatalf("expected active worktreeB to survive tick 2, got %v", err)
	}
	if _, err := os.Stat(slotPathC); err != nil {
		t.Fatalf("expected canary worktreeC to survive tick 2, got %v", err)
	}
}

func TestDrainAdaptersRetireReviews_MutationErrorObservable(t *testing.T) {
	root := t.TempDir()
	ownedChild := exec.Command("tail", "-f", "/dev/null")
	if err := ownedChild.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ownedChild.Process.Kill()
		_ = ownedChild.Wait()
	})

	if err := os.MkdirAll(filepath.Join(root, ".herd", "review"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfgContent := "version: \"1\"\nproject:\n  name: fixture\ntask_provider:\n  type: memory\n"
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}

	runGit := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "source.txt")
	runGit("-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "source")
	runGit("remote", "add", "origin", "https://example.invalid/fixture.git")

	shaOut, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))
	repository, err := dispatch.AuthenticatedRepositoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}

	poolA := filepath.Join(root, ".herd", "pool-fail")
	slotPathA := filepath.Join(poolA, "pool-01")
	if err := os.MkdirAll(poolA, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", slotPathA, "HEAD").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	leaseA := "lease-fail-01"
	poolStateA := []byte(`{"version":1,"slots":[{"name":"pool-01","path":".herd/pool-fail/pool-01","lease_id":"` + leaseA + `","leased_at":"1970-01-01T00:00:00.000000001Z","base":"HEAD"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolA, "pool.json"), poolStateA, 0o600); err != nil {
		t.Fatal(err)
	}
	refA := "refs/herd/reviews/fac-708-fail"
	runGit("update-ref", refA, sha)
	promptRelA := ".herd/review/prompts/fac-708-fail.md"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(promptRelA))), 0o700); err != nil {
		t.Fatal(err)
	}
	promptBodyA := []byte("review prompt for fail\n")
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(promptRelA)), promptBodyA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd", "reviews"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(slotPathA, filepath.Join(root, ".herd", "reviews", "fac-708-fail")); err != nil {
		t.Fatal(err)
	}
	reviewerA := "forge-reviewer-fail"
	manifestRelA := ".herd/review/manifests/fail.json"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(manifestRelA))), 0o700); err != nil {
		t.Fatal(err)
	}
	mA := herdr.NewReviewRetirementManifest(time.Now(), herdr.ReviewRetirementManifest{
		Repository: repository, TaskRef: "FAC-708", TaskID: "task-fail",
		CandidateSHA: sha, BaseSHA: sha, Branch: refA,
		Worktree: ".herd/pool-fail/pool-01", Pool: ".herd/pool-fail", Slot: "pool-01",
		LeaseGeneration: 1, Workspace: "wK", TabID: "wK:tFail", PaneID: "wK:pFail",
		TerminalID: "term-fail", SessionID: "session-fail",
		Reviewer: reviewerA, ReviewerFamily: "openai", ReviewerModel: "gpt-5.6-luna",
		PromptArtifact: promptRelA, PromptDigest: reviewack.ArtifactDigest(promptBodyA),
		Surface: ".herd/reviews/fac-708-fail", ReviewRef: refA,
		ManifestArtifact: manifestRelA, Generation: "gen-fail", Nonce: leaseA,
	})
	mBodyA, _ := json.Marshal(mA)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifestRelA)), append(mBodyA, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	ledgerPath := filepath.Join(root, ".herd", "review", "ledger.jsonl")
	rows := []string{
		`{"event":"record","sha":"` + sha + `","reviewer":"` + reviewerA + `","lease":"` + leaseA + `","branch":"FAC-708"}`,
		`{"event":"verdict","sha":"` + sha + `","candidate_sha":"` + sha + `","reviewer":"` + reviewerA + `","verdict":"PASS","artifact_digest":"fail-artifact-digest"}`,
	}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reviewack.Emit(root, reviewack.Ack{SHA: sha, Reviewer: reviewerA, LaunchIdentity: reviewerA, ArtifactDigest: "fail-artifact-digest"}); err != nil {
		t.Fatal(err)
	}
	registry := herdr.ReviewRetirementRegistry{Path: filepath.Join(root, ".herd", "review", "retirement-manifests.jsonl")}
	if err := registry.Record(mA); err != nil {
		t.Fatal(err)
	}

	// Fake herdr CLI that returns an error on tab close
	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "herdr")
	pidStr := fmt.Sprint(ownedChild.Process.Pid)
	script := `#!/bin/sh
case "$1 $2" in
  "agent list") printf '%s\n' '{"result":{"agents":[{"name":"forge-reviewer-fail","agent_status":"idle","pane_id":"wK:pFail","tab_id":"wK:tFail","workspace_id":"wK","terminal_id":"term-fail","focused":false,"agent_session":{"value":"session-fail"}}],"type":"agents"}}' ;;
  "workspace list") printf '%s\n' '{"result":{"workspaces":[{"workspace_id":"wK","label":"fixture"}]}}' ;;
  "tab list") printf '%s\n' '{"result":{"tabs":[{"tab_id":"wK:tFail","workspace_id":"wK","number":15,"pane_count":1,"focused":false}]}}' ;;
  "pane process-info") printf '%s\n' '{"result":{"process_info":{"pane_id":"'$3'","shell_pid":'` + pidStr + `,"foreground_processes":[]}}}' ;;
  "tab close") printf '%s\n' '{"error":{"code":"close_failed","message":"simulated tab close failure"}}'; exit 1 ;;
  *) printf '%s\n' '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv(herdr.BinaryEnv, fake)
	t.Setenv(herdr.NoLiveEnv, "1")
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REVIEW_LEDGER", ledgerPath)
	t.Setenv("HERD_WORKSPACE", "wK")

	ledger, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	adapters := &drainAdapters{
		root:       root,
		repository: repository,
		ledger:     ledger,
	}

	_, err = adapters.retireReviews(context.Background())
	if err == nil || !strings.Contains(err.Error(), "retire review gen-fail") {
		t.Fatalf("expected observable cleanup error, got %v", err)
	}
}

func TestDrainAdaptersRetireReviews_OriginMainAdvancedPostReleaseProof(t *testing.T) {
	root := t.TempDir()
	ownedChild := exec.Command("tail", "-f", "/dev/null")
	if err := ownedChild.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ownedChild.Process.Kill()
		_ = ownedChild.Wait()
	})

	if err := os.MkdirAll(filepath.Join(root, ".herd", "review", "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd", "review", "manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd", "reviews"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfgContent := "version: \"1\"\nproject:\n  name: fixture\ntask_provider:\n  type: memory\n"
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}

	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "source.txt")
	runGit("-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "initial base")
	oldBaseSHA := runGit("rev-parse", "HEAD")

	runGit("remote", "add", "origin", "https://example.invalid/fixture.git")
	runGit("update-ref", "refs/remotes/origin/main", oldBaseSHA)

	repository, err := dispatch.AuthenticatedRepositoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}

	// Create real pool
	poolRoot := filepath.Join(root, ".herd", "pool-fac792-drain")
	slotPath := filepath.Join(poolRoot, "pool-01")
	if err := os.MkdirAll(poolRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", slotPath, "HEAD").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	leaseID := "pool-01-1789037282935664000"
	poolState := []byte(`{"version":1,"slots":[{"name":"pool-01","path":".herd/pool-fac792-drain/pool-01","lease_id":"` + leaseID + `","leased_at":"2026-09-10T10:48:02.935664000Z","base":"origin/main"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(poolRoot, "pool.json"), poolState, 0o600); err != nil {
		t.Fatal(err)
	}

	// Candidate commit & review branch
	if err := os.WriteFile(filepath.Join(root, "cand.txt"), []byte("cand\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "cand.txt")
	runGit("-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "cand commit")
	candSHA := runGit("rev-parse", "HEAD")

	ref := "refs/herd/reviews/fac-792-41c2c7cb-pool-01"
	runGit("update-ref", ref, candSHA)

	// Set worktree to candSHA
	if out, err := exec.Command("git", "-C", slotPath, "reset", "--hard", candSHA).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}

	// Now advance origin/main to newMainSHA
	if err := os.WriteFile(filepath.Join(root, "newmain.txt"), []byte("new main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "newmain.txt")
	runGit("-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "new main commit")
	newMainSHA := runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", newMainSHA)

	promptRel := ".herd/review/prompts/fac-792.md"
	promptBody := []byte("review prompt for fac-792\n")
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(promptRel)), promptBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(slotPath, filepath.Join(root, ".herd", "reviews", "fac-792")); err != nil {
		t.Fatal(err)
	}

	reviewer := "review-fac-792-41c2c7cb"
	manifestRel := ".herd/review/manifests/pool-01-1789037282935664000.json"
	mA := herdr.NewReviewRetirementManifest(time.Now(), herdr.ReviewRetirementManifest{
		Repository: repository, TaskRef: "FAC-792", TaskID: "task-792",
		CandidateSHA: candSHA, BaseSHA: oldBaseSHA, Branch: ref, ReviewRef: ref,
		Worktree: ".herd/pool-fac792-drain/pool-01", Pool: ".herd/pool-fac792-drain", Slot: "pool-01",
		LeaseGeneration: 1789037282935664000, Workspace: "wK", TabID: "wK:t792", PaneID: "wK:p792",
		TerminalID: "term-792", SessionID: "session-792",
		Reviewer: reviewer, ReviewerFamily: "open-weight", ReviewerModel: "litellm/lazer/claude-haiku-4.5",
		PromptArtifact: promptRel, PromptDigest: reviewack.ArtifactDigest(promptBody),
		Surface: ".herd/reviews/fac-792",
		ManifestArtifact: manifestRel, Generation: "pool-01-1789037282935664000", Nonce: leaseID,
	})
	mBodyA, _ := json.Marshal(mA)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifestRel)), append(mBodyA, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// Ledger & Ack
	ledgerPath := filepath.Join(root, ".herd", "review", "ledger.jsonl")
	artifactDigest := "digest-fac-792-drain-test"
	rows := []string{
		`{"event":"record","sha":"` + candSHA + `","reviewer":"` + reviewer + `","lease":"` + leaseID + `","branch":"FAC-792"}`,
		`{"event":"verdict","sha":"` + candSHA + `","candidate_sha":"` + candSHA + `","reviewer":"` + reviewer + `","verdict":"PASS","artifact_digest":"` + artifactDigest + `"}`,
	}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reviewack.Emit(root, reviewack.Ack{SHA: candSHA, Reviewer: reviewer, LaunchIdentity: reviewer, ArtifactDigest: artifactDigest}); err != nil {
		t.Fatal(err)
	}

	registry := herdr.ReviewRetirementRegistry{Path: filepath.Join(root, ".herd", "review", "retirement-manifests.jsonl")}
	if err := registry.Record(mA); err != nil {
		t.Fatal(err)
	}

	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "herdr")
	herdrState := filepath.Join(fakeDir, "herdr-state")
	closeCount := filepath.Join(fakeDir, "close-count")
	if err := os.WriteFile(herdrState, []byte("0\n0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(closeCount, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	pidStr := fmt.Sprint(ownedChild.Process.Pid)
	script := `#!/bin/sh
state="${FAC708_FAKE_STATE:?missing state file}"
closed="$(sed -n '1p' "$state")"
case "$1 $2" in
  "agent list")
    if [ "$closed" = "1" ]; then
      printf '%s\n' '{"result":{"agents":[]}}'
    else
      printf '%s\n' '{"result":{"agents":[{"name":"review-fac-792-41c2c7cb","agent_status":"idle","pane_id":"wK:p792","tab_id":"wK:t792","workspace_id":"wK","terminal_id":"term-792","focused":false,"agent_session":{"value":"session-792"}}]}}'
    fi ;;
  "workspace list") printf '%s\n' '{"result":{"workspaces":[{"workspace_id":"wK","label":"fixture"}]}}' ;;
  "tab list")
    if [ "$closed" = "1" ]; then
      printf '%s\n' '{"result":{"tabs":[]}}'
    else
      printf '%s\n' '{"result":{"tabs":[{"tab_id":"wK:t792","workspace_id":"wK","number":19,"pane_count":1,"focused":false}]}}'
    fi ;;
  "pane process-info")
    if [ "$closed" = "1" ]; then
      printf '%s\n' '{"error":{"code":"pane_not_found","message":"pane not found"}}'; exit 1
    fi
    printf '{"result":{"process_info":{"pane_id":"wK:p792","shell_pid":%s,"foreground_processes":[]}}}\n' "${FAC708_FAKE_PID}" ;;
  "tab close")
    closes="$(sed -n '1p' "${FAC708_FAKE_CLOSE_COUNT}")"
    closes=$((closes + 1))
    printf '%s\n' "$closes" > "${FAC708_FAKE_CLOSE_COUNT}"
    printf '1\nclose\n' > "$state"
    printf '%s\n' '{"result":{}}' ;;
  *) printf '%s\n' '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv(herdr.BinaryEnv, fake)
	t.Setenv(herdr.NoLiveEnv, "1")
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REVIEW_LEDGER", ledgerPath)
	t.Setenv("HERD_WORKSPACE", "wK")
	t.Setenv("FAC708_FAKE_STATE", herdrState)
	t.Setenv("FAC708_FAKE_CLOSE_COUNT", closeCount)
	t.Setenv("FAC708_FAKE_PID", pidStr)

	ledger, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	adapters := &drainAdapters{
		root:       root,
		repository: repository,
		ledger:     ledger,
	}

	// First drain tick
	rep1, err := adapters.retireReviews(context.Background())
	if err != nil {
		t.Fatalf("first drain tick retireReviews failed: %v", err)
	}
	if rep1.Retired != 1 {
		t.Fatalf("expected 1 retired review in tick 1, got %d", rep1.Retired)
	}

	// Assert worktree retired
	if _, err := os.Stat(slotPath); !os.IsNotExist(err) {
		t.Fatalf("expected slotPath removed upon retirement, but exists")
	}

	// Second drain tick (idempotent replay)
	rep2, err := adapters.retireReviews(context.Background())
	if err != nil {
		t.Fatalf("second drain tick retireReviews failed: %v", err)
	}
	if rep2.Retired != 0 {
		t.Fatalf("expected 0 retired reviews in tick 2 replay, got %d", rep2.Retired)
	}
}

// TestDrainExecuteActions_ReviewRetirementBlockedObservabilityPreservedWithoutRefusal asserts
// that executeDrainActions surfaces individual blocked review retirement lanes and reasons
// into operator output and structured report without marking the drain tick as failed or
// treating safe holds as refusals.
func TestDrainExecuteActions_ReviewRetirementBlockedObservabilityPreservedWithoutRefusal(t *testing.T) {
	mockReport := herdr.ReviewRetirementReport{
		Retired: 1,
		Blocked: 1,
		Candidates: []herdr.ReviewRetirementCandidate{
			{
				Manifest: herdr.ReviewRetirementManifest{Generation: "gen-retired"},
				Decision: herdr.ReviewRetirementDecision{Eligible: true, Reason: "cleanly retired"},
			},
			{
				Manifest: herdr.ReviewRetirementManifest{Generation: "gen-blocked"},
				Decision: herdr.ReviewRetirementDecision{Eligible: false, Reason: "BLOCKED: review pane still active"},
			},
		},
	}

	hooks := drainActionHooks{
		launchReview: func(context.Context, drainActionEvidence) error { return nil },
		harvest:      func(context.Context, drainActionEvidence) error { return nil },
		retireReviews: func(context.Context) (herdr.ReviewRetirementReport, error) {
			return mockReport, nil
		},
	}

	var out strings.Builder
	result := executeDrainActions(context.Background(), drainTestReport(), nil, 0, 0, 0, "", &out, hooks)

	if result.Failed {
		t.Fatalf("drain action result must not fail when review lanes are safely blocked, got failed=true")
	}
	if result.Refusals != 0 {
		t.Fatalf("drain refusals must be 0 for safely blocked review lanes, got %d", result.Refusals)
	}
	if result.ReviewRetirements.Retired != 1 {
		t.Fatalf("expected 1 retired review in result, got %d", result.ReviewRetirements.Retired)
	}
	if result.ReviewRetirements.Blocked != 1 {
		t.Fatalf("expected 1 blocked review in result, got %d", result.ReviewRetirements.Blocked)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "RETIRED review generation=gen-retired") {
		t.Errorf("expected human output to report retired review, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "BLOCKED review-retirement generation=gen-blocked: BLOCKED: review pane still active") {
		t.Errorf("expected human output to report blocked review with reason, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "review_retired=1 review_blocked=1 refusals=0") {
		t.Errorf("expected summary line to carry review_retired=1 review_blocked=1 refusals=0, got:\n%s", outStr)
	}
}
