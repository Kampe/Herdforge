package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// FAC-462 follow-up: a route decision was already computed successfully
// before the durable log write is attempted. A failure to write that log
// (e.g. an unwritable directory) must not turn a successful route pick into
// a hard command failure -- it is an audit-trail gap, not a routing failure.
func TestRouteSucceedsWhenDecisionLogWriteFails(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	providerPath := filepath.Join(dir, "providers")
	if err := os.Mkdir(providerPath, 0o700); err != nil {
		t.Fatal(err)
	}
	// The route probe must see the exact readiness sentinel. These stubs make
	// the test independent of installed CLIs and their live provider quota.
	for _, name := range []string{"claude", "grok"} {
		path := filepath.Join(providerPath, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' 'HERD_PROVIDER_PROBE_OK'\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write provider stub %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(providerPath, "openusage"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"providers\":{}}'\n"), 0o755); err != nil {
		t.Fatalf("write usage stub: %v", err)
	}
	herdrStub := filepath.Join(dir, "herdr")
	if err := os.WriteFile(herdrStub, []byte("#!/bin/sh\nprintf '%s\\n' '{\"result\":{\"agents\":[],\"type\":\"agents\"}}'\n"), 0o755); err != nil {
		t.Fatalf("write herdr stub: %v", err)
	}

	// A route-log path that is itself a directory can never be opened for
	// writing, deterministically forcing AppendRouteDecision to fail.
	logAsDir := filepath.Join(dir, "route.log")
	if err := os.Mkdir(logAsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "route", "implementation", "--json")
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + providerPath,
		herdr.BinaryEnv + "=" + herdrStub,
		herdr.NoLiveEnv + "=1",
		"HERD_AVAILABLE_PROVIDERS=claude,grok",
		"HERD_CLAUDE_ONLY=0",
		"HERD_NO_CLAUDE=0",
		"HERD_ROUTE_DECISION_LOG=" + logAsDir,
		"HERD_OPENUSAGE_BIN=" + filepath.Join(providerPath, "openusage"),
		"HERD_QUOTA_HANDOFF_REQUIRED=0",
		"HERD_QUOTA_HANDOFF_BIN=",
		"HERD_STATE_DIR=" + filepath.Join(dir, "state"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"HERDR_ROUTE_STATE_DIR=" + filepath.Join(dir, "routing-state"),
		"HOME=" + os.Getenv("HOME"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd route: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "\"provider\"") {
		t.Fatalf("expected a route decision on stdout, got:\n%s", out)
	}
}
