package patterns

import (
	"testing"
)

func TestReviewMarkerPattern(t *testing.T) {
	tests := []struct {
		text        string
		shouldMatch bool
	}{
		// Review markers should match
		{"Verdict: PASS", true},
		{"verdict: fail", true},
		{"Merge recommendation: YES", true},
		{"merge recommendation: NO", true},
		{"confirmed by reviewer", true},
		{"findings found", true},
		{"reviewing the code", true},
		{"pass/fail analysis", true},

		// Non-review text should not match
		{"output limit reached", false},
		{"agent done working", false},
		{"", false},
	}

	re := ReviewMarkerPattern()
	for _, tt := range tests {
		matched := re.MatchString(tt.text)
		if matched != tt.shouldMatch {
			t.Errorf("ReviewMarkerPattern(%q): got %v, want %v", tt.text, matched, tt.shouldMatch)
		}
	}
}

func TestOutputLimitPattern(t *testing.T) {
	tests := []struct {
		text        string
		shouldMatch bool
	}{
		// Output limit variations should match
		{"finish=length", true},
		{"finish_reason=length", true},
		{"finish_reason='length'", true},
		{"output token limit reached", true},
		{"Output Limit Exceeded", true},
		{"maximum context length reached", true},
		{"maximum token length exceeded", true},
		{"max tokens reached", true},
		{"maximum tokens reached", true},
		{"response truncated due to output limit", true},

		// Non-output-limit text should not match
		{"working fine", false},
		{"completed successfully", false},
		{"", false},
	}

	re := OutputLimitPattern()
	for _, tt := range tests {
		matched := re.MatchString(tt.text)
		if matched != tt.shouldMatch {
			t.Errorf("OutputLimitPattern(%q): got %v, want %v", tt.text, matched, tt.shouldMatch)
		}
	}
}

func TestOutputLimitMessage(t *testing.T) {
	if OutputLimitMessage == "" {
		t.Error("OutputLimitMessage should not be empty")
	}
	if OutputLimitMessage != "output token limit reached (finish=length)" {
		t.Errorf("OutputLimitMessage has unexpected value: %q", OutputLimitMessage)
	}
}

func TestPatternsSingleton(t *testing.T) {
	// Verify that repeated calls return the same compiled regexp instances
	// (not strictly required, but more efficient).
	re1 := ReviewMarkerPattern()
	re2 := ReviewMarkerPattern()
	if re1 != re2 {
		t.Error("ReviewMarkerPattern should return the same instance")
	}

	re3 := OutputLimitPattern()
	re4 := OutputLimitPattern()
	if re3 != re4 {
		t.Error("OutputLimitPattern should return the same instance")
	}
}
