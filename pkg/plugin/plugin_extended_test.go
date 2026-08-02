package plugin

import (
	"context"
	"testing"
)

func TestRegisterPlugin_EmptyName(t *testing.T) {
	e := NewPluginEngine()
	err := e.RegisterPlugin("", "v1.0.0", []byte("wasm"))
	if err == nil {
		t.Fatal("expected error for empty plugin name")
	}
}

func TestExecutePlugin_NotFound(t *testing.T) {
	e := NewPluginEngine()
	_, err := e.ExecutePlugin(context.Background(), "nonexistent", []byte("code"))
	if err == nil {
		t.Fatal("expected error for nonexistent plugin")
	}
}

func TestExecutePlugin_Success(t *testing.T) {
	e := NewPluginEngine()
	err := e.RegisterPlugin("my-plugin", "v0.1.0", []byte("\x00asm\x01\x00\x00\x00"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	out, err := e.ExecutePlugin(context.Background(), "my-plugin", []byte("source"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	expected := "wasm-verified:my-plugin:source"
	if string(out) != expected {
		t.Errorf("expected %s, got %s", expected, out)
	}
}
