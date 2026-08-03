package worktree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFAC178_AmbientDotGuardRejectsRootMutation(t *testing.T) {
	fixtureRoot := t.TempDir()
	initRepo(t, fixtureRoot)
	wm := NewWorktreeManager(fixtureRoot)
	resolvedFixture, err := filepath.Abs(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	// This is the pre-operation guard that kills the NewWorktreeManager(".")
	// mutant before Git listing or any removal seam can run.
	if !sameWorktreePath(wm.RepoRoot, resolvedFixture) {
		t.Fatalf("fixture manager escaped disposable root: got %q want %q", wm.RepoRoot, resolvedFixture)
	}

	removals := 0
	wm.RemoveWorktreeFunc = func(context.Context, string) error {
		removals++
		t.Fatal("ambient-dot mutation reached removal seam")
		return nil
	}
	policy := fac178Policy(t, wm, ".")
	if _, err := wm.Reap(context.Background(), policy); err == nil {
		t.Fatal("ambient dot target must be refused")
	}
	if removals != 0 {
		t.Fatalf("ambient-dot guard reached removal %d times", removals)
	}
}

func TestFAC178_LeaseAcquiredBetweenPlanAndJITRefuses(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-LEASE")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge lease fixture", wi.Branch)
	runCmd(root, "git", "push", "origin", "main")

	removals, probeCalls := 0, 0
	wm.RemoveWorktreeFunc = func(context.Context, string) error { removals++; return nil }
	policy := fac178Policy(t, wm, wi.Path)
	policy.LeaseProbe = func(context.Context, string, string) (bool, error) {
		probeCalls++
		return probeCalls >= 2, nil // acquired between plan and JIT
	}
	report, err := wm.Reap(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if probeCalls < 2 || removals != 0 || len(report.Reaped) != 0 {
		t.Fatalf("lease JIT fence failed: probes=%d removals=%d reaped=%v", probeCalls, removals, report.Reaped)
	}
	if _, err := os.Stat(filepath.Join(wi.Path, ".git")); err != nil {
		t.Fatalf("leased worktree disappeared: %v", err)
	}
}

func TestFAC178_ConcurrentCommitBetweenPlanAndJITRefuses(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-COMMIT")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge commit fixture", wi.Branch)
	runCmd(root, "git", "push", "origin", "main")

	removals, probeCalls := 0, 0
	wm.RemoveWorktreeFunc = func(context.Context, string) error { removals++; return nil }
	policy := fac178Policy(t, wm, wi.Path)
	policy.LeaseProbe = func(context.Context, string, string) (bool, error) {
		probeCalls++
		if probeCalls == 2 {
			// This commit lands after planning and before JIT classification.
			runCmd(wi.Path, "git", "commit", "--allow-empty", "-m", "concurrent fixture")
			runCmd(root, "git", "checkout", "main")
			runCmd(root, "git", "merge", "--no-ff", "-m", "integrate concurrent fixture", wi.Branch)
			runCmd(root, "git", "push", "origin", "main")
			runCmd(root, "git", "fetch", "origin", "main")
			// Refresh only the external integration evidence; the planned HEAD
			// remains stale and must still fail the action binding.
			policy.Evidence.IntegrationSHA = gitOut(t, root, "rev-parse", "origin/main")
		}
		return false, nil
	}
	report, err := wm.Reap(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if probeCalls < 2 || removals != 0 || len(report.Reaped) != 0 {
		t.Fatalf("concurrent HEAD fence failed: probes=%d removals=%d reaped=%v", probeCalls, removals, report.Reaped)
	}
	if len(report.Refused) == 0 {
		t.Fatal("concurrent commit must produce a refusal receipt")
	}
	bindingRefused := false
	for _, candidate := range report.Refused {
		if strings.Contains(candidate.Reason, "action binding changed") {
			bindingRefused = true
		}
	}
	if !bindingRefused {
		t.Fatalf("concurrent commit did not trip the exact action binding: %+v", report.Refused)
	}
}

func TestFAC178_ReceiptsSerializePortableEvidence(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-RECEIPT")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge receipt fixture", wi.Branch)
	runCmd(root, "git", "push", "origin", "main")

	var durable bytes.Buffer
	policy := fac178Policy(t, wm, wi.Path)
	policy.ReceiptSink = func(receipt ReapReceipt) error {
		return json.NewEncoder(&durable).Encode(receipt)
	}
	if _, err := wm.Reap(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	if durable.Len() == 0 || strings.Contains(durable.String(), root) {
		t.Fatalf("receipt was not durable/portable: %q", durable.String())
	}
	var receipt ReapReceipt
	lines := bytes.Split(bytes.TrimSpace(durable.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected intent and terminal receipts, got %d: %q", len(lines), durable.String())
	}
	if err := json.Unmarshal(lines[len(lines)-1], &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Outcome != "removed" || receipt.Actor == "" || receipt.ActionPolicy != "remove" ||
		receipt.BoardEvidence == "" || receipt.LeaseGeneration == "" ||
		receipt.IntegrationSHA == "" || receipt.PolicyDigest == "" || receipt.SalvageRef == "" {
		t.Fatalf("incomplete durable receipt: %+v", receipt)
	}
}

func TestFAC178_ReceiptIntentPrecedesMutationAndSinkFailureFailsClosed(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-SINK")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge sink fixture", wi.Branch)
	runCmd(root, "git", "push", "origin", "main")

	removeCalls := 0
	wm.RemoveWorktreeFunc = func(context.Context, string) error { removeCalls++; return nil }
	var events []string
	policy := fac178Policy(t, wm, wi.Path)
	policy.ReceiptSink = func(receipt ReapReceipt) error {
		events = append(events, receipt.Outcome)
		if receipt.Outcome == "remove-intent" {
			return errors.New("durable sink unavailable")
		}
		return nil
	}
	if _, err := wm.Reap(context.Background(), policy); err == nil {
		t.Fatal("intent sink failure must be a hard error")
	}
	if removeCalls != 0 || len(events) != 1 || events[0] != "remove-intent" {
		t.Fatalf("sink failure was not fail-closed: events=%v removeCalls=%d", events, removeCalls)
	}
	if _, err := wm.revParse(context.Background(), SalvageRefFor(wi.Branch)); err == nil {
		t.Fatal("sink failure wrote salvage ref before durable intent")
	}

	// A successful sink proves the durable ordering: intent, then removal,
	// then terminal outcome. The injected removal seam makes this assertion
	// independent of filesystem deletion.
	events = nil
	wm.RemoveWorktreeFunc = func(context.Context, string) error {
		events = append(events, "remove")
		return nil
	}
	policy.ReceiptSink = func(receipt ReapReceipt) error {
		events = append(events, receipt.Outcome)
		return nil
	}
	if _, err := wm.Reap(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(events, ","), "remove-intent,remove,removed"; got != want {
		t.Fatalf("mutation ordering=%q want %q", got, want)
	}
}

func fac178Policy(t *testing.T, wm *WorktreeManager, target string) ReapPolicy {
	t.Helper()
	const board, generation, digest, actor = "board-proof-178", "generation-178", "policy-178", "fac-178-test"
	return ReapPolicy{
		DefaultBranch: "main", AutoReap: true, TargetPaths: []string{target},
		LeaseProbe:           func(context.Context, string, string) (bool, error) { return false, nil },
		LeaseGenerationProbe: func(context.Context, string, string) (string, error) { return generation, nil },
		BoardEvidenceProbe:   func(context.Context, string, string) (string, error) { return board, nil },
		ReceiptSink:          func(ReapReceipt) error { return nil },
		Evidence: ReapEvidence{
			IntegrationSHA: gitOut(t, wm.RepoRoot, "rev-parse", "origin/main"),
			BoardEvidence:  board, LeaseGeneration: generation, PolicyDigest: digest, Actor: actor,
		},
		ActionPolicy: "remove",
	}
}
