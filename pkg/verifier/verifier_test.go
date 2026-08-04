package verifier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// verifierStressSlots lets independent worktrees and marker inodes exercise
// cross-run isolation concurrently without allowing unbounded process-table
// contention under -count. Tests that replace package-level mutation seams or
// process-wide environment do not call t.Parallel and therefore finish before
// these tests are released.
var verifierStressSlots = make(chan struct{}, 2)

func parallelVerifierStress(t *testing.T) {
	t.Helper()
	t.Parallel()
	verifierStressSlots <- struct{}{}
	t.Cleanup(func() { <-verifierStressSlots })
}

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path     string
		expected Language
	}{
		{"main.go", LangGo},
		{"app.ts", LangNode},
		{"script.py", LangPython},
		{"main.rs", LangRust},
		{"README.md", LangUnknown},
	}

	for _, tt := range tests {
		if got := DetectLanguage(tt.path); got != tt.expected {
			t.Errorf("DetectLanguage(%s) = %v, expected %v", tt.path, got, tt.expected)
		}
	}
}

func TestExecute_EmptyCommandFailsClosed(t *testing.T) {
	for _, verifier := range []*Verifier{NewVerifier(""), NewVerifierArgs(nil)} {
		if _, err := verifier.Execute(context.Background(), t.TempDir()); err == nil {
			t.Fatal("empty verification command must fail, not pass vacuously")
		}
	}
}

func TestExecute_QuotedArgumentsRemainOneArg(t *testing.T) {
	parallelVerifierStress(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "assert-argv")
	writeExecutable(t, script, `#!/bin/sh
[ "$#" -eq 2 ] || exit 1
[ "$1" = "hello world" ] || exit 1
[ "$2" = "second value" ] || exit 1
`)

	v := NewVerifier(fmt.Sprintf("%s 'hello world' \"second value\"", script))
	result, err := v.Execute(context.Background(), dir)
	if err != nil {
		t.Fatalf("quoted command should execute: %v", err)
	}
	if !result.Passed || result.Outcome != OutcomePASS {
		t.Fatalf("quoted arguments were not preserved: %+v", result)
	}
}

func TestVerifyCandidateReceiptBindsExactSHAAndDigest(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := verificationRepo(t)
	v := NewVerifierArgs([]string{"./check.sh"})
	req := VerificationRequest{
		TaskRef:           "FAC-122",
		LeaseGeneration:   "lease-7",
		CandidateSHA:      candidate,
		BaseSHA:           candidate,
		EnvironmentPolicy: "hermetic",
		Artifacts:         []string{"candidate.txt"},
	}
	receipt, err := v.VerifyCandidate(context.Background(), dir, req)
	if err != nil {
		t.Fatalf("verification failed: %v", err)
	}
	if receipt.Outcome != OutcomePASS || receipt.Digest == "" {
		t.Fatalf("expected durable PASS receipt: %+v", receipt)
	}
	if len(receipt.Command) != 1 || receipt.Command[0] != "./check.sh" {
		t.Fatalf("receipt must retain exact argv: %+v", receipt.Command)
	}
	if err := receipt.ValidateReceipt(context.Background(), dir); err != nil {
		t.Fatalf("fresh receipt must validate: %v", err)
	}

	writeFile(t, filepath.Join(dir, "later.txt"), "later\n")
	git(t, dir, "add", "later.txt")
	git(t, dir, "commit", "-m", "later")
	if err := receipt.ValidateReceipt(context.Background(), dir); err == nil {
		t.Fatal("receipt must be invalid after candidate SHA changes")
	}
}

func TestReceiptDigestCoversEveryReceiptField(t *testing.T) {
	receipt := fixturePassingReceipt(strings.Repeat("f", 40))

	tests := []struct {
		name   string
		mutate func(*Receipt)
	}{
		{"version", func(r *Receipt) { r.Version++ }},
		{"task ref", func(r *Receipt) { r.TaskRef = "FAC-999" }},
		{"lease generation", func(r *Receipt) { r.LeaseGeneration = "lease-8" }},
		{"candidate SHA", func(r *Receipt) { r.CandidateSHA = strings.Repeat("a", 40) }},
		{"base SHA", func(r *Receipt) { r.BaseSHA = strings.Repeat("b", 40) }},
		{"argv", func(r *Receipt) { r.Command = []string{"./different-check.sh"} }},
		{"exit code", func(r *Receipt) { r.ExitCode++ }},
		{"duration", func(r *Receipt) { r.Duration++ }},
		{"environment policy", func(r *Receipt) { r.EnvironmentPolicy = EnvironmentPolicyHermetic }},
		{"artifacts", func(r *Receipt) { r.Artifacts = []string{"different-artifact"} }},
		{"output digest", func(r *Receipt) { r.OutputDigest = strings.Repeat("c", 64) }},
		{"outcome", func(r *Receipt) { r.Outcome = OutcomeFAIL }},
		{"digest", func(r *Receipt) { r.Digest = "sha256:" + strings.Repeat("d", 64) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tampered := receipt
			tampered.Command = append([]string(nil), receipt.Command...)
			tampered.Artifacts = append([]string(nil), receipt.Artifacts...)
			tt.mutate(&tampered)
			if err := tampered.ValidateDigest(); err == nil {
				t.Fatal("receipt field tampering must invalidate the digest")
			}
		})
	}
}

func TestVerifyAndPersistAdmissionRequiresCurrentPassingDigest(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := verificationRepo(t)
	store, err := NewFileReceiptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := NewVerifierArgs([]string{"./check.sh"}).VerifyAndPersist(context.Background(), dir, VerificationRequest{
		TaskRef:           "FAC-122",
		LeaseGeneration:   "lease-7",
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
	}, store)
	if err != nil {
		t.Fatalf("persisted verification failed: %v", err)
	}
	admission := NewReceiptAdmission(store)
	accepted, err := admission.RequireCurrentPassing(context.Background(), dir, receipt.Digest)
	if err != nil || accepted.Digest != receipt.Digest {
		t.Fatalf("current PASS receipt must be admitted: receipt=%+v err=%v", accepted, err)
	}
	if _, err := admission.RequireCurrentPassing(context.Background(), dir, "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("unknown digest must not be admitted")
	}

	writeFile(t, filepath.Join(dir, "later.txt"), "later\n")
	git(t, dir, "add", "later.txt")
	git(t, dir, "commit", "-q", "-m", "candidate changed")
	if _, err := admission.RequireCurrentPassing(context.Background(), dir, receipt.Digest); err == nil {
		t.Fatal("receipt must not be admitted after candidate changes")
	}
}

func TestFileReceiptStoreRejectsDigestTraversal(t *testing.T) {
	store, err := NewFileReceiptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	malicious := "sha256:../" + strings.Repeat("a", 61)
	if _, err := store.Load(context.Background(), malicious); err == nil || strings.Contains(err.Error(), "no such file") {
		t.Fatalf("traversal digest must fail validation before filesystem lookup: %v", err)
	}
	if _, err := NewReceiptAdmission(store).RequireCurrentPassing(context.Background(), t.TempDir(), malicious); err == nil || strings.Contains(err.Error(), "no such file") {
		t.Fatalf("admission traversal digest must fail validation: %v", err)
	}
}

func TestVerifyCandidateDirtyCheckoutIsBlocked(t *testing.T) {
	dir, candidate := verificationRepo(t)
	writeFile(t, filepath.Join(dir, "untracked.txt"), "dirty\n")
	receipt, err := NewVerifierArgs([]string{"./check.sh"}).VerifyCandidate(context.Background(), dir, VerificationRequest{CandidateSHA: candidate, EnvironmentPolicy: EnvironmentPolicyInherited})
	if err != nil {
		t.Fatalf("dirty candidate should produce a BLOCKED receipt: %v", err)
	}
	if receipt.Outcome != OutcomeBLOCKED {
		t.Fatalf("dirty candidate must be blocked: %+v", receipt)
	}
}

