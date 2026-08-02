package plugin

import (
	"context"
	"testing"
)

func TestPluginEngine_RegisterAndExecute(t *testing.T) {
	engine := NewPluginEngine()
	dummyWASM := []byte("\x00asm\x01\x00\x00\x00")

	if err := engine.RegisterPlugin("custom-linter", "v1.0.0", dummyWASM); err != nil {
		t.Fatalf("expected clean plugin registration, got err: %v", err)
	}

	out, err := engine.ExecutePlugin(context.Background(), "custom-linter", []byte("code"))
	if err != nil || len(out) == 0 {
		t.Fatalf("expected clean plugin execution, got err: %v", err)
	}
}
