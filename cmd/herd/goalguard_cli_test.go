package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/goalguard"
	"github.com/Kampe/Herdforge/pkg/herdr"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

func TestGoalGuardCLISetCheckAndClear(t *testing.T) {
	// Positive genuine coordinator clear with verified caller process ancestry
	state := filepath.Join(t.TempDir(), "goal.json")
	claims := filepath.Join(t.TempDir(), "leases.db")
	t.Setenv("HERD_CLAIMS_DB", claims)
	leaseStore, err := claim.NewSQLiteLeaseStore(claims)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseStore.Close()
	lease, err := leaseStore.Acquire(context.Background(), claim.LeaseKey{Repo: "repo", Provider: "memory", Project: "project", TaskRef: "FAC-308"}, "coordinator", "coordinator", "", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", "%42")
	restoreHerdr := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"type":"agent_list","agents":[{"name":"coordinator","agent":"codex","agent_status":"working","pane_id":"%42","tab_id":"t1","agent_session":{"value":"01a072e0-7206-71b1-9f12-706158a4c4a6"}}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"%%42","shell_pid":%d,"foreground_process_group_id":%d,"foreground_processes":[{"pid":%d,"name":"go","argv":["go"]}]}}}`, os.Getpid(), os.Getpid(), os.Getpid()), nil
		}
		return "", errors.New("unexpected herdr command")
	})
	defer restoreHerdr()

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"herd", "goal-guard", "--set", "--state", state, "--lane", "forge-worker", "--task", "FAC-308", "--owner", "coordinator", "--generation", strconv.FormatInt(lease.Generation, 10), "--max", "1", "--grantor", "coordinator", "--packet", "packet.md", "--autonomy", "bounded", "--mutations", "worktree", "--forbidden", "merge", "--stop-conditions", "stop"}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}

	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()
	input, err := os.CreateTemp(t.TempDir(), "evidence-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.WriteString(fmt.Sprintf(`{"lane":"forge-worker","task":"FAC-308","owner":"coordinator","generation":%d,"lease_held":true,"now":"2026-08-16T02:00:00Z"}`, lease.Generation)); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	os.Stdin = input
	os.Args = []string{"herd", "goal-guard", "--check", "--state", state}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}

	os.Args = []string{"herd", "goal-guard", "--clear", "--state", state, "--grantor", "coordinator", "--generation", strconv.FormatInt(lease.Generation, 10)}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("clear left state behind: %v", err)
	}
}

func TestGoalGuardClearRefusesAgentAndStaleGrantorWithoutMutation(t *testing.T) {
	previousCaller := goalGuardCallerIdentity
	goalGuardCallerIdentity = func() (string, error) { return "coordinator", nil }
	defer func() { goalGuardCallerIdentity = previousCaller }()
	state := filepath.Join(t.TempDir(), "goal.json")
	claims := filepath.Join(t.TempDir(), "leases.db")
	t.Setenv("HERD_CLAIMS_DB", claims)
	leaseStore, err := claim.NewSQLiteLeaseStore(claims)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseStore.Close()
	lease, err := leaseStore.Acquire(context.Background(), claim.LeaseKey{Repo: "repo", Provider: "memory", Project: "project", TaskRef: "FAC-767"}, "native-owner", "worker", "", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := goalguard.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Set(goalguard.Goal{
		Lane: "standing", Task: "FAC-767", Owner: "coordinator", Generation: lease.Generation,
		CreatedAt: now, UpdatedAt: now,
		Authority: &goalguard.AuthorityEnvelope{Grantor: "coordinator", PacketPath: "packet.md", BoundedAutonomy: "bounded", MutationLimits: "worktree", ForbiddenActions: []string{"merge"}, StopConditions: []string{"stop"}},
	}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := clearGoal(s, "", 0, ""); err == nil || !strings.Contains(err.Error(), "agent-only clear") {
		t.Fatalf("agent clear error = %v, want refusal naming supported route", err)
	}
	if got, _ := os.ReadFile(state); string(got) != string(original) {
		t.Fatal("agent refusal mutated goal state")
	}
	if err := clearGoal(s, "coordinator", 8, ""); err == nil || !strings.Contains(err.Error(), "stale generation") {
		t.Fatalf("stale clear error = %v, want stale generation refusal", err)
	}
	if got, _ := os.ReadFile(state); string(got) != string(original) {
		t.Fatal("stale refusal mutated goal state")
	}
	if err := clearGoal(s, "coordinator", lease.Generation, ""); err == nil || !strings.Contains(err.Error(), "native claim owner/generation mismatch") {
		t.Fatalf("forged envelope clear error = %v, want native owner refusal", err)
	}
	if got, _ := os.ReadFile(state); string(got) != string(original) {
		t.Fatal("native-owner refusal mutated goal state")
	}
}

func TestGoalGuardClearRefusesMatchingClaimFromWrongNativeCaller(t *testing.T) {
	previousCaller := goalGuardCallerIdentity
	goalGuardCallerIdentity = func() (string, error) { return "different-native-agent", nil }
	defer func() { goalGuardCallerIdentity = previousCaller }()
	state := filepath.Join(t.TempDir(), "goal.json")
	claims := filepath.Join(t.TempDir(), "leases.db")
	t.Setenv("HERD_CLAIMS_DB", claims)
	leaseStore, err := claim.NewSQLiteLeaseStore(claims)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseStore.Close()
	lease, err := leaseStore.Acquire(context.Background(), claim.LeaseKey{Repo: "repo", Provider: "memory", Project: "project", TaskRef: "FAC-768"}, "coordinator", "coordinator", "", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := goalguard.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Set(goalguard.Goal{Lane: "standing", Task: "FAC-768", Owner: "coordinator", Generation: lease.Generation, CreatedAt: now, UpdatedAt: now, Authority: &goalguard.AuthorityEnvelope{Grantor: "coordinator", PacketPath: "packet.md", BoundedAutonomy: "bounded", MutationLimits: "worktree", ForbiddenActions: []string{"merge"}, StopConditions: []string{"stop"}}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := clearGoal(s, "coordinator", lease.Generation, ""); err == nil || !strings.Contains(err.Error(), "verified native caller") {
		t.Fatalf("wrong native caller error = %v", err)
	}
	if after, _ := os.ReadFile(state); string(after) != string(before) {
		t.Fatal("wrong native caller refusal mutated goal state")
	}
}

func TestPaneSpoofingAllowsUnauthorizedRetirement(t *testing.T) {
	// Unauthorized caller A attempts to spoof genuine coordinator B's HERDR_PANE_ID
	state := filepath.Join(t.TempDir(), "goal.json")
	claims := filepath.Join(t.TempDir(), "leases.db")
	t.Setenv("HERD_CLAIMS_DB", claims)
	leaseStore, err := claim.NewSQLiteLeaseStore(claims)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseStore.Close()
	lease, err := leaseStore.Acquire(context.Background(), claim.LeaseKey{Repo: "repo", Provider: "memory", Project: "project", TaskRef: "FAC-767"}, "coordinatorB", "coordinator", "", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := goalguard.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Set(goalguard.Goal{
		Lane: "standing", Task: "FAC-767", Owner: "coordinatorB", Generation: lease.Generation,
		CreatedAt: now, UpdatedAt: now,
		Authority: &goalguard.AuthorityEnvelope{Grantor: "coordinatorB", PacketPath: "packet.md", BoundedAutonomy: "bounded", MutationLimits: "worktree", ForbiddenActions: []string{"merge"}, StopConditions: []string{"stop"}},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}

	// Caller sets HERDR_PANE_ID=%10 (spoofing coordinatorB's pane), but caller's process PID is NOT in pane %10
	t.Setenv("HERDR_PANE_ID", "%10")
	restoreHerdr := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"type":"agent_list","agents":[{"name":"coordinatorB","agent":"codex","agent_status":"working","pane_id":"%10","tab_id":"t1","agent_session":{"value":"01a072e0-7206-71b1-9f12-706158a4c4a6"}}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			// Genuine pane %10 has shell PID 99999 and foreground PID 99998 (unrelated to caller process)
			return `{"result":{"process_info":{"pane_id":"%10","shell_pid":99999,"foreground_process_group_id":99998,"foreground_processes":[{"pid":99998,"name":"codex","argv":["codex"]}]}}}`, nil
		}
		return "", errors.New("unexpected herdr command")
	})
	defer restoreHerdr()

	err = clearGoal(s, "coordinatorB", lease.Generation, "")
	if err == nil {
		t.Fatal("spoofed HERDR_PANE_ID was accepted without verifying caller process ancestry")
	}
	if !strings.Contains(err.Error(), "caller process is not in pane") && !strings.Contains(err.Error(), "process ancestry") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if after, _ := os.ReadFile(state); string(after) != string(before) {
		t.Fatal("unauthorized retirement mutated or deleted goal file")
	}
	if retired, _ := s.HasRetirement(); retired {
		t.Fatal("unauthorized retirement recorded retirement evidence")
	}
}

func TestLocalCwdFabricatedReceiptAllowsGoalRetirement(t *testing.T) {
	// An agent operating inside its own writable worktree creates a synthetic completion receipt
	// and appends a synthetic entry to its local ./.herd/board-done.jsonl
	tmp := t.TempDir()
	state := filepath.Join(tmp, "goal.json")
	s, err := goalguard.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Set(goalguard.Goal{
		Lane: "standing", Task: "FAC-767", Owner: "worker", Generation: 1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}

	// Create synthetic receipt in tmp
	receiptPath := filepath.Join(tmp, "fake-receipt.json")
	fakeReceipt := hsync.CompletionReceipt{
		Version:           1,
		TaskRef:           "FAC-767",
		Verdict:           "PASS",
		IntegrationResult: hsync.IntegrationMerged,
		RepoID:            "file" + tmp,
		BaseSHA:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CandidateSHA:      "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		MergeSHA:          "cccccccccccccccccccccccccccccccccccccccc",
		PatchID:           "dddddddddddddddddddddddddddddddddddddddd",
		AcceptanceDigest:  "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		TaskID:            "task-1",
		ProviderRevision:  "rev-1",
		VerificationDigest: "ffffffffffffffffffffffffffffffffffffffff",
		RiskTier:          "R1",
		AuthorFamily:      "openai",
		ReviewerFamily:    "google",
		LeaseGeneration:   1,
	}
	fakeReceipt.Digest = fakeReceipt.ComputeDigest()
	rb, err := json.Marshal(fakeReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, rb, 0600); err != nil {
		t.Fatal(err)
	}

	// Create local .herd/board-done.jsonl in tmp
	localDoneDir := filepath.Join(tmp, ".herd")
	if err := os.MkdirAll(localDoneDir, 0700); err != nil {
		t.Fatal(err)
	}
	localDoneRecord := hsync.DoneRecord{
		Ref:              "FAC-767",
		ReceiptDigest:    fakeReceipt.Digest,
		ProviderReadback: "done",
	}
	ldb, err := json.Marshal(localDoneRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDoneDir, "board-done.jsonl"), append(ldb, '\n'), 0600); err != nil {
		t.Fatal(err)
	}

	err = clearGoal(s, "", 0, receiptPath)
	if err == nil {
		t.Fatal("locally fabricated receipt and board-done.jsonl bypassed goal retirement protection")
	}
	if after, _ := os.ReadFile(state); string(after) != string(before) {
		t.Fatal("fabricated receipt mutated or deleted goal file")
	}
	if retired, _ := s.HasRetirement(); retired {
		t.Fatal("fabricated receipt recorded retirement evidence")
	}
}

func TestGenuineCanonicalCompletionReceiptRetiresGoal(t *testing.T) {
	// A genuine merged PASS completion receipt against canonical repository state retires the goal
	repoDir := t.TempDir()
	state := filepath.Join(repoDir, "goal.json")
	t.Setenv("HERD_ROOT", repoDir)
	s, err := goalguard.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Set(goalguard.Goal{
		Lane: "standing", Task: "FAC-767", Owner: "worker", Generation: 1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Initialize genuine Git repo with an origin/main commit
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	exec.Command("git", "-C", repoDir, "init").Run()
	exec.Command("git", "-C", repoDir, "config", "user.name", "Herd Test").Run()
	exec.Command("git", "-C", repoDir, "config", "user.email", "test@herd.local").Run()
	testFile := filepath.Join(repoDir, "file.txt")
	os.WriteFile(testFile, []byte("hello\n"), 0600)
	exec.Command("git", "-C", repoDir, "add", ".").Run()
	exec.Command("git", "-C", repoDir, "commit", "-m", "initial commit").Run()
	baseOut, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(baseOut))

	os.WriteFile(testFile, []byte("hello world\n"), 0600)
	exec.Command("git", "-C", repoDir, "add", ".").Run()
	exec.Command("git", "-C", repoDir, "commit", "-m", "feature commit").Run()
	mergeOut, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	mergeSHA := strings.TrimSpace(string(mergeOut))
	patchID, err := hsync.PatchID(repoDir, mergeSHA)
	if err != nil {
		t.Fatal(err)
	}
	exec.Command("git", "-C", repoDir, "branch", "origin/main", "HEAD").Run()
	exec.Command("git", "-C", repoDir, "config", "remote.origin.url", "file://"+repoDir).Run()
	repoID, err := toolchild.RepositoryIdentity(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	// Create reduced completion receipt (proven post-merge)
	fakeReceipt := hsync.CompletionReceipt{
		Version:           1,
		ProvenanceMode:    hsync.ProvenanceReduced,
		PullRequest:       42,
		TaskRef:           "FAC-767",
		Verdict:           "PASS",
		IntegrationResult: hsync.IntegrationMerged,
		RepoID:            repoID,
		BaseSHA:           baseSHA,
		CandidateSHA:      mergeSHA,
		MergeSHA:          mergeSHA,
		PatchID:           patchID,
		VerificationDigest: "ffffffffffffffffffffffffffffffffffffffff",
		RiskTier:          "R1",
		AuthorFamily:      "openai",
		ReviewerFamily:    "google",
	}
	fakeReceipt.Digest = fakeReceipt.ComputeDigest()
	receiptPath := filepath.Join(repoDir, "receipt.json")
	rb, err := json.Marshal(fakeReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, rb, 0600); err != nil {
		t.Fatal(err)
	}

	// Create canonical Done record in repoDir/.herd/board-done.jsonl
	doneDir := filepath.Join(repoDir, ".herd")
	if err := os.MkdirAll(doneDir, 0700); err != nil {
		t.Fatal(err)
	}
	doneRecord := hsync.DoneRecord{
		Ref:              "FAC-767",
		ReceiptDigest:    fakeReceipt.Digest,
		ProviderReadback: "done",
	}
	db, err := json.Marshal(doneRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doneDir, "board-done.jsonl"), append(db, '\n'), 0600); err != nil {
		t.Fatal(err)
	}

	if err := clearGoal(s, "", 0, receiptPath); err != nil {
		t.Fatalf("genuine canonical completion receipt clear failed: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("goal file was not removed after genuine completion retirement")
	}
	if retired, err := s.HasRetirement(); err != nil || !retired {
		t.Fatalf("retirement record was not written: err=%v, retired=%v", err, retired)
	}
}

func TestGoalGuardCLIMalformedEvidenceFailsClosed(t *testing.T) {
	state := filepath.Join(t.TempDir(), "goal.json")
	oldArgs, oldStdin := os.Args, os.Stdin
	defer func() { os.Args, os.Stdin = oldArgs, oldStdin }()
	os.Args = []string{"herd", "goal-guard", "--check", "--state", state}
	input, err := os.CreateTemp(t.TempDir(), "evidence-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.WriteString(strings.Repeat("{", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	os.Stdin = input
	if err := runGoalGuard(); err == nil {
		t.Fatal("malformed evidence must fail closed")
	}
	_ = input.Close()
}

func TestGoalGuardStopHookAllowsStopWhenLeaseIsLost(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "goal.json")
	dbPath := filepath.Join(dir, ".herd", "launch-claims.db")
	t.Setenv("HERD_LEASE_DB", dbPath)

	store, err := claim.NewSQLiteLeaseStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireWithIdentity(context.Background(), claim.LeaseKey{
		Repo: "repo", Provider: "kaneo", Project: "project", TaskRef: "FAC-472",
	}, "owner-472", "worker", "", "repo", "worker", "forge-worker", time.Now(), time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, changed, err := store.Release(context.Background(), lease.LeaseKey, lease.OwnerID, lease.Generation, time.Now()); err != nil || !changed {
		_ = store.Close()
		t.Fatalf("release lease: changed=%v err=%v", changed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	oldArgs, oldStdin, oldStdout := os.Args, os.Stdin, os.Stdout
	defer func() { os.Args, os.Stdin, os.Stdout = oldArgs, oldStdin, oldStdout }()
	os.Args = []string{"herd", "goal-guard", "--set", "--state", state, "--lane", "forge-worker", "--task", "FAC-472", "--owner", "coordinator", "--generation", strconv.FormatInt(lease.Generation, 10), "--max", "1"}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}

	input, err := os.CreateTemp(dir, "stop-hook-input-*")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	os.Stdin = input
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	os.Args = []string{"herd", "goal-guard", "--stop-hook", "--state", state}
	if err := runGoalGuard(); err != nil {
		_ = write.Close()
		_ = read.Close()
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	var decision struct {
		Continue bool   `json:"continue"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(read).Decode(&decision); err != nil {
		t.Fatal(err)
	}
	if decision.Continue || decision.Reason != "lease_lost" {
		t.Fatalf("stop-hook decision = %+v, want non-continuing lease_lost", decision)
	}
}

