package main

import (
	"os/exec"
	"testing"
)

func requireHerdrForLiveTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skipf("herdr CLI is unavailable; live fleet integration test skipped: %v", err)
	}
}

func requireRouteEnvironment(t *testing.T) {
	t.Helper()
	requireHerdrForLiveTest(t)

	for _, cli := range []string{"agy", "claude", "codex", "grok", "kimi", "opencode"} {
		if _, err := exec.LookPath(cli); err == nil {
			return
		}
	}
	t.Skip("no configured routing provider CLI is available; live route integration test skipped")
}
