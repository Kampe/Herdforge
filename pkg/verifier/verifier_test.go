package verifier

import (
	"context"
	"testing"
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
		got := DetectLanguage(tt.path)
		if got != tt.expected {
			t.Errorf("DetectLanguage(%s) = %v, expected %v", tt.path, got, tt.expected)
		}
	}
}

func TestVerifier_MutationCheck(t *testing.T) {
	v := NewVerifier("echo baseline ok")
	res, err := v.RunMutationCheck(context.Background(), ".", "config.go", "code", "mutant")
	if err != nil {
		t.Fatalf("expected clean mutation check execution, got err: %v", err)
	}
	if res.MutantID != "mutant-config.go" {
		t.Errorf("unexpected mutant ID: %s", res.MutantID)
	}
}
