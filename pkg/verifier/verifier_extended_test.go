package verifier

import (
	"context"
	"testing"
)

func TestExecute_EmptyCommand(t *testing.T) {
	v := NewVerifier("")
	res, err := v.Execute(context.Background(), ".")
	if err != nil {
		t.Fatalf("expected no error for empty command, got: %v", err)
	}
	if !res.Passed {
		t.Errorf("expected passed=true for empty command, got passed=%v, output=%s", res.Passed, res.Output)
	}
}

func TestExecute_ValidCommand(t *testing.T) {
	v := NewVerifier("echo ok")
	res, err := v.Execute(context.Background(), ".")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !res.Passed {
		t.Errorf("expected passed=true, got passed=%v", res.Passed)
	}
}

func TestExecute_FailingCommand(t *testing.T) {
	v := NewVerifier("false")
	res, err := v.Execute(context.Background(), ".")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if res.Passed {
		t.Errorf("expected passed=false for failing command")
	}
}

func TestRunMutationCheck_BaselineFail(t *testing.T) {
	v := NewVerifier("false")
	_, err := v.RunMutationCheck(context.Background(), ".", "main.go", "orig", "mutant")
	if err == nil {
		t.Fatal("expected error when baseline fails")
	}
}

func TestRunMutationCheck_MutantPasses(t *testing.T) {
	v := NewVerifier("true")
	result, err := v.RunMutationCheck(context.Background(), ".", "main.go", "orig", "mutant")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.Killed {
		t.Error("expected mutant not killed when tests pass")
	}
	if result.MutantID == "" {
		t.Error("expected non-empty MutantID")
	}
}

func TestRunMutationCheck_SecondExecFails(t *testing.T) {
	v := &Verifier{Command: "./nonexistent-script-xyzzy"}
	_, err := v.RunMutationCheck(context.Background(), "/nonexistent-dir", "main.go", "orig", "mutant")
	if err == nil {
		t.Fatal("expected error when second exec fails")
	}
}