func TestVerifyCandidateCommandFailureIsFAIL(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := verificationRepo(t)
	receipt, err := NewVerifierArgs([]string{"./always-fail.sh"}).VerifyCandidate(context.Background(), dir, VerificationRequest{CandidateSHA: candidate, EnvironmentPolicy: EnvironmentPolicyInherited})
	if err != nil {
		t.Fatalf("command failure should be represented by receipt: %v", err)
	}
	if receipt.Outcome != OutcomeFAIL || receipt.ExitCode == 0 {
		t.Fatalf("expected FAIL receipt with nonzero exit: %+v", receipt)
	}
}

func TestVerifyCandidatePostRunDirtyIsBlocked(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := verificationRepo(t)
	receipt, err := NewVerifierArgs([]string{"./dirty-check.sh"}).VerifyCandidate(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Outcome != OutcomeBLOCKED {
		t.Fatalf("post-run dirty candidate must be BLOCKED: %+v", receipt)
	}
}

func TestVerifyCandidateEnvironmentPolicyIsEnforced(t *testing.T) {
	dir, candidate := verificationRepo(t)
	t.Setenv("VERIFIER_AMBIENT_SECRET", "must-not-leak")
	v := NewVerifierArgs([]string{"./env-check.sh"})

	hermetic, err := v.VerifyCandidate(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyHermetic,
	})
	if err != nil || hermetic.Outcome != OutcomePASS {
		t.Fatalf("hermetic policy must exclude ambient variables: receipt=%+v err=%v", hermetic, err)
	}
	inherited, err := v.VerifyCandidate(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
	})
	if err != nil || inherited.Outcome != OutcomeFAIL {
		t.Fatalf("inherited policy must honestly expose ambient behavior: receipt=%+v err=%v", inherited, err)
	}
	if _, err := v.VerifyCandidate(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: "unknown-policy",
	}); err == nil {
		t.Fatal("unknown environment policy must be rejected")
	}
}

func TestHermeticPolicyResolvesBinaryFromPolicyPath(t *testing.T) {
	dir, candidate := verificationRepo(t)
	ambientDir := t.TempDir()
	writeExecutable(t, filepath.Join(ambientDir, "ambient-only"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", ambientDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	v := NewVerifierArgs([]string{"ambient-only"})

	hermetic, err := v.VerifyCandidate(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyHermetic,
	})
	if err != nil || hermetic.Outcome != OutcomeBLOCKED {
		t.Fatalf("hermetic policy must reject an ambient-only binary: receipt=%+v err=%v", hermetic, err)
	}

	inherited, err := v.VerifyCandidate(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
	})
	if err != nil || inherited.Outcome != OutcomePASS {
		t.Fatalf("inherited policy must resolve the ambient binary: receipt=%+v err=%v", inherited, err)
	}
}

func TestHermeticEnvironmentFindsGoToolchain(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := verificationRepo(t)
	receipt, err := NewVerifierArgs([]string{"go", "version"}).VerifyCandidate(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyHermetic,
	})
	if err != nil || receipt.Outcome != OutcomePASS {
		t.Fatalf("hermetic toolchain lookup failed: receipt=%+v err=%v", receipt, err)
	}
}

func TestExecuteBoundsRetainedOutput(t *testing.T) {
	parallelVerifierStress(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "emit-output")
	writeExecutable(t, script, "#!/bin/sh\nhead -c 2000000 /dev/zero\n")
	result, err := NewVerifierArgs([]string{"./emit-output"}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) > MaxRetainedOutputBytes {
		t.Fatalf("retained output exceeded bound: got %d want <= %d", len(result.Output), MaxRetainedOutputBytes)
	}
	if result.OutputDigest == "" {
		t.Fatal("full output digest must remain available after truncation")
	}
}

func TestReceiptUsesFullOutputDigestWithBoundedRetention(t *testing.T) {
	fullOutput := bytes.Repeat([]byte{'x'}, 2_000_000)
	result := &Result{
		Passed:       true,
		Outcome:      OutcomePASS,
		Output:       boundedOutput(fullOutput),
		OutputDigest: digestBytes(fullOutput),
		ExitCode:     0,
	}
	v := NewVerifierArgs([]string{"./emit-output"})
	receipt := makeReceipt(VerificationRequest{
		CandidateSHA:      strings.Repeat("a", 40),
		EnvironmentPolicy: EnvironmentPolicyInherited,
	}, v.argv(), result, result.Outcome)
	if len(result.Output) > MaxRetainedOutputBytes {
		t.Fatalf("retained output exceeded bound: %d", len(result.Output))
	}
	if receipt.OutputDigest != result.OutputDigest || receipt.OutputDigest == digestBytes([]byte(result.Output)) {
		t.Fatal("receipt must preserve the full process-output digest, not the truncated payload")
	}
}

func TestMutationPathGuardsRejectEscapesAndMetadataWithoutOutsideWrites(t *testing.T) {
	parallelVerifierStress(t)
	dir, _ := verificationRepo(t)
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	writeFile(t, outsideFile, "outside\n")
	gitMetadataProbe := filepath.Join(dir, ".git", "hooks", "fac122-probe")
	writeFile(t, gitMetadataProbe, "metadata\n")

	trackedLink := filepath.Join(dir, "tracked-link")
	if err := os.Symlink(outsideFile, trackedLink); err != nil {
		t.Fatal(err)
	}

	gitParentLink := filepath.Join(dir, "git-parent")
	if err := os.Symlink(".git", gitParentLink); err != nil {
		t.Fatal(err)
	}
	outsideParent := t.TempDir()
	outsideVictim := filepath.Join(outsideParent, "victim.txt")
	writeFile(t, outsideVictim, "outside-parent\n")
	outsideParentLink := filepath.Join(dir, "outside-parent")
	if err := os.Symlink(outsideParent, outsideParentLink); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "tracked-link", "git-parent", "outside-parent")
	git(t, dir, "commit", "-q", "-m", "add mutation guard links")
	candidate := gitOutput(t, dir, "rev-parse", "HEAD")
	before := snapshotWorktree(t, dir)

	tests := []struct {
		name     string
		target   string
		expected string
	}{
		{name: "absolute", target: outsideFile, expected: "relative path"},
		{name: "parent", target: "../outside.txt", expected: "escapes candidate"},
		{name: "nested parent", target: "nested/../../outside.txt", expected: "escapes candidate"},
		{name: "tracked symlink", target: "tracked-link", expected: "Lstat regular file"},
		{name: "resolved git dir", target: "git-parent/hooks/fac122-probe", expected: "git metadata"},
		{name: "git first component", target: ".git/hooks/fac122-probe", expected: "may not enter .git"},
		{name: "symlinked parent", target: "outside-parent/victim.txt", expected: "resolves outside candidate root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewVerifierArgs([]string{"true"}).RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
				CandidateSHA:      candidate,
				EnvironmentPolicy: EnvironmentPolicyInherited,
				TargetFile:        tt.target,
				OriginalCode:      "outside\n",
				MutantCode:        "clobbered\n",
				Timeout:           time.Second,
			})
			if err == nil || !strings.Contains(err.Error(), tt.expected) {
				t.Fatalf("unsafe mutation target must fail with %q, got %v", tt.expected, err)
			}
			assertFile(t, outsideFile, "outside\n")
			assertFile(t, outsideVictim, "outside-parent\n")
			assertFile(t, gitMetadataProbe, "metadata\n")
			assertWorktreeSnapshot(t, dir, before)
		})
	}
	assertClean(t, dir)
}

