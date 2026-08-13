package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestControlSurfaceCoversEveryRoutedCommandExactlyOnce(t *testing.T) {
	want := []string{
		"activate", "approve", "attention", "board-audit", "board-done", "board-freeze", "board-frozen", "board-sync", "broker", "claude-only", "cleanup", "clone", "command", "commands", "containers", "control", "control-surface", "daemon", "deps", "dispatch", "doctor-models", "drain", "feedback", "fence-broker", "fence-provision", "forge", "fresh-build", "harvest", "harvest-merge", "herdr-deliver", "hold", "hostcreds", "init", "kick", "labels", "lifecycle", "lock", "lost", "merge-admit", "merge-complete", "netbroker-serve", "next", "no-claude", "overlap", "park", "posture", "preflight", "process", "pulse", "quota", "quota-supervisor", "receipt", "repl", "rescue", "reset-safe", "resolve-lane", "resources", "review", "review-classify", "review-ingest", "review-ledger", "role-inject", "route", "scope", "seed-lane-state", "selftest", "send", "sh", "shoot", "shot", "signer-boundary", "spin", "standing", "stash", "status", "stop", "task", "tests-for", "throughput", "tool-probe", "unmerged", "up", "usage", "validate-config", "verify", "verify-fac151", "watch", "wave", "wind-down", "worktrees",
	}
	m := controlSurface()
	if err := validateControlSurfaceManifest(m); err != nil {
		t.Fatal(err)
	}
	got := knownSubcommands()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("manifest commands differ from routed commands\n got: %v\nwant: %v", got, want)
	}
}

func TestControlSurfaceContractMutationRequiresVersionBump(t *testing.T) {
	m := controlSurface()
	m.Commands[0].Output = "mutated contract"
	if err := validateControlSurfaceManifest(m); err == nil || !strings.Contains(err.Error(), "version bump") {
		t.Fatalf("contract mutation must require an explicit version bump, got %v", err)
	}
}

func TestControlSurfaceDiscoveryIsPublicAgentOnly(t *testing.T) {
	binary := buildHerd(t)
	cmd := exec.Command(binary, "control-surface", "--json")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("control-surface: %v", err)
	}
	var got controlSurfaceManifest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("machine-readable output: %v\n%s", err, out)
	}
	if got.Version != controlSurfaceVersion || len(got.Commands) == 0 {
		t.Fatalf("unexpected public manifest: %+v", got)
	}
	for _, c := range got.Commands {
		if c.Classification != classPublicAgent {
			t.Fatalf("discovery leaked %s command %q", c.Classification, c.Command)
		}
		if len(c.Audience) == 0 || len(c.Roles) == 0 || len(c.Evidence) == 0 || c.Input == "" || c.Output == "" {
			t.Fatalf("incomplete public contract: %+v", c)
		}
	}

	// --all was the old-style capability escalation path. It is rejected by
	// flag parsing before the handler reads state or starts a process.
	denied := exec.Command(binary, "control-surface", "--all")
	probe := filepath.Join(t.TempDir(), "probe")
	denied.Env = append(os.Environ(), helpProbeEnv+"="+probe)
	if err := denied.Run(); err == nil {
		t.Fatal("unavailable discovery invocation must be denied")
	}
	if data, err := os.ReadFile(probe); err == nil && len(strings.TrimSpace(string(data))) != 0 {
		t.Fatalf("unavailable discovery entered operational code: %q", data)
	}
}