// FAC-532: a goal recorded before authority envelopes existed is still an
// operator-granted goal. The hook must WARN loudly rather than return an
// error — a Stop hook that errors terminates the agent, so refusing here
// stranded every lane whose goal predated FAC-525. The FAC-525 invariant is
// preserved in that the hook never SILENTLY invents authority.
func TestGoalGuardStopHookWarnsButContinuesOnLegacyGrant(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "goal.json")
	dbPath := filepath.Join(dir, ".herd", "launch-claims.db")
	t.Setenv("HERD_LEASE_DB", dbPath)
	store, err := claim.NewSQLiteLeaseStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireWithIdentity(context.Background(), claim.LeaseKey{Repo: "repo", Provider: "kaneo", Project: "project", TaskRef: "FAC-525"}, "owner-525", "worker", "", "repo", "worker", "forge-worker", time.Now().UTC(), time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	defer store.Close()
	s, err := goalguard.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Set(goalguard.Goal{Lane: "forge-worker", Task: "FAC-525", Owner: "coordinator", Generation: lease.Generation, MaxContinuations: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	hookErr := runGoalGuardStopHook(s, nil)
	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldStdout, oldStderr
	stdout, _ := io.ReadAll(outR)
	stderr, _ := io.ReadAll(errR)
	_ = outR.Close()
	_ = errR.Close()

	// The whole point: NEVER an error. An erroring Stop hook kills the agent.
	if hookErr != nil {
		t.Fatalf("stop hook must never return an error (it terminates the agent), got %v", hookErr)
	}
	if !strings.Contains(string(stderr), "predates authority envelopes") {
		t.Fatalf("legacy grant must warn loudly, stderr=%q", stderr)
	}
	if !strings.Contains(string(stdout), `"decision": "block"`) && !strings.Contains(string(stdout), `"decision":"block"`) {
		t.Fatalf("legacy grant must still block a premature stop, stdout=%q", stdout)
	}
	if !strings.Contains(string(stdout), "AUTOMATED STOP-HOOK OUTPUT — NOT AN ASSIGNMENT") {
		t.Fatalf("stop-hook output must be distinguishable from assignments, stdout=%q", stdout)
	}
}

// FAC-581 correction (independent review finding 5): the production stop hook
// must feed the event-wait branch from the REAL observation path — the lane
// worktree's HEAD commit — and an unchanged HEAD must hold the lane without
// spending a continuation.
func TestGoalGuardStopHookEventWaitsOnUnchangedWorktreeHEAD(t *testing.T) {
	dir := t.TempDir()
	gitRun := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun("init")
	gitRun("config", "user.email", "t@example.com")
	gitRun("config", "user.name", "t")
	gitRun("add", "f.txt")
	gitRun("commit", "-m", "init")
	head1 := gitRun("rev-parse", "HEAD")

	// The Stop hook observes the cwd worktree; lease store absence is UNKNOWN
	// (held), so the goal stays active without a live lease db.
	t.Chdir(dir)
	t.Setenv("HERD_LEASE_DB", filepath.Join(dir, "launch-claims.db"))

	state := filepath.Join(dir, "goal.json")
	s, err := goalguard.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Set(goalguard.Goal{Lane: "forge-worker", Task: "FAC-581", Owner: "coordinator", Generation: 7, MaxContinuations: 5, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	runHook := func() string {
		t.Helper()
		oldStdout, oldStderr := os.Stdout, os.Stderr
		outR, outW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout, os.Stderr = outW, errW
		hookErr := runGoalGuardStopHook(s, []byte("{}"))
		_ = outW.Close()
		_ = errW.Close()
		os.Stdout, os.Stderr = oldStdout, oldStderr
		stdout, _ := io.ReadAll(outR)
		_ = outR.Close()
		_ = errR.Close()
		if hookErr != nil {
			t.Fatalf("stop hook must never return an error: %v", hookErr)
		}
		return string(stdout)
	}

	// First stop: the HEAD observation establishes the durable baseline and
	// spends one continuation, exactly as every pre-FAC-581 stop did.
	first := runHook()
	if !strings.Contains(first, `"decision": "block"`) && !strings.Contains(first, `"decision":"block"`) {
		t.Fatalf("active goal must block the stop, stdout=%q", first)
	}
	g, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if g.Continuations != 1 || g.Progress == nil || g.Progress.LastArtifact != head1 {
		t.Fatalf("first stop must record the observed HEAD %s and spend one continuation: continuations=%d progress=%+v", head1, g.Continuations, g.Progress)
	}

	// Second stop with NO new commit: an unchanged HEAD is an EVENT WAIT —
	// the lane is held (blocked) but the continuation budget is NOT spent.
	second := runHook()
	if !strings.Contains(second, "event wait") || !strings.Contains(second, "NOT spent") {
		t.Fatalf("unchanged HEAD must instruct a wait without spending budget, stdout=%q", second)
	}
	g, err = s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if g.Continuations != 1 {
		t.Fatalf("event wait must not spend a continuation: continuations=%d", g.Continuations)
	}

	// A new artifact (commit) resumes real work and spends budget again.
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", "f.txt")
	gitRun("commit", "-m", "work")
	third := runHook()
	if !strings.Contains(third, `"decision": "block"`) && !strings.Contains(third, `"decision":"block"`) {
		t.Fatalf("a lane with a new artifact must stay blocked on an unmet goal, stdout=%q", third)
	}
	g, err = s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if g.Continuations != 2 {
		t.Fatalf("a new artifact must spend a continuation: continuations=%d", g.Continuations)
	}
}
