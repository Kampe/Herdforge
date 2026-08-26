package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-692. The memory-cap wrapper lived only on the review host's disk, under
// version control nowhere. It is the single most fragile artifact from the
// 2026-08-26 incident: it is installed on a symlink Claude's own updater owns,
// it took four wrong attempts to get right, and if it were lost the failure
// would be silent capacity reports rather than a visible error.
//
// These tests pin the two properties that were each learned the hard way. They
// read the shipped file, so an edit that drops either one fails here rather
// than fleet-wide at the next launch.

func readWrapper(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "host", "claude-memory-cap.sh"))
	if err != nil {
		t.Fatalf("the memory-cap wrapper is missing from version control: %v", err)
	}
	return string(body)
}

func TestWrapperForcesArgv0ToClaude(t *testing.T) {
	// THE defect that broke the fleet. herdr identifies an agent from the pane
	// process's argv[0]; a plain `exec "$REAL"` presents the versioned install
	// path, so herdr reported "timed out waiting for agent startup" and
	// agent_not_found while Claude ran perfectly. Every launch failed.
	body := readWrapper(t)
	if !strings.Contains(body, "exec -a claude") {
		t.Fatal("wrapper does not force argv[0] to claude: herdr will not recognise the agent and every launch will time out")
	}
	// Inside the scope too -- capping without detection is just a broken fleet.
	if !strings.Contains(body, "systemd-run") {
		t.Fatal("wrapper no longer creates a memory-bounded scope")
	}
	if !strings.Contains(body, "MemoryMax") {
		t.Fatal("wrapper no longer sets MemoryMax")
	}
}

func TestWrapperFailsOpenAndSaysSo(t *testing.T) {
	// A harness that refuses to start is a certain outage on every launch; an
	// uncapped review is bounded by the memory, swap and derived-slot gates. So
	// it must fail OPEN -- but never silently, because a silent fall-open is
	// exactly how a wrapper reported itself capped while running uncapped.
	body := readWrapper(t)
	if !strings.Contains(body, "note_uncapped") {
		t.Fatal("wrapper no longer records fall-open events; an uncapped launch could become silent again")
	}
	if !strings.Contains(body, "uncapped-launches.log") {
		t.Fatal("wrapper no longer names the fall-open log")
	}
	// The last statement must be an unconditional exec, or a host without
	// systemd loses its harness entirely.
	lines := strings.Split(strings.TrimSpace(body), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(last, "exec ") {
		t.Fatalf("wrapper does not end in an unconditional exec, so it can fail closed: %q", last)
	}
}

func TestWrapperAcceptsDegradedSystemdState(t *testing.T) {
	// The review host reports `degraded` because an unrelated unit failed.
	// Treating that as "no systemd" made the wrapper fall open while the
	// capacity check still reported harness_capped: true. Scopes work fine in
	// the degraded state -- verified by reading memory.max off a live cgroup.
	body := readWrapper(t)
	if !strings.Contains(body, "degraded") {
		t.Fatal("wrapper no longer treats a degraded user manager as usable; it will fall open on the review host")
	}
}

func TestHardeningScriptIsIdempotentAndValidatesSshdBeforeReload(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "host", "review-host-harden.sh"))
	if err != nil {
		t.Fatalf("the host hardening script is missing from version control: %v", err)
	}
	text := string(body)
	// Reloading an invalid sshd config on the only remote access path to this
	// machine would end the ability to fix it.
	if !strings.Contains(text, "sshd -t") {
		t.Fatal("hardening script reloads sshd without validating first; a bad config would lock out the only remote path")
	}
	if !strings.Contains(text, "--dry-run") {
		t.Fatal("hardening script has no dry run; an operator cannot see what it would change before running it as root")
	}
}
