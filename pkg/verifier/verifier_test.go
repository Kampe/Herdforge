package verifier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
	dir, candidate := verificationRepo(t)
	receipt, err := NewVerifierArgs([]string{"./check.sh"}).VerifyCandidate(context.Background(), dir, VerificationRequest{
		TaskRef:           "FAC-122",
		LeaseGeneration:   "lease-7",
		CandidateSHA:      candidate,
		BaseSHA:           candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
		Artifacts:         []string{"candidate.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}

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
			tampered := *receipt
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
	dir := t.TempDir()
	script := filepath.Join(dir, "emit-output")
	writeExecutable(t, script, "#!/bin/sh\nhead -c 2000000 /dev/zero\n")
	v := NewVerifierArgs([]string{"./emit-output"})
	result, err := v.Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
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
	git(t, dir, "add", "tracked-link")
	git(t, dir, "commit", "-q", "-m", "add tracked link")

	gitParentLink := filepath.Join(dir, "git-parent")
	if err := os.Symlink(".git", gitParentLink); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "git-parent")
	git(t, dir, "commit", "-q", "-m", "add git metadata alias")
	outsideParent := t.TempDir()
	outsideVictim := filepath.Join(outsideParent, "victim.txt")
	writeFile(t, outsideVictim, "outside-parent\n")
	outsideParentLink := filepath.Join(dir, "outside-parent")
	if err := os.Symlink(outsideParent, outsideParentLink); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "outside-parent")
	git(t, dir, "commit", "-q", "-m", "add outside parent alias")
	candidate := gitOutput(t, dir, "rev-parse", "HEAD")

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
			assertClean(t, dir)
		})
	}
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

// TestExecuteCancellationRequiresProcessGroupReap mutation-proves that Cancel
// must kill the process group (-pgid), not only the leader pid. With leader-only
// kill the ready descendant survives Execute completion.
func TestExecuteCancellationRequiresProcessGroupReap(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	writeExecutable(t, filepath.Join(dir, "spawn-child"), "#!/bin/sh\n"+
		"sh -c 'printf \"%s\\n\" \"$$\" > \"$1\"; exec sleep 30' childpid \"$1\" &\n"+
		"wait\n")

	// Mutate: leader-only kill (positive pid), not process group.
	prev := processGroupKiller
	processGroupKiller = func(pgid int) error {
		if pgid <= 0 {
			return nil
		}
		return syscall.Kill(pgid, syscall.SIGKILL) // leader only — WRONG
	}
	t.Cleanup(func() { processGroupKiller = prev })

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
	// Leader-only kill leaves the descendant alive — mutation proof.
	// Group probe would also see survivors; leader ESRCH alone is insufficient.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("mutation expected descendant %d to survive leader-only kill; got %v", pid, err)
	}
	// Explicit kill of the orphaned descendant so TempDir cleanup does not race
	// a live writer. (pid is the descendant, not the process-group id.)
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

// waitForPIDGone waits until kill(pid,0) returns an error (ESRCH), proving the
// OS no longer tracks the process (including after zombie reaping).
func waitForPIDGone(pid int, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
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
// Production afterWaitOwnership must BLOCK and reap the residual group.
//
// $1=pidfile $2=writetarget (absolute path under verification dir)
const productionLeaveWriterScript = `
sh -c 'printf "%s\n" "$$" > "$1"; while true; do printf w >> "$2"; done' writer "$1" "$2" </dev/null >/dev/null 2>&1 &
# Handshake: do not exit until the writer has published its pid.
while [ ! -s "$1" ]; do :; done
exit 0
`

// productionDetachedSessionScript: leader starts a new-session (setsid) writer
// that leaves the process group AND a same-group background writer. Both have
// stdio detached. Leader waits for the same-group pid handshake then exits 0.
// Production afterWaitOwnership must BLOCKED on residual same-group writer.
//
// $1=sessionPid $2=writetarget $3=groupPid
// Note: sh -c positional params are $1/$2 relative to args after the command name.
const productionDetachedSessionScript = `
sh -c 'printf "%s\n" "$$" > "$1"; while true; do printf g >> "$2"; done' grpwriter "$3" "$2" </dev/null >/dev/null 2>&1 &
if command -v setsid >/dev/null 2>&1; then
  setsid sh -c 'printf "%s\n" "$$" > "$1"; while true; do printf w >> "$2"; done' sesswriter "$1" "$2" </dev/null >/dev/null 2>&1 &
else
  printf "no-setsid\n" > "$1"
fi
while [ ! -s "$3" ]; do :; done
exit 0
`

// TestExecuteSuccessWithBackgroundWriterBlocksAndReaps is the production-path
// proof: Execute of a command that exits 0 while a same-group writer remains
// must return BLOCKED (residual process group) and the writer must be gone.
func TestExecuteSuccessWithBackgroundWriterBlocksAndReaps(t *testing.T) {
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
	if !strings.Contains(result.Output, "residual process group") &&
		!strings.Contains(result.Output, "ownership") {
		t.Fatalf("BLOCKED output must name ownership/residual close: %q", result.Output)
	}
	// Writer pid was recorded before leader exited; after ownership close it must be gone.
	data, readErr := os.ReadFile(pidFile)
	if readErr == nil {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
			if err := waitForPIDGone(pid, 2*time.Second); err != nil {
				t.Fatalf("production Execute left writer pid %d live: %v", pid, err)
			}
		}
	}
}