func TestVerifierNilReceiverFailsClosed(t *testing.T) {
	var v *Verifier
	if _, err := v.VerifyCandidate(context.Background(), t.TempDir(), VerificationRequest{}); err == nil {
		t.Fatal("nil VerifyCandidate receiver must return an error")
	}
	if _, err := v.RunMutationCheck(context.Background(), t.TempDir(), "candidate.txt", "", ""); err == nil {
		t.Fatal("nil RunMutationCheck receiver must return an error")
	}
}

func TestRunMutationCheck_RealMutantIsKilledAndRestored(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := mutationRepo(t, false)
	v := NewVerifierArgs([]string{"./check.sh"})
	result, err := v.RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
		TaskRef:           "FAC-122",
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
		TargetFile:        "candidate.txt",
		OriginalCode:      "original\n",
		MutantCode:        "mutant\n",
		Timeout:           time.Second,
	})
	if err != nil {
		t.Fatalf("mutation check failed: %v", err)
	}
	if !result.Killed || result.Outcome != OutcomePASS || !result.Restored {
		t.Fatalf("real mutation must PASS only after kill and restore: %+v", result)
	}
	if result.Mutant.Outcome != OutcomeFAIL || result.Final.Outcome != OutcomePASS {
		t.Fatalf("mutation receipts do not prove negative then positive runs: mutant=%+v final=%+v", result.Mutant, result.Final)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	assertClean(t, dir)
}

