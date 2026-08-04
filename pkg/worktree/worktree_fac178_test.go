package worktree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestFAC178_DryRunReceiptOutcomesMatchEligibility(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	merged, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-DRY-MERGED")
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-DRY-DIRTY")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge dry receipt fixture", merged.Branch)
	runCmd(root, "git", "push", "origin", "main")
	if err := os.WriteFile(filepath.Join(dirty.Path, "dirty.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}
	report, err := wm.Reap(context.Background(), ReapPolicy{DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]ReapReceipt{}
	for _, receipt := range report.Receipts {
		byTarget[receipt.Branch] = receipt
	}
	if byTarget["main"].Outcome != "refused" || byTarget[dirty.Branch].Outcome != "refused" || byTarget[merged.Branch].Outcome != "planned" {
		t.Fatalf("dry-run receipt outcomes do not match eligibility: %+v", byTarget)
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
	if receipt.Outcome != "removed" || receipt.HEAD == "" || receipt.Actor == "" || receipt.ActionPolicy != "remove" ||
		receipt.BoardEvidence == "" || receipt.LeaseGeneration == "" ||
		receipt.IntegrationSHA == "" || receipt.PolicyDigest == "" || receipt.SalvageRef == "" || !receipt.EvidenceObserved {
		t.Fatalf("incomplete durable receipt: %+v", receipt)
	}
}

func TestFAC178_InitialRefusalSinkPrecedesEligibleMutation(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	merged, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-INITIAL-MERGED")
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-INITIAL-DIRTY")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge initial fixture", merged.Branch)
	runCmd(root, "git", "push", "origin", "main")
	if err := os.WriteFile(filepath.Join(dirty.Path, "dirty.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}

	removals := 0
	wm.RemoveWorktreeFunc = func(context.Context, string) error { removals++; return nil }
	policy := fac178Policy(t, wm, merged.Path)
	policy.TargetPaths = []string{merged.Path, dirty.Path}
	policy.ReceiptSink = func(receipt ReapReceipt) error {
		if receipt.Outcome == "refused" {
			return errors.New("initial refusal sink unavailable")
		}
		return nil
	}
	if _, err := wm.Reap(context.Background(), policy); err == nil {
		t.Fatal("initial refusal sink failure must be hard")
	}
	if removals != 0 {
		t.Fatalf("eligible target mutated after initial refusal sink failure: %d", removals)
	}
}

func TestFAC178_RefusalReceiptRedactsProbePaths(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-REDACT")
	if err != nil {
		t.Fatal(err)
	}
	var durable bytes.Buffer
	policy := fac178Policy(t, wm, wi.Path)
	policy.LeaseProbe = func(context.Context, string, string) (bool, error) {
		return false, fmt.Errorf("lease read failed at %s", root)
	}
	policy.ReceiptSink = func(receipt ReapReceipt) error {
		return json.NewEncoder(&durable).Encode(receipt)
	}
	if _, err := wm.Reap(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	serialized := durable.String()
	if strings.Contains(serialized, root) || strings.Contains(serialized, "lease read failed") {
		t.Fatalf("portable refusal leaked diagnostic path/text: %q", serialized)
	}
	var receipt ReapReceipt
	if err := json.Unmarshal(bytes.TrimSpace(durable.Bytes()), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Branch != wi.Branch || receipt.HEAD == "" || receipt.ReasonCode != "unknown-evidence" || receipt.Actor == "" ||
		receipt.IntegrationSHA != "" || receipt.BoardEvidence != "" || receipt.LeaseGeneration != "" || receipt.EvidenceObserved {
		t.Fatalf("refusal receipt lost exact portable evidence: %+v", receipt)
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
	wm.RemoveWorktreeFunc = func(_ context.Context, path string) error {
		events = append(events, "remove")
		runCmd(root, "git", "worktree", "remove", "--force", path)
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

func TestFAC178_NoOpRemoveCannotProduceRemoved(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-NOOP")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge noop fixture", wi.Branch)
	runCmd(root, "git", "push", "origin", "main")

	var outcomes []string
	policy := fac178Policy(t, wm, wi.Path)
	wm.RemoveWorktreeFunc = func(context.Context, string) error { return nil }
	policy.ReceiptSink = func(receipt ReapReceipt) error {
		outcomes = append(outcomes, receipt.Outcome)
		return nil
	}
	if _, err := wm.Reap(context.Background(), policy); err == nil {
		t.Fatal("no-op remove must not report success")
	}
	if containsString(outcomes, "removed") || !containsString(outcomes, "unverified") {
		t.Fatalf("no-op remove fabricated terminal success: %v", outcomes)
	}
}

func TestFAC178_NonForceRemovalRefusesLateDirtyWrite(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-LATE-DIRTY")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge late dirty fixture", wi.Branch)
	runCmd(root, "git", "push", "origin", "main")
	var outcomes []string
	policy := fac178Policy(t, wm, wi.Path)
	policy.ReceiptSink = func(receipt ReapReceipt) error { outcomes = append(outcomes, receipt.Outcome); return nil }
	wm.BeforeRemoveFunc = func(context.Context, string) error {
		return os.WriteFile(filepath.Join(wi.Path, "late-dirty.txt"), []byte("late"), 0644)
	}
	if _, err := wm.Reap(context.Background(), policy); err != nil {
		// Removal failure is a durable refusal, not a successful reap.
		if !containsString(outcomes, "refused") {
			t.Fatalf("late dirty failure lacked refusal receipt: %v", outcomes)
		}
	}
	if containsString(outcomes, "removed") {
		t.Fatalf("late dirty write fabricated removed outcome: %v", outcomes)
	}
	if _, err := os.Stat(filepath.Join(wi.Path, ".git")); err != nil {
		t.Fatalf("late dirty worktree was destroyed: %v", err)
	}
}

func TestFAC178_FinalBoundHEADRefusesLateCommit(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-BOUND-HEAD")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge bound HEAD fixture", wi.Branch)
	runCmd(root, "git", "push", "origin", "main")
	var outcomes []string
	policy := fac178Policy(t, wm, wi.Path)
	policy.ReceiptSink = func(receipt ReapReceipt) error { outcomes = append(outcomes, receipt.Outcome); return nil }
	wm.BeforeRemoveFunc = func(context.Context, string) error {
		runCmd(wi.Path, "git", "commit", "--allow-empty", "-m", "late bound HEAD")
		return nil
	}
	if _, err := wm.Reap(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	if containsString(outcomes, "removed") || !containsString(outcomes, "refused") {
		t.Fatalf("late HEAD drift was not refused: %v", outcomes)
	}
	if _, err := os.Stat(filepath.Join(wi.Path, ".git")); err != nil {
		t.Fatalf("late HEAD worktree was destroyed: %v", err)
	}
}

func TestFAC178_FinalFenceRejectsCommitAfterIntent(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-LATE-HEAD")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge late HEAD fixture", wi.Branch)
	runCmd(root, "git", "push", "origin", "main")

	removals := 0
	wm.RemoveWorktreeFunc = func(context.Context, string) error { removals++; return nil }
	policy := fac178Policy(t, wm, wi.Path)
	var outcomes []string
	policy.ReceiptSink = func(receipt ReapReceipt) error {
		outcomes = append(outcomes, receipt.Outcome)
		if receipt.Outcome == "remove-intent" {
			runCmd(wi.Path, "git", "commit", "--allow-empty", "-m", "late intent commit")
		}
		return nil
	}
	report, err := wm.Reap(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if removals != 0 || len(report.Reaped) != 0 || !containsString(outcomes, "refused") {
		t.Fatalf("late HEAD crossed final fence: removals=%d reaped=%v outcomes=%v", removals, report.Reaped, outcomes)
	}
	if !containsReason(report.Refused, "final action binding changed after intent") {
		t.Fatalf("missing final HEAD refusal: %+v", report.Refused)
	}
}

func TestFAC178_FinalFenceRejectsLeaseChangeAfterIntent(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	wm := NewWorktreeManager(root)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-178-LATE-LEASE")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(root, "git", "checkout", "main")
	runCmd(root, "git", "merge", "--no-ff", "-m", "merge late lease fixture", wi.Branch)
	runCmd(root, "git", "push", "origin", "main")

	removals, active := 0, false
	wm.RemoveWorktreeFunc = func(context.Context, string) error { removals++; return nil }
	policy := fac178Policy(t, wm, wi.Path)
	var outcomes []string
	policy.LeaseProbe = func(context.Context, string, string) (bool, error) { return active, nil }
	policy.ReceiptSink = func(receipt ReapReceipt) error {
		outcomes = append(outcomes, receipt.Outcome)
		if receipt.Outcome == "remove-intent" {
			active = true
		}
		return nil
	}
	report, err := wm.Reap(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if removals != 0 || len(report.Reaped) != 0 || !containsString(outcomes, "refused") {
		t.Fatalf("late lease crossed final fence: removals=%d reaped=%v outcomes=%v", removals, report.Reaped, outcomes)
	}
	if !containsReason(report.Refused, "final action fence refused removal") {
		t.Fatalf("missing final lease refusal: %+v", report.Refused)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsReason(candidates []ReapCandidate, want string) bool {
	for _, candidate := range candidates {
		if strings.Contains(candidate.Reason, want) {
			return true
		}
	}
	return false
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
		HoldReader:   unheldHoldReader{}, IdentityFor: reapHoldIdentity,
	}
}
