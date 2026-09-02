package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/laneenv"
	"github.com/Kampe/Herdforge/pkg/slot"
)

func TestInProcessStripStillClearsManagedSlotMarker(t *testing.T) {
	if _, ok := os.LookupEnv(slot.EnvHeld); ok {
		t.Fatal("in-process tests inherited HERD_HEAVY_PHASE_SLOT_HELD after Strip")
	}
	for _, leaked := range laneenv.Leaked() {
		t.Fatalf("in-process tests leaked launch metadata %s", leaked)
	}
}

func TestHerdCommandNestedSlotReentryOmitsFleetMetadata(t *testing.T) {
	orig := nestedVerifierSlotHeld
	nestedVerifierSlotHeld = true
	t.Cleanup(func() { nestedVerifierSlotHeld = orig })

	cmd := applyNestedSlotReentry(&exec.Cmd{})
	if cmd.Env == nil {
		t.Fatal("held nested CLI must materialize env so the re-entrancy marker is not left to inherit")
	}
	held := false
	for _, kv := range cmd.Env {
		name, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch name {
		case slot.EnvHeld:
			if val != "1" {
				t.Fatalf("%s=%q, want 1", slot.EnvHeld, val)
			}
			held = true
		case "HERD_ROOT", "HERD_PROJECT_ROOT", "HERD_WORKSPACE", "HERD_ROLE", "HERD_LANE", "HERD_USE_PI", "HERD_MODE", "HERD_ISOLATION_ATTESTATION":
			t.Fatalf("nested CLI inherited unrelated fleet metadata %s", name)
		}
		if strings.HasPrefix(name, "HERDR_") {
			t.Fatalf("nested CLI inherited pane metadata %s", name)
		}
	}
	if !held {
		t.Fatal("nested CLI missing HERD_HEAVY_PHASE_SLOT_HELD=1")
	}

	nestedVerifierSlotHeld = false
	idle := applyNestedSlotReentry(&exec.Cmd{})
	if idle.Env != nil {
		for _, kv := range idle.Env {
			name, val, ok := strings.Cut(kv, "=")
			if ok && name == slot.EnvHeld && val == "1" {
				t.Fatal("idle nested CLI invented a held marker")
			}
		}
	}
}

func TestNestedSlotReentryIsHerdCommandOnly(t *testing.T) {
	orig := nestedVerifierSlotHeld
	nestedVerifierSlotHeld = true
	t.Cleanup(func() { nestedVerifierSlotHeld = orig })

	herd := herdCommand("herd", "status")
	found := false
	for _, kv := range herd.Env {
		name, val, ok := strings.Cut(kv, "=")
		if ok && name == slot.EnvHeld && val == "1" {
			found = true
		}
	}
	if !found {
		t.Fatal("herdCommand lost HERD_HEAVY_PHASE_SLOT_HELD=1 re-entrancy")
	}

	for _, name := range []string{"git", "go", "sh"} {
		cmd := exec.Command(name, "--version")
		if cmd.Env != nil {
			t.Fatalf("%s subprocess received a stamped env; only nested herd CLI children may", name)
		}
	}
}

func TestNestedCLISlotReentryAvoidsHostAcquireWait(t *testing.T) {
	slotDir := filepath.Join(t.TempDir(), "slots")
	s, err := slot.New(slotDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Acquire(t.Context(), "managed-verifier", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Errorf("release host slot: %v", err)
		}
	})

	binary := buildHerd(t)
	childEnv := append(os.Environ(),
		slot.EnvDirectory+"="+slotDir,
		slot.EnvCount+"=1",
	)

	red := exec.Command(binary, "slot", "acquire", "--wait", "250ms", "--purpose", "nested-red")
	red.Env = childEnv
	started := time.Now()
	out, err := red.CombinedOutput()
	waited := time.Since(started)
	if err == nil {
		t.Fatalf("watched RED: nested CLI without re-entrancy acquired the busy host slot:\n%s", out)
	}
	if waited < 150*time.Millisecond {
		t.Fatalf("watched RED: nested acquire returned in %s without waiting on the host slot; out=%s", waited, out)
	}

	orig := nestedVerifierSlotHeld
	nestedVerifierSlotHeld = true
	t.Cleanup(func() { nestedVerifierSlotHeld = orig })
	green := exec.Command(binary, "slot", "acquire", "--wait", "250ms", "--purpose", "nested-green")
	green.Env = childEnv
	applyNestedSlotReentry(green)
	started = time.Now()
	out, err = green.CombinedOutput()
	if err != nil {
		t.Fatalf("nested CLI with slot re-entrancy failed: %v\n%s", err, out)
	}
	if time.Since(started) >= 150*time.Millisecond {
		t.Fatalf("nested CLI with re-entrancy still waited %s:\n%s", time.Since(started), out)
	}
	if !strings.Contains(string(out), "slot=") {
		t.Fatalf("re-entrant acquire output missing slot line:\n%s", out)
	}
}
