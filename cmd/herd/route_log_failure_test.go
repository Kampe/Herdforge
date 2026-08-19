package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-462 follow-up: a route decision was already computed successfully
// before the durable log write is attempted. A failure to write that log
// (e.g. an unwritable directory) must not turn a successful route pick into
// a hard command failure -- it is an audit-trail gap, not a routing failure.
func TestRouteSucceedsWhenDecisionLogWriteFails(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()

	// A route-log path that is itself a directory can never be opened for
	// writing, deterministically forcing AppendRouteDecision to fail.
	logAsDir := filepath.Join(dir, "route.log")
	if err := os.Mkdir(logAsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "route", "implementation", "--json")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HERD_ROUTE_DECISION_LOG="+logAsDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd route: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "\"provider\"") {
		t.Fatalf("expected a route decision on stdout, got:\n%s", out)
	}
}
