package provider

import (
	"fmt"
	"strings"
)

// ReadbackDriftError reports a write-then-read mismatch after a mutation.
// Callers must not treat the mutation as successful when this is returned.
type ReadbackDriftError struct {
	TaskID   string
	Field    string
	Expected string
	Actual   string
}

func (e *ReadbackDriftError) Error() string {
	if e == nil {
		return "<nil readback drift>"
	}
	field := e.Field
	if field == "" {
		field = "status"
	}
	return fmt.Sprintf("readback drift on task %s: %s expected %q, got %q",
		e.TaskID, field, e.Expected, e.Actual)
}

// VerifyStatusReadback compares expected vs actual status after a mutation.
// Both sides are normalized before comparison so provider aliases do not
// produce false drift. Empty actual (missing task / unknown state) always fails.
func VerifyStatusReadback(taskID, expected, actual string) error {
	want := NormalizeStatus(expected)
	got := NormalizeStatus(actual)
	if actual == "" {
		return &ReadbackDriftError{
			TaskID:   taskID,
			Field:    "status",
			Expected: want,
			Actual:   StatusUnknown,
		}
	}
	if want != got {
		return &ReadbackDriftError{
			TaskID:   taskID,
			Field:    "status",
			Expected: want,
			Actual:   got,
		}
	}
	return nil
}

// VerifyFieldReadback is a generic string equality check for non-status fields
// (title, assignee, etc.). Whitespace is trimmed; comparison is case-sensitive
// unless fold is true.
func VerifyFieldReadback(taskID, field, expected, actual string, fold bool) error {
	exp := strings.TrimSpace(expected)
	act := strings.TrimSpace(actual)
	if fold {
		if !strings.EqualFold(exp, act) {
			return &ReadbackDriftError{TaskID: taskID, Field: field, Expected: exp, Actual: act}
		}
		return nil
	}
	if exp != act {
		return &ReadbackDriftError{TaskID: taskID, Field: field, Expected: exp, Actual: act}
	}
	return nil
}
