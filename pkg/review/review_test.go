package review

import (
	"testing"
)

func TestClassifyRiskTier(t *testing.T) {
	tests := []struct {
		files    []string
		expected RiskTier
	}{
		{[]string{"README.md", "docs/spec.md"}, TierR0RiskMechanical},
		{[]string{"pkg/config/config.go"}, TierR1RiskStandard},
		{[]string{"pkg/auth/jwt.go"}, TierR3RiskCritical},
	}

	for _, tt := range tests {
		got := ClassifyRiskTier(tt.files)
		if got != tt.expected {
			t.Errorf("ClassifyRiskTier(%v) = %s, expected %s", tt.files, got, tt.expected)
		}
	}
}
