package skill

import (
	"context"
	"strings"
	"testing"
)

func TestRunner_RegisterAndExecuteSkill(t *testing.T) {
	runner := NewRunner()

	skill := &Skill{
		Name:        "code-review-graph",
		Version:     "1.2.0",
		Description: "Code intelligence graph generator",
		MaxMemoryMB: 128,
	}

	if err := runner.RegisterSkill(skill); err != nil {
		t.Fatalf("expected clean RegisterSkill, got err: %v", err)
	}

	out, err := runner.ExecuteSkill(context.Background(), "code-review-graph", []byte("query: ast"))
	if err != nil {
		t.Fatalf("expected clean ExecuteSkill, got err: %v", err)
	}

	if !strings.Contains(string(out), "WASM execution [code-review-graph v1.2.0]") {
		t.Errorf("unexpected skill execution output: %s", string(out))
	}
}

func TestRunner_UnregisteredAndMemoryLimit(t *testing.T) {
	runner := NewRunner()

	_, err := runner.ExecuteSkill(context.Background(), "non-existent", []byte("input"))
	if err == nil {
		t.Errorf("expected error for unregistered skill, got nil")
	}

	_ = runner.RegisterSkill(&Skill{Name: "small-mem", MaxMemoryMB: 1})
	largeInput := make([]byte, 2*1024*1024) // 2MB input exceeds 1MB limit
	_, err = runner.ExecuteSkill(context.Background(), "small-mem", largeInput)
	if err == nil {
		t.Errorf("expected memory limit error, got nil")
	}
}
