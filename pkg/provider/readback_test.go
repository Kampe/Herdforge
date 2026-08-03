package provider

import (
	"errors"
	"strings"
	"testing"
)

func TestVerifyStatusReadback_Match(t *testing.T) {
	// Exact and alias matches after normalization.
	cases := []struct {
		expected, actual string
	}{
		{"in-progress", "in-progress"},
		{"in-progress", "In Progress"},
		{"done", "closed"},
		{"to-do", "open"},
		{"in-review", "code-review"},
	}
	for _, tc := range cases {
		if err := VerifyStatusReadback("t-1", tc.expected, tc.actual); err != nil {
			t.Errorf("expected match for %q vs %q: %v", tc.expected, tc.actual, err)
		}
	}
}

func TestVerifyStatusReadback_Drift(t *testing.T) {
	err := VerifyStatusReadback("task-42", "done", "in-progress")
	if err == nil {
		t.Fatal("expected readback drift error, got nil")
	}
	var re *ReadbackDriftError
	if !errors.As(err, &re) {
		t.Fatalf("expected *ReadbackDriftError, got %T: %v", err, err)
	}
	if re.TaskID != "task-42" {
		t.Errorf("TaskID=%q", re.TaskID)
	}
	if re.Expected != StatusDone {
		t.Errorf("Expected=%q want done", re.Expected)
	}
	if re.Actual != StatusInProgress {
		t.Errorf("Actual=%q want in-progress", re.Actual)
	}
	if !strings.Contains(err.Error(), "readback drift") {
		t.Errorf("Error()=%q missing readback drift", err.Error())
	}

	// Non-vacuity: matching status must succeed (proves the failure path is specific).
	if err := VerifyStatusReadback("task-42", "done", "done"); err != nil {
		t.Fatalf("matching status must not drift: %v", err)
	}
}

func TestVerifyStatusReadback_EmptyActual(t *testing.T) {
	err := VerifyStatusReadback("t-9", "in-progress", "")
	if err == nil {
		t.Fatal("empty actual must fail")
	}
	var re *ReadbackDriftError
	if !errors.As(err, &re) {
		t.Fatalf("expected *ReadbackDriftError, got %T", err)
	}
	if re.Actual != StatusUnknown {
		t.Errorf("Actual=%q want unknown", re.Actual)
	}
}

func TestVerifyFieldReadback(t *testing.T) {
	if err := VerifyFieldReadback("t", "title", "Hello", "Hello", false); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFieldReadback("t", "title", "Hello", "hello", true); err != nil {
		t.Fatal(err)
	}
	err := VerifyFieldReadback("t", "title", "Hello", "Goodbye", false)
	if err == nil {
		t.Fatal("expected field drift")
	}
	var re *ReadbackDriftError
	if !errors.As(err, &re) || re.Field != "title" {
		t.Fatalf("got %v", err)
	}
}
