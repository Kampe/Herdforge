package selftest

import (
	"context"
	"testing"
)

func TestSelfTestRunner_RunSuite(t *testing.T) {
	st := NewSelfTestRunner("../..")
	results, err := st.RunSuite(context.Background())
	if err != nil {
		t.Fatalf("expected clean selftest suite run, got err: %v", err)
	}

	if len(results) < 3 {
		t.Errorf("expected at least 3 assertion results, got %d", len(results))
	}
}

func TestRunSuite_AssertionFailure(t *testing.T) {
	// Using a nonexistent repo root triggers preflight_boundary_check failure
	st := NewSelfTestRunner("/nonexistent")
	results, err := st.RunSuite(context.Background())
	if err == nil {
		t.Fatal("expected error from assertion failure")
	}
	if len(results) == 0 {
		t.Errorf("expected at least 1 result, got 0")
	}
}