func TestRunMutationCheck_BaselineFailureIsNotKilled(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := verificationRepo(t)
	result, err := NewVerifierArgs([]string{"./always-fail.sh"}).RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
		TargetFile:        "candidate.txt",
		OriginalCode:      "original\n",
		MutantCode:        "mutant\n",
		Timeout:           time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Killed || result.Outcome != OutcomeFAIL || result.Baseline.Outcome != OutcomeFAIL {
		t.Fatalf("baseline failure must stop mutation as FAIL: %+v", result)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	assertClean(t, dir)
}

func TestRunMutationCheck_MismatchedOriginalIsBlocked(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := verificationRepo(t)
	result, err := NewVerifierArgs([]string{"true"}).RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
		TargetFile:        "candidate.txt",
		OriginalCode:      "not-the-candidate\n",
		MutantCode:        "mutant\n",
		Timeout:           time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Killed || result.Outcome != OutcomeBLOCKED || !strings.Contains(result.Output, "does not match") {
		t.Fatalf("mismatched original bytes must block before mutation: %+v", result)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	assertClean(t, dir)
}

func TestExecuteCancellationKillsProcessGroup(t *testing.T) {
	parallelVerifierStress(t)
	// Deterministic ownership barrier:
	//  1. Start Execute asynchronously (context cancel is explicit, not timed).
	//  2. Wait for child-ready signal (pid file written by the descendant).
	//  3. cancel() immediately after ready — ready, not timeout, triggers Cancel.
	//  4. Wait for Execute completion (explicit reap completion inside execute).
	//  5. Assert the exact descendant is gone.
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	writeExecutable(t, filepath.Join(dir, "spawn-child"), "#!/bin/sh\n"+
		// Descendant writes $$ then parks. Ready signal IS the pid file.
		"sh -c 'printf \"%s\\n\" \"$$\" > \"$1\"; exec sleep 30' childpid \"$1\" &\n"+
		"wait\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type execOut struct {
		result *Result
		err    error
	}
	done := make(chan execOut, 1)
	go func() {
		r, e := NewVerifierArgs([]string{"./spawn-child", pidFile}).Execute(ctx, dir)
		done <- execOut{r, e}
	}()

	// Diagnostic bound only: fails the test if the child never signals ready.
	// It does NOT cancel Execute — cancel runs only after ready is observed.
	pid, err := waitForChildReadyPID(pidFile, 5*time.Second)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("child-ready barrier: %v", err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		cancel()
		<-done
		t.Fatalf("ready pid %d is not a live process: %v", pid, err)
	}

	cancel() // cancellation mechanism: ready-observed, not a timer

	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.result.Outcome != OutcomeBLOCKED {
		t.Fatalf("canceled process group must be BLOCKED: %+v", out.result)
	}
	// After Execute returns, residual group reap has completed. Zombies may
	// briefly remain until the OS reparents; ESRCH is the ownership proof.
	if err := waitForPIDGone(pid, 2*time.Second); err != nil {
		t.Fatalf("canceled verifier left descendant process %d alive after reap completion: %v", pid, err)
	}
}

// TestExecuteCancellationWithoutReadyBarrierCannotProveDescendant documents the
// pre-fix flake class: cancelling before the child-ready signal means the
// test cannot prove a descendant existed for process-group reap. This is the
// race×100 failure mode (missing child.pid) when cancel is timer-driven.
func TestExecuteCancellationWithoutReadyBarrierCannotProveDescendant(t *testing.T) {
	parallelVerifierStress(t)
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	writeExecutable(t, filepath.Join(dir, "spawn-child"), "#!/bin/sh\n"+
		"sh -c 'printf \"%s\\n\" \"$$\" > \"$1\"; exec sleep 30' childpid \"$1\" &\n"+
		"wait\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan execOutOrErr, 1)
	go func() {
		r, e := NewVerifierArgs([]string{"./spawn-child", pidFile}).Execute(ctx, dir)
		done <- execOutOrErr{r, e}
	}()
	// Immediate cancel — no ready barrier. May fail Start with context.Canceled
	// or return BLOCKED without a proven descendant. Either way, without the
	// ready barrier we must not claim process-group ownership of a child.
	cancel()
	out := <-done
	if out.err != nil {
		if !errors.Is(out.err, context.Canceled) {
			t.Fatalf("immediate cancel without ready barrier: unexpected err %v", out.err)
		}
		return
	}
	if out.result == nil || out.result.Outcome != OutcomeBLOCKED {
		t.Fatalf("immediate cancel without ready barrier must BLOCKED or ctx err: %+v", out.result)
	}
	// Non-vacuous claim: this test deliberately does NOT call
	// waitForChildReadyPID, so it cannot assert a specific descendant was
	// reaped — that proof lives only in TestExecuteCancellationKillsProcessGroup.
}

// TestExecuteCancellationRequiresProcessGroupReap mutation-proves the incomplete
// ownership shape: leader-only Cancel kill PLUS muted residual drain and
// finalizeOwnedTree leaves the ready descendant alive. Production pairs
// live-group Cancel with done-phase residual drain + finalize so the
// descendant does not survive.
func TestExecuteCancellationRequiresProcessGroupReap(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	writeExecutable(t, filepath.Join(dir, "spawn-child"), "#!/bin/sh\n"+
		"sh -c 'printf \"%s\\n\" \"$$\" > \"$1\"; exec sleep 30' childpid \"$1\" &\n"+
		"wait\n")

	prevKill := processGroupKiller
	processGroupKiller = func(pgid int) error {
		if pgid <= 0 {
			return nil
		}
		return syscall.Kill(pgid, syscall.SIGKILL) // leader only — WRONG
	}
	prevFin := finalizeOwnedTree
	prevDrain := residualDrainFn
	// MUTATION: skip residual drain and finalize kill (incomplete ownership).
	residualDrainFn = func(o *ownedSubprocess) error {
		if o != nil {
			o.freeze()
		}
		return nil
	}
	finalizeOwnedTree = func(o *ownedSubprocess) error {
		if o != nil {
			_ = o.stopTracker()
		}
		return nil
	}
	t.Cleanup(func() {
		processGroupKiller = prevKill
		finalizeOwnedTree = prevFin
		residualDrainFn = prevDrain
		if data, err := os.ReadFile(pidFile); err == nil {
			if p, conv := strconv.Atoi(strings.TrimSpace(string(data))); conv == nil && p > 0 {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan execOutOrErr, 1)
	go func() {
		r, e := NewVerifierArgs([]string{"./spawn-child", pidFile}).Execute(ctx, dir)
		done <- execOutOrErr{r, e}
	}()

	pid, err := waitForChildReadyPID(pidFile, 5*time.Second)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("child-ready barrier: %v", err)
	}
	cancel()
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	// Incomplete ownership leaves the descendant alive — mutation proof.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("mutation expected descendant %d to survive leader-only+no-drain+no-finalize: %v", pid, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !isESRCH(err) {
		t.Fatalf("mutation cleanup kill descendant %d: %v", pid, err)
	}
	if err := waitForPIDGone(pid, 2*time.Second); err != nil {
		t.Fatalf("mutation cleanup: descendant %d still live: %v", pid, err)
	}
}

// TestExecuteCancellationReadyBarrierSelectorRuns proves waitForChildReadyPID
// fails closed when the ready signal never appears (non-vacuous helper gate).
func TestExecuteCancellationReadyBarrierSelectorRuns(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-ready.pid")
	if _, err := waitForChildReadyPID(missing, time.Millisecond); err == nil {
		t.Fatal("waitForChildReadyPID must fail when the ready signal never appears")
	}
}

type execOutOrErr struct {
	result *Result
	err    error
}

// waitForChildReadyPID blocks until path contains a positive live pid, or the
// diagnostic bound elapses. The bound only fails the waiter — callers must
// cancel Execute explicitly after ready.
func waitForChildReadyPID(path string, bound time.Duration) (int, error) {
	deadline := time.Now().Add(bound)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 0 {
				if killErr := syscall.Kill(pid, 0); killErr == nil {
					return pid, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("diagnostic ready bound exceeded waiting for %s", filepath.Base(path))
		}
		// Observe the ready file; not a cancel/cleanup delay.
		time.Sleep(time.Millisecond)
	}
}

// waitForPIDGone waits until the pid is gone or a reaped/zombie non-target,
// proving it can no longer mutate (matches production waitHandleGone policy).
func waitForPIDGone(pid int, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return nil
		}
		// Zombie: reparented residual already SIGKILL'd; cannot mutate.
		if processIsZombie(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pid %d still exists after diagnostic bound", pid)
		}
		time.Sleep(time.Millisecond)
	}
}

// productionLeaveWriterScript: leader backgrounds a same-group active writer
// (stdio detached so Wait is not pipe-blocked), waits until the writer pid
// file exists (explicit handshake, not a sleep proof), then exits 0.
// Production finalizeOwnedTree must BLOCK and reap the residual tree.
//
// $1=pidfile $2=writetarget (absolute path under verification dir)
const productionLeaveWriterScript = `
sh -c 'printf "%s\n" "$$" > "$1"; while true; do printf w >> "$2"; done' writer "$1" "$2" </dev/null >/dev/null 2>&1 &
# Handshake: do not exit until the writer has published its pid.
while [ ! -s "$1" ]; do :; done
exit 0
`

// productionDetachedOnlyScript: adversarial setsid + double-fork residual writer.
// Intermediate parents exit immediately (no keep-alive). The grandchild calls
// os.setsid() so it is NOT a member of the original process group — membership
// kill alone cannot own it. Production must discover it via the inherited
// locked marker FD and identity-kill by token; the open writetarget is
// corroboration only.
//
// $1=writerPid $2=writetarget
// Optional $3=startedPath $4=releasePath provides an explicit ordering gate
// for tests that must launch an unrelated holder after this command starts.
const productionDetachedOnlyScript = `
if [ "$#" -ge 4 ]; then
  printf "%s\n" "$$" > "$3" || exit 1
  while [ ! -e "$4" ]; do :; done
fi
python3 -c '
import os, sys
path, target = sys.argv[1], sys.argv[2]
# Ownership wrapper leaves FD5 open as the inherited lineage marker.
# Do not close FD5 across setsid/double-fork — that is kill authority.
# First fork + setsid: leave the original process group / session.
if os.fork() > 0:
    os._exit(0)
os.setsid()
# Second fork: intermediate session leader exits; grandchild is reparented
# outside the original pgid and is not a session leader wait-edge.
if os.fork() > 0:
    os._exit(0)
# Open a descendant file under the candidate (path corroboration only),
# then chdir away. Marker FD5 remains the lineage authority.
out = open(target, "a", encoding="utf-8")
os.chdir("/")
with open(path, "w", encoding="utf-8") as f:
    f.write("%d\n" % os.getpid())
while True:
    out.write("w")
    out.flush()
' "$1" "$2" </dev/null >/dev/null 2>&1
while [ ! -s "$1" ]; do :; done
exit 0
`

// productionDetachedSessionScript: same-group background + real setsid residual
// that retains FD5 marker lineage and chdirs away with an open descendant FD.
// Each writer has a bounded first generation, then remains alive briefly so
// Execute must reap it rather than passing after natural writer exit.
// $1=sessionPid $2=writetarget $3=groupPid
const productionDetachedSessionScript = `
wait_for_nonempty() {
  i=0
  while [ ! -s "$1" ]; do
    if [ "$i" -ge 500 ]; then return 124; fi
    i=$((i + 1))
    sleep 0.01
  done
}
wait_for_exists() {
  i=0
  while [ ! -e "$1" ]; do
    if [ "$i" -ge 500 ]; then return 124; fi
    i=$((i + 1))
    sleep 0.01
  done
}
sh -c 'printf "%s\n" "$$" > "$1"; for i in $(seq 1 4096); do printf g >> "$2"; done; sleep 5' grpwriter "$3" "$2" </dev/null >/dev/null 2>&1 &
python3 -c '
import os, sys
path, target = sys.argv[1], sys.argv[2]
# Keep FD5 (inherited ownership marker) open across setsid/double-fork.
if os.fork() > 0:
    os._exit(0)
os.setsid()
if os.fork() > 0:
    os._exit(0)
out = open(target, "a", encoding="utf-8")
os.chdir("/")
with open(path, "w", encoding="utf-8") as f:
    f.write("%d\n" % os.getpid())
for _ in range(4096):
    out.write("w")
    out.flush()
import time
time.sleep(5)
' "$1" "$2" </dev/null >/dev/null 2>&1
wait_for_nonempty "$3" || exit $?
wait_for_nonempty "$1" || exit $?
if [ "$#" -ge 5 ]; then
  printf "%s\n" "$$" > "$4" || exit 1
  wait_for_exists "$5" || exit $?
fi
exit 0
`

type pidTokenObservation struct {
	path string
	tok  procToken
	err  error
}

func observePIDToken(ctx context.Context, path string, deadline time.Time) (procToken, error) {
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 1 {
				if tok, tokenErr := tokenOf(pid); tokenErr == nil && tok.isLiveTarget() {
					return tok, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return procToken{}, fmt.Errorf("diagnostic ready bound exceeded waiting for %s", filepath.Base(path))
		}
		select {
		case <-ctx.Done():
			return procToken{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func reapExactTokens(t *testing.T, tokens ...procToken) {
	t.Helper()
	for _, tok := range tokens {
		if !tok.valid() {
			continue
		}
		h, err := openHandle(tok)
		if err != nil {
			if tok.stillSame() {
				t.Errorf("cleanup open exact pid %d: %v", tok.pid, err)
			}
			continue
		}
		if _, err := h.kill(); err != nil {
			t.Errorf("cleanup kill exact pid %d: %v", tok.pid, err)
		}
		h.close()
		if err := waitTokenGone(tok, 2*time.Second); err != nil {
			t.Errorf("cleanup wait exact pid %d gone: %v", tok.pid, err)
		}
	}
}

func assertWriterGone(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read writer pid: %v", err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil || pid <= 0 {
		t.Fatalf("bad writer pid %q", data)
	}
	if err := waitForPIDGone(pid, 2*time.Second); err != nil {
		t.Fatalf("production left writer pid %d live: %v", pid, err)
	}
}

func terminateTestProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("cleanup kill pid %d: %v", cmd.Process.Pid, err)
	}
	if err := cmd.Wait(); err != nil && !isExpectedKillWait(err) {
		t.Errorf("cleanup wait pid %d: %v", cmd.Process.Pid, err)
	}
}

func parentPidOf(pid int) (int, error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	return ppid, nil
}

func forceKillTrackedPID(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("cleanup read pid: %v", err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil || pid <= 0 {
		return
	}
	// Kill parent first so SIGKILL'd children are not stuck as zombies (PPID still
	// alive and not waiting). Then kill the pid and any remaining children.
	if ppid, perr := parentPidOf(pid); perr == nil && ppid > 1 {
		if err := syscall.Kill(ppid, syscall.SIGKILL); err != nil && !isESRCH(err) {
			t.Fatalf("cleanup SIGKILL parent %d: %v", ppid, err)
		}
	}
	if kids, kerr := listChildPids(pid); kerr != nil {
		t.Fatalf("cleanup listChildPids %d: %v", pid, kerr)
	} else {
		for _, k := range kids {
			if err := syscall.Kill(k, syscall.SIGKILL); err != nil && !isESRCH(err) {
				t.Fatalf("cleanup SIGKILL child %d: %v", k, err)
			}
		}
	}
	if err := killProcessGroupIfLive(pid); err != nil && !isESRCH(err) {
		t.Fatalf("cleanup killProcessGroupIfLive %d: %v", pid, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !isESRCH(err) {
		t.Fatalf("cleanup SIGKILL pid %d: %v", pid, err)
	}
	if err := waitForPIDGone(pid, 2*time.Second); err != nil {
		t.Fatalf("cleanup wait pid %d gone: %v", pid, err)
	}
}

// TestExecuteSuccessWithBackgroundWriterBlocksAndReaps is the production-path
// proof: Execute of a command that exits 0 while a same-group writer remains
// must return BLOCKED (residual owned tree) and the writer must be gone.
func TestExecuteSuccessWithBackgroundWriterBlocksAndReaps(t *testing.T) {
	parallelVerifierStress(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "writer.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	writeExecutable(t, filepath.Join(dir, "leave-writer"), "#!/bin/sh\n"+productionLeaveWriterScript)

	result, err := NewVerifierArgs([]string{"./leave-writer", pidFile, writeTarget}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeBLOCKED {
		t.Fatalf("exit-0 with residual same-group writer must BLOCKED, got %+v", result)
	}
	if !strings.Contains(result.Output, "residual") && !strings.Contains(result.Output, "ownership") {
		t.Fatalf("BLOCKED output must name ownership/residual close: %q", result.Output)
	}
	assertWriterGone(t, pidFile)
}

// TestExecuteMutationOmittingFinalizeOwnedTreeReturnsTooEarly mutation-proves
// production finalizeOwnedTree is load-bearing: when it only stops tracking
// without reaping, Execute returns PASS while the background writer is live.
func TestExecuteMutationOmittingFinalizeOwnedTreeReturnsTooEarly(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "writer.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	writeExecutable(t, filepath.Join(dir, "leave-writer"), "#!/bin/sh\n"+productionLeaveWriterScript)

	prevFin := finalizeOwnedTree
	prevDrain := residualDrainFn
	// MUTATION: skip done-phase residual drain and finalize kill.
	residualDrainFn = func(o *ownedSubprocess) error {
		if o != nil {
			o.freeze()
		}
		return nil
	}
	finalizeOwnedTree = func(o *ownedSubprocess) error {
		if o != nil {
			_ = o.stopTracker()
		}
		return nil
	}
	t.Cleanup(func() {
		finalizeOwnedTree = prevFin
		residualDrainFn = prevDrain
		forceKillTrackedPID(t, pidFile)
	})

	result, err := NewVerifierArgs([]string{"./leave-writer", pidFile, writeTarget}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomePASS {
		t.Fatalf("mutation: omitting residual drain should return PASS on exit 0; got %+v", result)
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("mutation: writer pid file: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil || pid <= 0 {
		t.Fatalf("mutation: bad writer pid %q", data)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("mutation expected writer %d to survive without finalizeOwnedTree reap: %v", pid, err)
	}
}

// TestExecuteCancelAfterStartClosesProcessGroup covers cancellation after the
// process is running: Cancel kills the live group, Wait + finalizeOwnedTree
// prove no residual members before BLOCKED returns.
func TestExecuteCancelAfterStartClosesProcessGroup(t *testing.T) {
	parallelVerifierStress(t)
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	writeExecutable(t, filepath.Join(dir, "spawn-child"), "#!/bin/sh\n"+
		"sh -c 'printf \"%s\\n\" \"$$\" > \"$1\"; exec sleep 30' childpid \"$1\" &\n"+
		"wait\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan execOutOrErr, 1)
	go func() {
		r, e := NewVerifierArgs([]string{"./spawn-child", pidFile}).Execute(ctx, dir)
		done <- execOutOrErr{r, e}
	}()
	pid, err := waitForChildReadyPID(pidFile, 5*time.Second)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("child-ready: %v", err)
	}
	cancel()
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.result == nil || out.result.Outcome != OutcomeBLOCKED {
		t.Fatalf("cancel after Start must BLOCKED: %+v", out.result)
	}
	if err := waitForPIDGone(pid, 2*time.Second); err != nil {
		t.Fatalf("cancel path left descendant %d live after ownership close: %v", pid, err)
	}
}

// TestMarkerLineageFindsSetsidChdirAwayWriter proves processesHoldingMarker
// discovers a setsid writer that inherited the ownership marker FD, chdir'd
// away, and still mutates a descendant file — lineage authority, not path.
func TestOwnershipMarkerLockTracksLastInheritedHolder(t *testing.T) {
	marker, markerPath, err := createOwnershipMarker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if marker != nil {
			if err := marker.Close(); err != nil {
				t.Errorf("cleanup marker close: %v", err)
			}
		}
		if err := removeMarkerPath(markerPath); err != nil {
			t.Errorf("cleanup marker path: %v", err)
		}
	})

	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = []*os.File{marker}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminateTestProcess(t, cmd) })
	if err := marker.Close(); err != nil {
		t.Fatalf("close parent marker: %v", err)
	}
	marker = nil

	drained, err := markerLineageDrained(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if drained {
		t.Fatal("marker fixed point reported drained while inherited holder was live")
	}
	terminateTestProcess(t, cmd)
	drained, err = markerLineageDrained(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !drained {
		t.Fatal("marker fixed point remained held after last inherited holder exited")
	}
}

func TestMarkerLineageFindsSetsidChdirAwayWriter(t *testing.T) {
	parallelVerifierStress(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for marker lineage fixture")
	}
	marker, markerPath, err := createOwnershipMarker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = marker.Close()
		_ = os.Remove(markerPath)
	})
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "writer.pid")
	target := filepath.Join(dir, "nested", "residue.log")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	// Inherit marker as FD3 (ExtraFiles[0]); keep it open across setsid.
	cmd := exec.Command("python3", "-c", `
import os, sys, time
path, target = sys.argv[1], sys.argv[2]
# ExtraFiles[0] is FD 3 in the child — do not close it (lineage marker).
if os.fork() > 0:
    os._exit(0)
os.setsid()
if os.fork() > 0:
    os._exit(0)
# Retain marker FD (lineage) + open descendant (path corroboration only).
out = open(target, "a", encoding="utf-8")
os.chdir("/")
with open(path, "w", encoding="utf-8") as f:
    f.write("%d\n" % os.getpid())
while True:
    out.write("w")
    out.flush()
    time.sleep(0.05)
`, pidFile, target)
	cmd.ExtraFiles = []*os.File{marker}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		forceKillTrackedPID(t, pidFile)
	})
	pid, err := waitForChildReadyPID(pidFile, 5*time.Second)
	if err != nil {
		t.Fatalf("writer ready: %v", err)
	}
	toks, err := processesHoldingMarker(markerPath)
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("host-wide libproc diagnostic unavailable: %v", err)
		}
		t.Fatalf("processesHoldingMarker: %v", err)
	}
	toks = filterResidualTokens(toks, -1)
	found := false
	for _, tok := range toks {
		if tok.pid == pid {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("marker lineage must find setsid writer pid %d holding inherited marker; got %v", pid, toks)
	}
	excl := residualExcludePIDs()
	for _, tok := range toks {
		if _, bad := excl[tok.pid]; bad {
			t.Fatalf("marker lineage must not include control-plane pid %d", tok.pid)
		}
	}
}

// TestUnrelatedPathContactWithoutMarkerIsNotLineage is the negative unit
// control: after the parent releases the marker, an unrelated process that
// opens a descendant must not hold the marker fixed point closed. Production
// therefore returns before enumeration or any identity-signal path.
func TestUnrelatedPathContactWithoutMarkerIsNotLineage(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	marker, markerPath, err := createOwnershipMarker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if marker != nil {
			_ = marker.Close()
		}
		_ = removeMarkerPath(markerPath)
	})
	if err := marker.Close(); err != nil {
		t.Fatalf("release parent marker: %v", err)
	}
	marker = nil
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "unrelated.pid")
	target := filepath.Join(dir, "descendant.log")
	// Unrelated: opens descendant, no marker FD inherited.
	cmd := exec.Command("python3", "-c", `
import os, signal, sys
path, target = sys.argv[1], sys.argv[2]
out = open(target, "a", encoding="utf-8")
with open(path, "w", encoding="utf-8") as f:
    f.write("%d\n" % os.getpid())
out.write("u")
out.flush()
signal.pause()
`, pidFile, target)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminateTestProcess(t, cmd) })
	pid, err := waitForChildReadyPID(pidFile, 5*time.Second)
	if err != nil {
		t.Fatalf("unrelated ready: %v", err)
	}
	drained, err := markerLineageDrained(markerPath)
	if err != nil {
		t.Fatalf("marker fixed point: %v", err)
	}
	if !drained {
		t.Fatalf("unrelated path-contact pid %d held marker fixed point closed", pid)
	}
}