// TestExecuteMutationOmittingAfterWaitOwnershipReturnsTooEarly mutation-proves
// production afterWaitOwnership is load-bearing: when it is a no-op, Execute
// returns PASS while the background writer is still live.
func TestExecuteMutationOmittingAfterWaitOwnershipReturnsTooEarly(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "writer.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	writeExecutable(t, filepath.Join(dir, "leave-writer"), "#!/bin/sh\n"+productionLeaveWriterScript)

	prev := afterWaitOwnership
	afterWaitOwnership = func(cmd *exec.Cmd) error { return nil } // MUTATION: skip residual reap
	t.Cleanup(func() {
		afterWaitOwnership = prev
		// Reap any orphan left by the mutation path.
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				_ = waitForPIDGone(pid, 2*time.Second)
			}
		}
	})

	result, err := NewVerifierArgs([]string{"./leave-writer", pidFile, writeTarget}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	// Old execute shape: PASS (leader exit 0) with writer still live.
	if result.Outcome != OutcomePASS {
		t.Fatalf("mutation: omitting afterWaitOwnership should return PASS on exit 0; got %+v", result)
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
		t.Fatalf("mutation expected writer %d to survive without afterWaitOwnership: %v", pid, err)
	}
}

// TestExecuteCancelAfterStartClosesProcessGroup covers cancellation after the
// process is running: Cancel kills the group, Wait + afterWaitOwnership prove
// no residual members before BLOCKED returns.
func TestExecuteCancelAfterStartClosesProcessGroup(t *testing.T) {
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

// TestExecuteDetachedSessionAndBackgroundWriters covers a command that exits 0
// after starting both a setsid (new-session) writer and a same-group writer.
// Production must BLOCKED on residual same-group ownership and reap that
// group; the setsid writer is cleaned by test teardown if still live.
func TestExecuteDetachedSessionAndBackgroundWriters(t *testing.T) {
	dir := t.TempDir()
	sessionPidFile := filepath.Join(dir, "session.pid")
	groupPidFile := filepath.Join(dir, "group.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	writeExecutable(t, filepath.Join(dir, "leave-detached"), "#!/bin/sh\n"+productionDetachedSessionScript)

	result, err := NewVerifierArgs([]string{
		"./leave-detached", sessionPidFile, writeTarget, groupPidFile,
	}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	// Same-group residual forces BLOCKED under production afterWaitOwnership.
	if result.Outcome != OutcomeBLOCKED {
		// no-setsid path still has same-group writer → must BLOCKED
		t.Fatalf("detached+background leave-writer must BLOCKED on residual group, got %+v", result)
	}
	if data, readErr := os.ReadFile(groupPidFile); readErr == nil {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
			if err := waitForPIDGone(pid, 2*time.Second); err != nil {
				t.Fatalf("same-group writer %d still live after Execute: %v", pid, err)
			}
		}
	}
	// Session writer may escape process-group kill; tear down if present.
	if data, readErr := os.ReadFile(sessionPidFile); readErr == nil {
		s := strings.TrimSpace(string(data))
		if s != "" && s != "no-setsid" {
			if pid, convErr := strconv.Atoi(s); convErr == nil && pid > 0 {
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !isESRCH(err) {
					t.Errorf("teardown setsid writer %d: %v", pid, err)
				}
				_ = waitForPIDGone(pid, 2*time.Second)
			}
		}
	}
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
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "user.name", "verifier-test")
	git(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	writeExecutable(t, filepath.Join(dir, "check.sh"), "#!/bin/sh\n[ \"$(cat candidate.txt)\" = \"original\" ]\n")
	writeExecutable(t, filepath.Join(dir, "always-fail.sh"), "#!/bin/sh\nexit 7\n")
	writeExecutable(t, filepath.Join(dir, "env-check.sh"), "#!/bin/sh\n[ -z \"${VERIFIER_AMBIENT_SECRET:-}\" ]\n")
	writeExecutable(t, filepath.Join(dir, "dirty-check.sh"), "#!/bin/sh\ntouch post-run-dirty.txt\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "candidate")
	return dir, gitOutput(t, dir, "rev-parse", "HEAD")
}

func restorationFailureRepo(t *testing.T) (string, string, string) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "user.name", "verifier-test")
	git(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	writeExecutable(t, filepath.Join(dir, "check.sh"), "#!/bin/sh\ncount=$(cat \"$1\" 2>/dev/null || echo 0)\ncount=$((count + 1))\nprintf '%s\\n' \"$count\" > \"$1\"\nif [ \"$(cat candidate.txt)\" = \"mutant\" ]; then exit 1; fi\n[ \"$count\" -lt 3 ]\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "candidate")
	return dir, gitOutput(t, dir, "rev-parse", "HEAD"), filepath.Join(t.TempDir(), "invocations")
}

func mutationRepo(t *testing.T, waits bool) (string, string) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "user.name", "verifier-test")
	git(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	sleep := ""
	if waits {
		sleep = "\nif [ \"$(cat candidate.txt)\" = \"mutant\" ]; then sleep 3; fi\n"
	}
	writeExecutable(t, filepath.Join(dir, "check.sh"), "#!/bin/sh\n"+sleep+"[ \"$(cat candidate.txt)\" != \"mutant\" ]\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "candidate")
	return dir, gitOutput(t, dir, "rev-parse", "HEAD")
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
