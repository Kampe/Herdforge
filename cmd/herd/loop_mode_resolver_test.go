package main

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

// resolveLaneLoopMode had one production caller and zero tests, which
// code-review-graph flagged after FAC-524 landed. It is the only source of a
// lane's loop mode now that herdr is known not to emit loop_mode, so its
// fail-closed edges matter: a wrong answer here reports a held lane as
// available capacity.
func TestResolveLaneLoopModeFailsClosedWithoutConfig(t *testing.T) {
	if _, err := resolveLaneLoopMode(nil, "orchestrator"); err == nil {
		t.Fatal("a nil config must fail closed rather than imply a running loop")
	}
}

func TestResolveLaneLoopModeRejectsUnknownLane(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{
		{Name: "orchestrator", Role: "orchestrator"},
	}}
	mode, err := resolveLaneLoopMode(cfg, "no-such-lane")
	if err == nil {
		t.Fatalf("an unknown lane must fail closed, got mode %q", mode)
	}
	if mode != "" {
		t.Fatalf("a failed resolution must not report a mode, got %q", mode)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "lane") {
		t.Fatalf("error should name the lane problem, got %v", err)
	}
}