// TestResidualExcludePIDsCoversSelfAndParent is a non-vacuous guard that the
// control-plane exclusion set always contains the verifier and its parent.
func TestResidualExcludePIDsCoversSelfAndParent(t *testing.T) {
	excl := residualExcludePIDs()
	self := os.Getpid()
	if _, ok := excl[self]; !ok {
		t.Fatalf("residualExcludePIDs must include self %d", self)
	}
	ppid := os.Getppid()
	if ppid > 1 {
		if _, ok := excl[ppid]; !ok {
			t.Fatalf("residualExcludePIDs must include parent %d", ppid)
		}
	}
}

// TestExecuteDetachedOnlySessionWriterBlocksAndReaps is the production-path
// proof for adversarial setsid+double-fork residual writers that chdir away
// while retaining the inherited marker FD and an open descendant. Intermediate
// parents exit immediately; grandchild leaves the original process group via
// setsid. Execute must BLOCKED via marker lineage and the writer must be gone
// without test teardown.
func TestExecuteDetachedOnlySessionWriterBlocksAndReaps(t *testing.T) {
	parallelVerifierStress(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for setsid residual writer fixture")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "session.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	writeExecutable(t, filepath.Join(dir, "leave-detached-only"), "#!/bin/sh\n"+productionDetachedOnlyScript)
	t.Cleanup(func() { forceKillTrackedPID(t, pidFile) })

	result, err := NewVerifierArgs([]string{"./leave-detached-only", pidFile, writeTarget}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeBLOCKED {
		t.Fatalf("detached-only double-fork writer must BLOCKED (owned tree residual), got %+v", result)
	}
	if !strings.Contains(result.Output, "residual") && !strings.Contains(result.Output, "ownership") {
		t.Fatalf("BLOCKED output must name ownership close: %q", result.Output)
	}
	// Production must have reaped the residual writer — no test teardown kill.
	assertWriterGone(t, pidFile)
}

// TestExecuteUnrelatedPathContactSurvivesMarkedWriterReaped is the production
// negative control: an unrelated process that starts after Execute begins and
// opens a descendant under the candidate (no inherited marker) must SURVIVE,
// while the marked setsid detached writer is reaped.
func TestExecuteUnrelatedPathContactSurvivesMarkedWriterReaped(t *testing.T) {
	parallelVerifierStress(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for setsid residual writer fixture")
	}
	dir := t.TempDir()
	markedPidFile := filepath.Join(dir, "marked.pid")
	unrelatedPidFile := filepath.Join(dir, "unrelated.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	unrelatedTarget := filepath.Join(dir, "unrelated.log")
	startedFile := filepath.Join(dir, "command-started.pid")
	releaseFile := filepath.Join(dir, "release-marked-writer")
	writeExecutable(t, filepath.Join(dir, "leave-detached-only"), "#!/bin/sh\n"+productionDetachedOnlyScript)
	t.Cleanup(func() { forceKillTrackedPID(t, markedPidFile) })

	ctx, cancel := context.WithCancel(context.Background())
	type executeOutcome struct {
		result *Result
		err    error
	}
	executeDone := make(chan executeOutcome, 1)
	executeFinished := make(chan struct{})
	go func() {
		defer close(executeFinished)
		result, err := NewVerifierArgs([]string{
			"./leave-detached-only", markedPidFile, writeTarget, startedFile, releaseFile,
		}).Execute(ctx, dir)
		executeDone <- executeOutcome{result: result, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-executeFinished:
		case <-time.After(5 * time.Second):
			t.Errorf("cleanup: Execute did not stop after cancellation")
		}
	})
	if _, err := waitForChildReadyPID(startedFile, 5*time.Second); err != nil {
		t.Fatalf("verification command start gate: %v", err)
	}

	// Start only after the supervised command's explicit gate. This process
	// opens a descendant but inherits no marker FD.
	unrelated := exec.Command("python3", "-c", `
import os, signal, sys
path, target = sys.argv[1], sys.argv[2]
out = open(target, "a", encoding="utf-8")
with open(path, "w", encoding="utf-8") as f:
    f.write("%d\n" % os.getpid())
signal.pause()
`, unrelatedPidFile, unrelatedTarget)
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminateTestProcess(t, unrelated) })
	unrelatedPID, err := waitForChildReadyPID(unrelatedPidFile, 5*time.Second)
	if err != nil {
		t.Fatalf("unrelated ready: %v", err)
	}
	if err := os.WriteFile(releaseFile, []byte("release\n"), 0o600); err != nil {
		t.Fatalf("release marked writer: %v", err)
	}
	var outcome executeOutcome
	select {
	case outcome = <-executeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Execute exceeded bounded later-holder proof")
	}
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.result.Outcome != OutcomeBLOCKED {
		t.Fatalf("marked detached writer must BLOCKED, got %+v", outcome.result)
	}
	assertWriterGone(t, markedPidFile)

	// Unrelated must still be live — path contact is not kill authority.
	if err := syscall.Kill(unrelatedPID, 0); err != nil {
		t.Fatalf("unrelated path-contact pid %d must survive Execute residual drain: %v", unrelatedPID, err)
	}
}

