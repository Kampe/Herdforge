package verifier

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
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
