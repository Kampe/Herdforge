package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlSurfaceCoversEveryRoutedCommandExactlyOnce(t *testing.T) {
	m := controlSurface()
	if err := validateControlSurfaceManifest(m); err != nil {
		t.Fatal(err)
	}
	for _, command := range knownSubcommands() {
		if err := admitRoutedCommand(command, nil); err != nil {
			t.Fatalf("manifest command %q was not admitted: %v", command, err)
		}
	}
}

func TestControlSurfaceRefusesRoutedCommandAbsentFromManifest(t *testing.T) {
	// This simulates the old regression: a developer adds a main switch case
	// but omits its manifest record. The old optional lookup let that case reach
	// a handler; required admission must stop it before any handler can run.
	if err := admitRoutedCommand("new-routed-command", nil); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("unmanifested routed command must fail admission, got %v", err)
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
