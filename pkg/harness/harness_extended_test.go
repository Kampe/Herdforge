package harness

import (
	"strings"
	"testing"
)

func TestGetHarnessConfig_GenericFallback(t *testing.T) {
	cfg := GetHarnessConfig("unknown-harness")
	if cfg.Type != HarnessGeneric {
		t.Errorf("expected HarnessGeneric, got %s", cfg.Type)
	}
	if cfg.BinaryName != "unknown-harness" {
		t.Errorf("expected binary unknown-harness, got %s", cfg.BinaryName)
	}
}

func TestBuildInvocation_NoPromptFlag(t *testing.T) {
	cfg := &HarnessConfig{
		Type:       HarnessGeneric,
		BinaryName: "my-tool",
		PromptFlag: "",
		Supported:  true,
	}
	inv := cfg.BuildInvocation("do it")
	expected := []string{"my-tool", "do it"}
	if len(inv) != 2 || inv[0] != "my-tool" || inv[1] != "do it" {
		t.Errorf("BuildInvocation() = %v, expected %v", inv, expected)
	}
}

func TestLookPath_NotFound(t *testing.T) {
	cfg := &HarnessConfig{BinaryName: "this-binary-should-not-exist-xyzzy"}
	_, err := cfg.LookPath()
	if err == nil {
		t.Fatal("expected error for nonexistent binary")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("unexpected error message: %v", err)
	}
}