// TestExecuteDetachedOnlyMutationRemovingMarkerDrainLeavesWriter toggles only
// the marker fixed-point proof. The production positive test must fail under
// this mutation because the detached marked writer survives.
func TestExecuteDetachedOnlyMutationRemovingMarkerDrainLeavesWriter(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for setsid residual writer fixture")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "session.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	writeExecutable(t, filepath.Join(dir, "leave-detached-only"), "#!/bin/sh\n"+productionDetachedOnlyScript)

	previous := markerLineageDrainedFn
	markerLineageDrainedFn = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() {
		markerLineageDrainedFn = previous
		forceKillTrackedPID(t, pidFile)
	})

	result, err := NewVerifierArgs([]string{"./leave-detached-only", pidFile, writeTarget}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomePASS {
		t.Fatalf("mutation: detached-only without marker drain should PASS; got %+v", result)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("mutation: session pid: %v", err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil || pid <= 0 {
		t.Fatalf("mutation: bad session pid %q", data)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("mutation expected double-fork writer %d to survive: %v", pid, err)
	}
}

// TestExecuteDetachedSessionAndBackgroundWriters covers real setsid + same-group
// residual; production must BLOCKED and both writers gone via owned tree close.
func TestExecuteDetachedSessionAndBackgroundWriters(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for setsid residual writer fixture")
	}
	dir := t.TempDir()
	sessionPidFile := filepath.Join(dir, "session.pid")
	groupPidFile := filepath.Join(dir, "group.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	startedFile := filepath.Join(dir, "writers-started")
	releaseFile := filepath.Join(dir, "release-writers")
	writeExecutable(t, filepath.Join(dir, "leave-detached"), "#!/bin/sh\n"+productionDetachedSessionScript)

	type executeOutcome struct {
		result *Result
		err    error
	}
	done := make(chan executeOutcome, 1)
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	observerCtx, cancelObservers := context.WithCancel(context.Background())
	observations := make(chan pidTokenObservation, 2)
	observerDone := make(chan struct{}, 2)
	observerDeadline := time.Now().Add(5 * time.Second)
	var observedMu sync.Mutex
	var observedTokens []procToken
	observe := func(path string) {
		go func() {
			defer func() { observerDone <- struct{}{} }()
			tok, err := observePIDToken(observerCtx, path, observerDeadline)
			if err == nil {
				observedMu.Lock()
				observedTokens = append(observedTokens, tok)
				observedMu.Unlock()
			}
			observations <- pidTokenObservation{path: path, tok: tok, err: err}
		}()
	}
	go func() {
		defer close(finished)
		result, err := NewVerifierArgs([]string{
			"./leave-detached", sessionPidFile, writeTarget, groupPidFile, startedFile, releaseFile,
		}).Execute(ctx, dir)
		done <- executeOutcome{result: result, err: err}
	}()
	// Observers start before any readiness wait and capture the first valid
	// PID/start-token publication. Cleanup is registered immediately after all
	// three goroutines launch, before any diagnostic failure can occur.
	observe(sessionPidFile)
	observe(groupPidFile)
	// Register before any readiness wait. Cleanup order is explicit: cancel
	// Execute first; give both observers one shared capture deadline, then
	// cancel unfinished observers, join all three, and reap only stored tokens.
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Errorf("cleanup: Execute did not stop after cancellation")
		}
		observersJoined := 0
		captureWindow := time.Until(observerDeadline)
		if captureWindow < 0 {
			captureWindow = 0
		}
		captureTimer := time.NewTimer(captureWindow)
		captureExpired := false
		for observersJoined < 2 && !captureExpired {
			select {
			case <-observerDone:
				observersJoined++
			case <-captureTimer.C:
				captureExpired = true
			}
		}
		if !captureExpired && !captureTimer.Stop() {
			select {
			case <-captureTimer.C:
			default:
			}
		}
		cancelObservers()
		for observersJoined < 2 {
			<-observerDone
			observersJoined++
		}
		for {
			select {
			case <-observations:
			default:
				goto observationsDrained
			}
		}
	observationsDrained:
		observedMu.Lock()
		tokens := append([]procToken(nil), observedTokens...)
		observedMu.Unlock()
		if len(tokens) == 0 {
			t.Errorf("cleanup: no fixture PID/start-token identity was captured")
			return
		}
		reapExactTokens(t, tokens...)
	})
	if _, err := waitForChildReadyPID(startedFile, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	observed := make(map[string]procToken, 2)
	for range 2 {
		observation := <-observations
		if observation.err != nil {
			t.Fatal(observation.err)
		}
		observed[observation.path] = observation.tok
	}
	sessionToken, sessionOK := observed[sessionPidFile]
	groupToken, groupOK := observed[groupPidFile]
	if !sessionOK || !groupOK {
		t.Fatalf("observer did not capture both fixture identities: %+v", observed)
	}
	if sessionToken.equal(groupToken) {
		t.Fatalf("writer inventory aliases one identity: session=%+v group=%+v", sessionToken, groupToken)
	}
	t.Logf("writer inventory: session pid=%d start=%d/%d; group pid=%d start=%d/%d",
		sessionToken.pid, sessionToken.startSec, sessionToken.startUsec,
		groupToken.pid, groupToken.startSec, groupToken.startUsec)
	if err := os.WriteFile(releaseFile, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var outcome executeOutcome
	select {
	case outcome = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("bounded detached-session proof exceeded diagnostic bound")
	}
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	result := outcome.result
	if result.Outcome != OutcomeBLOCKED {
		t.Fatalf("detached+background leave-writer must BLOCKED, got %+v", result)
	}
	assertWriterGone(t, groupPidFile)
	assertWriterGone(t, sessionPidFile)
	residue, err := os.ReadFile(writeTarget)
	if err != nil {
		t.Fatal(err)
	}
	if len(residue) == 0 || len(residue) > 8192 {
		t.Fatalf("bounded residue must be non-empty and <=8192 bytes, got %d", len(residue))
	}
}

// TestProcTokenIdentityBoundRefusesStalePID proves kill is refused when a PID
// no longer matches the recorded start token (PID reuse safety).
func TestProcTokenIdentityBoundRefusesStalePID(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tok, err := tokenOf(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("tokenOf: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected wait error after kill")
	}
	// Token must no longer match (process exited).
	if tok.stillSame() {
		t.Fatal("token.stillSame after exit must be false")
	}
	h, err := openHandle(tok)
	if err != nil {
		// Process already gone — still proves stillSame is false.
		if tok.stillSame() {
			t.Fatal("token.stillSame after exit must be false")
		}
		return
	}
	signaled, err := h.kill()
	if err != nil {
		t.Fatalf("handle.kill on stale token: %v", err)
	}
	if signaled {
		t.Fatal("handle.kill must not signal a stale/reused PID identity")
	}
	h.close()
}

// TestKillProcessGroupMembersRejectsHostPGIDsBeforeSnapshot proves pgid 0/1
// fail before enumeration. It never invokes a real signal with either value.
func TestKillProcessGroupMembersRejectsHostPGIDsBeforeSnapshot(t *testing.T) {
	previous := processGroupSnapshotter
	snapshotCalls := 0
	processGroupSnapshotter = func() (processSnapshot, error) {
		snapshotCalls++
		return processSnapshot{}, nil
	}
	t.Cleanup(func() { processGroupSnapshotter = previous })

	for _, pgid := range []int{0, 1} {
		if err := killProcessGroupMembersExcept(pgid, -1); err == nil {
			t.Fatalf("host-wide pgid %d must be rejected", pgid)
		}
	}
	if snapshotCalls != 0 {
		t.Fatalf("invalid host pgids reached process enumeration %d time(s)", snapshotCalls)
	}
}

// TestKillProcessGroupMembersNeverUsesNegativePGID is a non-vacuous guard:
// processGroupKiller must reap via positive PIDs (identity), not kill(-pgid).
func TestKillProcessGroupMembersNeverUsesNegativePGID(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Spawn a same-group child so membership kill has work beyond the leader.
	child := exec.Command("sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	if err := processGroupKiller(pgid); err != nil {
		t.Fatalf("processGroupKiller: %v", err)
	}
	if err := waitPIDGone(cmd.Process.Pid, 2*time.Second); err != nil {
		t.Fatalf("leader not reaped by membership kill: %v", err)
	}
	if err := waitPIDGone(child.Process.Pid, 2*time.Second); err != nil {
		t.Fatalf("group child not reaped by membership kill: %v", err)
	}
}

// TestOwnedNeverReplacesTokenOnPIDReuse forces record of pid P, then simulates
// a second observation of P with a different start token — noteCausal must not
// replace the first incarnation (audit: never adopt reused PID work).
func TestOwnedNeverReplacesTokenOnPIDReuse(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	owned, err := adoptOwnedCmd(cmd, nil, nil, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owned.Close() })

	pid := cmd.Process.Pid
	owned.mu.Lock()
	first, ok := owned.handles[pid]
	owned.mu.Unlock()
	if !ok {
		t.Fatal("leader handle missing")
	}
	// Forged token with same pid, different start time (reused PID impostor).
	impostor := procToken{pid: pid, startSec: first.tok.startSec + 999999, startUsec: 1}
	if err := owned.noteCausal(impostor); err != nil {
		t.Fatalf("noteCausal impostor: %v", err)
	}
	owned.mu.Lock()
	second := owned.handles[pid]
	owned.mu.Unlock()
	if !second.tok.equal(first.tok) {
		t.Fatalf("token replaced on PID reuse: got %+v want %+v", second.tok, first.tok)
	}
}

