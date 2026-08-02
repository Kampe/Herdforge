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

func TestRunMutationCheck_BaselineFails(t *testing.T) {
	v := NewVerifier("false") // baseline always fails
	_, err := v.RunMutationCheck(context.Background(), ".", "main.go", "orig", "mutant")
	if err == nil {
		t.Fatal("expected error when baseline test suite fails")
	}
}
