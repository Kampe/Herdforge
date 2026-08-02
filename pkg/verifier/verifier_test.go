package verifier

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestVerifyCandidateDirtyCheckoutIsBlocked(t *testing.T) {
	dir, candidate := verificationRepo(t)
	writeFile(t, filepath.Join(dir, "untracked.txt"), "dirty\n")
	receipt, err := NewVerifierArgs([]string{"./check.sh"}).VerifyCandidate(context.Background(), dir, VerificationRequest{CandidateSHA: candidate})
	if err != nil {
		t.Fatalf("dirty candidate should produce a BLOCKED receipt: %v", err)
	}
	if receipt.Outcome != OutcomeBLOCKED {
		t.Fatalf("dirty candidate must be blocked: %+v", receipt)
	}
}

func TestVerifyCandidateCommandFailureIsFAIL(t *testing.T) {
	dir, candidate := verificationRepo(t)
	receipt, err := NewVerifierArgs([]string{"./always-fail.sh"}).VerifyCandidate(context.Background(), dir, VerificationRequest{CandidateSHA: candidate})
	if err != nil {
		t.Fatalf("command failure should be represented by receipt: %v", err)
	}
	if receipt.Outcome != OutcomeFAIL || receipt.ExitCode == 0 {
		t.Fatalf("expected FAIL receipt with nonzero exit: %+v", receipt)
	}
}

func TestRunMutationCheck_RealMutantIsKilledAndRestored(t *testing.T) {
	dir, candidate := mutationRepo(t, false)
	v := NewVerifierArgs([]string{"./check.sh"})
	result, err := v.RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
		TaskRef:      "FAC-122",
		CandidateSHA: candidate,
		TargetFile:   "candidate.txt",
		OriginalCode: "original\n",
		MutantCode:   "mutant\n",
		Timeout:      time.Second,
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

func TestRunMutationCheck_VacuousSuiteFailsMutationGate(t *testing.T) {
	dir, candidate := mutationRepo(t, false)
	result, err := NewVerifierArgs([]string{"true"}).RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
		CandidateSHA: candidate,
		TargetFile:   "candidate.txt",
		OriginalCode: "original\n",
		MutantCode:   "mutant\n",
		Timeout:      time.Second,
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
		CandidateSHA: candidate,
		TargetFile:   "candidate.txt",
		OriginalCode: "original\n",
		MutantCode:   "mutant\n",
		Timeout:      40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("timeout should return a BLOCKED result: %v", err)
	}
	if result.Outcome != OutcomeBLOCKED || result.Killed || !result.Restored {
		t.Fatalf("timeout must block and restore: %+v", result)
	}
	if time.Since(started) > time.Second {
		t.Fatal("bounded mutation timeout exceeded one second")
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	assertClean(t, dir)
}

func TestRunMutationCheck_CancellationRestoresCandidate(t *testing.T) {
	dir, candidate := mutationRepo(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()
	result, err := NewVerifierArgs([]string{"./check.sh"}).RunMutationCheckForCandidate(ctx, dir, MutationRequest{
		CandidateSHA: candidate,
		TargetFile:   "candidate.txt",
		OriginalCode: "original\n",
		MutantCode:   "mutant\n",
		Timeout:      2 * time.Second,
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

func verificationRepo(t *testing.T) (string, string) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "user.name", "verifier-test")
	git(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	writeExecutable(t, filepath.Join(dir, "check.sh"), "#!/bin/sh\n[ \"$(cat candidate.txt)\" = \"original\" ]\n")
	writeExecutable(t, filepath.Join(dir, "always-fail.sh"), "#!/bin/sh\nexit 7\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "candidate")
	return dir, gitOutput(t, dir, "rev-parse", "HEAD")
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
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}