// TestOwnedFreezeRejectsPostLeaderGroupAdoption freezes ownership then proves
// sample does not adopt new numeric-pgid members (post-Wait PGID reuse class).
func TestOwnedFreezeRejectsPostLeaderGroupAdoption(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	owned, err := adoptOwnedCmd(cmd, nil, nil, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Kill leader so freeze triggers on next sample; then spawn unrelated
	// process that might land in a recycled pgid space — we only check freeze.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	owned.freeze()
	before := len(owned.handles)
	// sample while frozen must be a no-op for discovery.
	if err := owned.sample(); err != nil {
		t.Fatalf("sample frozen: %v", err)
	}
	if len(owned.handles) != before {
		t.Fatalf("frozen sample adopted handles: before=%d after=%d", before, len(owned.handles))
	}
	// Close should not membership-kill numeric pgid (no processGroupKiller on Close).
	_ = owned.Close()
}

// TestIsExpectedKillWaitUsesTypedWaitStatus proves signal classification does
// not use substring matching on error text.
func TestIsExpectedKillWaitUsesTypedWaitStatus(t *testing.T) {
	if isExpectedKillWait(nil) != true {
		t.Fatal("nil wait err is expected")
	}
	if isExpectedKillWait(errors.New("signal: killed")) {
		t.Fatal("plain error with kill substring must NOT match (typed WaitStatus only)")
	}
	// Real signaled exit: kill a short-lived process group member via Wait.
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("expected wait error after SIGKILL")
	}
	if !isExpectedKillWait(waitErr) {
		t.Fatalf("typed WaitStatus SIGKILL must be expected, got %v (%T)", waitErr, waitErr)
	}
}

