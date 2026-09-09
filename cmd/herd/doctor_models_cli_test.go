package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorModelsCLI_UsesConfiguredNativeProviders(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `version: "1"
project:
  name: doctor-fixture
task_provider:
  type: kaneo
lanes:
  - name: codex-lane
    agent_kind: codex
    prompt: prompt.md
    provider: codex
    model: gpt-5.6-luna
    effort: medium
  - name: grok-lane
    agent_kind: grok
    prompt: prompt.md
    provider: grok
    model: grok-4.6
    effort: medium
  - name: agy-lane
    agent_kind: agy
    prompt: prompt.md
    provider: agy
    model: gemini-2.5-pro
  - name: opencode-lane
    agent_kind: opencode
    prompt: prompt.md
    provider: opencode
    model: litellm/lazer/deepseek-v4-flash
`
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	for _, command := range []string{"agy", "codex", "grok", "opencode"} {
		path := filepath.Join(binDir, command)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' PROBE_OK\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command(buildHerd(t), "doctor-models")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+binDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor-models failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"OK    codex-lane -> codex/gpt-5.6-luna",
		"OK    grok-lane -> grok/grok-4.6",
		"OK    agy-lane -> agy/gemini-2.5-pro",
		"OK    opencode-lane -> opencode/litellm/lazer/deepseek-v4-flash",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
}
