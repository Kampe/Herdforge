package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeLifecycleLaunchdScheduleRunsIndependentMaintenance(t *testing.T) {
	plist := filepath.Join("..", "..", "packaging", "launchd", "com.kampe.herdforge.worktree-lifecycle.plist")
	body, err := os.ReadFile(plist)
	if err != nil {
		t.Fatalf("read launchd schedule: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"com.kampe.herdforge.worktree-lifecycle",
		"HERD_ROOT",
		"herd maintenance --act",
		"<key>StartInterval</key>",
		"<integer>900</integer>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("launchd schedule missing %q", want)
		}
	}
	if strings.Contains(text, "herd pulse") {
		t.Fatal("launchd lifecycle schedule must not depend on herd pulse")
	}
}