func TestRunMutationCheck_VacuousSuiteFailsMutationGate(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := mutationRepo(t, false)
	result, err := NewVerifierArgs([]string{"true"}).RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
		TargetFile:        "candidate.txt",
		OriginalCode:      "original\n",
		MutantCode:        "mutant\n",
		Timeout:           time.Second,
	})
	if err != nil {
		t.Fatalf("vacuous mutation check should return a FAIL result: %v", err)
	}
	if result.Killed || result.Outcome != OutcomeFAIL || !result.Restored {
		t.Fatalf("vacuous suite must fail the mutation gate and restore: %+v", result)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
}

func TestRunMutationCheck_TimeoutRestoresCandidate(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := mutationRepo(t, true)
	v := NewVerifierArgs([]string{"./check.sh"})
	started := time.Now()
	result, err := v.RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
		TargetFile:        "candidate.txt",
		OriginalCode:      "original\n",
		MutantCode:        "mutant\n",
		Timeout:           40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("timeout should return a BLOCKED result: %v", err)
	}
	if result.Outcome != OutcomeBLOCKED || result.Killed || !result.Restored {
		t.Fatalf("timeout must block and restore: %+v", result)
	}
	// Bound the mutant Execute phase (Timeout + WaitDelay), not wall-clock of
	// git baseline setup under -race×count scheduler noise. One-second bound
	// matches the original contract applied to the timed mutation, not setup.
	if result.Mutant.Duration > time.Second {
		t.Fatalf("bounded mutation timeout exceeded one second: mutant_duration=%v wall=%v", result.Mutant.Duration, time.Since(started))
	}
	// Hang detector: full path including baseline must not stick on sleep-3.
	if time.Since(started) > 10*time.Second {
		t.Fatalf("RunMutationCheck hung: wall=%v (mutant_duration=%v)", time.Since(started), result.Mutant.Duration)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	assertClean(t, dir)
}

func TestRunMutationCheck_CancellationRestoresCandidate(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := mutationRepo(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after mutant bytes are on disk so Restored evidence is real.
	// Ready-on-mutant is the cancel trigger (not a 3s Timeout inflation).
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(filepath.Join(dir, "candidate.txt"))
			if err == nil && string(data) == "mutant\n" {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	result, err := NewVerifierArgs([]string{"./check.sh"}).RunMutationCheckForCandidate(ctx, dir, MutationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
		TargetFile:        "candidate.txt",
		OriginalCode:      "original\n",
		MutantCode:        "mutant\n",
		Timeout:           2 * time.Second,
	})
	if err != nil {
		t.Fatalf("cancellation should return a BLOCKED result: %v", err)
	}
	if result.Outcome != OutcomeBLOCKED || result.Killed || !result.Restored {
		t.Fatalf("cancellation must block and restore: %+v", result)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	assertClean(t, dir)
}

func TestRunMutationCheck_RestoredCandidateFailureIsBlocked(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate, counter := restorationFailureRepo(t)
	result, err := NewVerifierArgs([]string{"./check.sh", counter}).RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
		TargetFile:        "candidate.txt",
		OriginalCode:      "original\n",
		MutantCode:        "mutant\n",
		Timeout:           time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Killed || result.Outcome != OutcomeBLOCKED || !result.Restored {
		t.Fatalf("restored candidate failure must block rather than PASS: %+v", result)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	assertClean(t, dir)
}

func verificationRepo(t *testing.T) (string, string) {
	return copyCachedRepo(t, "verification", buildVerificationFixture)
}

func restorationFailureRepo(t *testing.T) (string, string, string) {
	dir, candidate := copyCachedRepo(t, "restoration-failure", buildRestorationFailureFixture)
	return dir, candidate, filepath.Join(t.TempDir(), "invocations")
}

func mutationRepo(t *testing.T, waits bool) (string, string) {
	key := fmt.Sprintf("mutation-waits-%t", waits)
	return copyCachedRepo(t, key, buildMutationFixture(waits))
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	writeFile(t, path, contents)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertClean(t *testing.T, dir string) {
	t.Helper()
	if output := gitOutput(t, dir, "status", "--porcelain", "--untracked-files=all"); output != "" {
		t.Fatalf("candidate is not clean after mutation: %q", output)
	}
}

func snapshotWorktree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if rel == "." {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += "|link=" + target
		case info.Mode().IsRegular():
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += "|digest=" + digestBytes(contents)
		}
		got[rel] = value
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot worktree: %v", err)
	}
	return got
}

func assertWorktreeSnapshot(t *testing.T, root string, want map[string]string) {
	t.Helper()
	got := snapshotWorktree(t, root)
	if !maps.Equal(got, want) {
		t.Fatalf("worktree changed after rejected mutation: got=%v want=%v", got, want)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	// Production hermetic runner: same process-group + config isolation as the
	// verifier path so tests cannot seed detached auto-gc writers that race
	// t.TempDir cleanup of dir/.git.
	if _, err := runGit(dir, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := runGit(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}
