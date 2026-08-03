package config

import (
	"strings"
	"testing"
	"time"
)

func TestOpDeadlines_ResolvedDefaultsEmpty(t *testing.T) {
	g, l, m, c, r, err := OpDeadlines{}.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	if g != 0 || l != 0 || m != 0 || c != 0 || r != 0 {
		t.Fatalf("empty should yield zeros, got %v %v %v %v %v", g, l, m, c, r)
	}
}

func TestOpDeadlines_ResolvedParses(t *testing.T) {
	d := OpDeadlines{Get: "15s", List: "30s", Mutate: "20s", Comment: "10s", Readback: "12s"}
	g, l, m, c, r, err := d.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	if g != 15*time.Second || l != 30*time.Second || m != 20*time.Second {
		t.Fatalf("got %v %v %v", g, l, m)
	}
	if c != 10*time.Second || r != 12*time.Second {
		t.Fatalf("got comment=%v readback=%v", c, r)
	}
}

func TestOpDeadlines_InvalidFailClosed(t *testing.T) {
	_, _, _, _, _, err := OpDeadlines{Get: "not-a-duration"}.Resolved()
	if err == nil || !strings.Contains(err.Error(), "get") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestValidate_RejectsBadDeadlines(t *testing.T) {
	cfg := &Config{
		Version: "1",
		Project: ProjectConfig{Name: "x"},
		TaskProvider: TaskProvider{
			Type:      "kaneo",
			Deadlines: OpDeadlines{List: "nope"},
		},
		Lanes: []LaneDef{{Name: "n", AgentKind: "k", Model: "m", Prompt: "p"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for bad deadlines")
	}
}
