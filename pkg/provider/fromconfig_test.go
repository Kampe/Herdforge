package provider

import (
	"context"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
)

func TestApplyDeadlines_Kaneo(t *testing.T) {
	k := NewKaneoProvider("http://x", "p", false)
	ApplyDeadlines(k, Deadlines{Get: 3 * time.Second, List: 4 * time.Second})
	if k.Deadlines.Get != 3*time.Second || k.Deadlines.List != 4*time.Second {
		t.Fatalf("deadlines not applied: %+v", k.Deadlines)
	}
}

func TestBoundOp_Fires(t *testing.T) {
	ctx, cancel := BoundOp(context.Background(), Deadlines{Get: 20 * time.Millisecond}, OpGet)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Bound op deadline did not fire")
	}
}

func TestNewFromHerdConfig_LinearUsesOnlyConfiguredCredentialEnv(t *testing.T) {
	cfg := &config.Config{TaskProvider: config.TaskProvider{
		Type:      "linear",
		APIKeyEnv: "LINEAR_API_KEY",
	}}
	t.Setenv("LINEAR_API_KEY", "linear-test-key")
	t.Setenv("KANEO_API_KEY", "must-not-be-used")

	tp, err := NewFromHerdConfig(cfg)
	if err != nil {
		t.Fatalf("NewFromHerdConfig: %v", err)
	}
	bound, ok := tp.(*BoundClient)
	if !ok {
		t.Fatalf("provider=%T, want *BoundClient", tp)
	}
	linear, ok := bound.Inner.(*LinearProvider)
	if !ok || linear.APIKey != "linear-test-key" {
		t.Fatalf("linear provider=%#v", bound.Inner)
	}

	t.Setenv("LINEAR_API_KEY", "")
	if _, err := NewFromHerdConfig(cfg); err == nil {
		t.Fatal("linear must not fall back to KANEO_API_KEY")
	}
}

func TestDeadlinesFromParts_Normalize(t *testing.T) {
	d := DeadlinesFromParts(0, 0, 5*time.Second, 0, 0)
	if d.Get != DefaultGetDeadline || d.Mutate != 5*time.Second {
		t.Fatalf("%+v", d)
	}
}
