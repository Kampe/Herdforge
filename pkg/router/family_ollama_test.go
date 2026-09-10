package router

import "testing"

func TestFamilyForOpenCodeFlashModelsUsesActualModelFamily(t *testing.T) {
	tests := []struct {
		model  string
		family string
	}{
		{"opencode/gemini-3.8-flash", "google"},
		{"opencode/deepseek-v4-flash", "deepseek"},
		{"ollama-cloud/deepseek-v4-flash", "deepseek"},
	}
	for _, test := range tests {
		if got := FamilyFor("opencode", test.model); got != test.family {
			t.Errorf("FamilyFor(opencode, %q) = %q, want %q", test.model, got, test.family)
		}
	}
}
