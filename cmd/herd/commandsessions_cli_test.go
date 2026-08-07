package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/cmdsession"
)

// absentPID returns a PID the live host confirms does not exist, so a sweep
// that re-proves identity cannot collide with an unrelated real process. It
// verifies absence through the same production probe the sweep uses rather
// than assuming any particular number is free.
func absentPID(t *testing.T) int {
	t.Helper()
	for pid := 60000; pid < 60500; pid++ {
		obs, err := cmdsession.SystemProbe(cmdsession.Identity{PID: pid})
		if err != nil {
			t.Fatalf("SystemProbe(%d): %v", pid, err)
		}
		if !obs.Present {
			return pid
		}
	}
	t.Skip("no absent pid available in the probe range on this host")
	return 0
}

// seedRetainedSession writes one retained receipt into a fresh store at
// dbPath, in the state the FAC-193 audit found: a completed tool call whose
// shell command session is still alive.
func seedRetainedSession(t *testing.T, dbPath string, pid int) cmdsession.Key {
	t.Helper()
	store, err := cmdsession.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	id := cmdsession.Identity{
		Key:           cmdsession.Key{CoordinatorSession: "coord-cli", ToolCallID: "git-status"},
		PID:           pid,
		ParentPID:     900,
		StartToken:    "start-token",
		PTY:           "ttys004",
		CommandDigest: "sha256:git-status",
		WorkingDir:    "./.herd/worktrees/fac-193",
		TaskRef:       "FAC-193",
		StartedAt:     time.Now().UTC().Add(-3 * time.Hour),
	}
	if _, err := store.Register(id); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store.MarkCompleted(id.Key, cmdsession.OutcomeNormal, nil); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	return id.Key
}

// TestHerdCommandsCLI covers the whole `herd commands` surface plus the
// `herd status` retained line. One binary for the table keeps cmd/herd inside
// its 180s package timeout — the baseline suite already spends most of it.
func TestHerdCommandsCLI(t *testing.T) {
	binary := buildHerd(t)

	t.Run("json reports the retained session with age and task", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "command-sessions.db")
		key := seedRetainedSession(t, dbPath, 4242)

		out, err := exec.Command(binary, "commands", "--json", "--db", dbPath).CombinedOutput()
		if err != nil {
			t.Fatalf("herd commands --json failed: %v\n%s", err, out)
		}
		var sum cmdsession.Summary
		if err := json.Unmarshal(out, &sum); err != nil {
			t.Fatalf("expected valid JSON: %v\n%s", err, out)
		}
		if sum.Retained != 1 {
			t.Fatalf("retained = %d, want 1\n%s", sum.Retained, out)
		}
		if sum.OldestAgeSeconds < int64((2 * time.Hour).Seconds()) {
			t.Errorf("oldest age = %ds, want at least 2h\n%s", sum.OldestAgeSeconds, out)
		}
		if len(sum.Rows) != 1 || sum.Rows[0].Key != key || sum.Rows[0].TaskRef != "FAC-193" {
			t.Fatalf("rows = %+v, want the seeded FAC-193 session", sum.Rows)
		}
	})

	t.Run("human summary on a fresh store", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "command-sessions.db")
		out, err := exec.Command(binary, "commands", "--db", dbPath).CombinedOutput()
		if err != nil {
			t.Fatalf("herd commands failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "retained command sessions: 0") {
			t.Errorf("expected a zero retained count, got:\n%s", out)
		}
	})

	// The dry run must not change a single receipt, so an operator can look
	// before anything is disposed of.
	t.Run("reconcile dry run writes nothing", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "command-sessions.db")
		key := seedRetainedSession(t, dbPath, 4242)

		out, err := exec.Command(binary, "commands", "reconcile", "--db", dbPath).CombinedOutput()
		if err != nil {
			t.Fatalf("herd commands reconcile failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "dry run") {
			t.Errorf("expected a dry-run banner, got:\n%s", out)
		}
		store, err := cmdsession.NewStore(dbPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		defer store.Close()
		r, err := store.Get(key)
		if err != nil || r == nil {
			t.Fatalf("Get: %+v, %v", r, err)
		}
		if r.State != cmdsession.StateCompleted {
			t.Fatalf("dry run changed state to %s", r.State)
		}
	})

	// The seeded PID is not a live process, so an --apply sweep settles the
	// receipt — and, running outside the coordinator, never claims a reap.
	t.Run("reconcile apply settles an absent session without claiming a reap", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "command-sessions.db")
		key := seedRetainedSession(t, dbPath, absentPID(t))

		out, err := exec.Command(binary, "commands", "reconcile", "--apply", "--json", "--db", dbPath).CombinedOutput()
		if err != nil {
			t.Fatalf("herd commands reconcile --apply failed: %v\n%s", err, out)
		}
		var rep cmdsession.Report
		if err := json.Unmarshal(out, &rep); err != nil {
			t.Fatalf("expected valid JSON: %v\n%s", err, out)
		}
		if len(rep.Reaped) != 0 {
			t.Fatalf("an out-of-process sweep claimed a reap: %v", rep.Reaped)
		}
		if len(rep.Settled) != 1 || rep.Settled[0] != key.String() {
			t.Fatalf("settled = %v, want the absent session %s\n%s", rep.Settled, key, out)
		}
		store, err := cmdsession.NewStore(dbPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		defer store.Close()
		r, err := store.Get(key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if r.State != cmdsession.StateSettledAbsent {
			t.Fatalf("state = %s, want %s", r.State, cmdsession.StateSettledAbsent)
		}
	})

	// Fleet status must surface the retained count itself, not merely be able
	// to compute it, so a background terminal cannot hide behind an
	// agent-level working state.
	t.Run("herd status reports retained command sessions", func(t *testing.T) {
		repo := t.TempDir()
		initCmd := exec.Command(binary, "init")
		initCmd.Dir = repo
		if out, err := initCmd.CombinedOutput(); err != nil {
			t.Fatalf("herd init failed: %v\n%s", err, out)
		}
		seedRetainedSession(t, filepath.Join(repo, ".herd", "command-sessions.db"), 4242)

		statusCmd := exec.Command(binary, "status")
		statusCmd.Dir = repo
		statusCmd.Env = append(os.Environ(), "HERD_ROOT="+repo)
		out, err := statusCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("herd status failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "Retained command sessions: 1") {
			t.Errorf("herd status did not report the retained session:\n%s", out)
		}
	})
}

// TestCommandSessionStatusLineDoesNotCreateAStore: `herd status` reading a
// repo with no receipt store must not mint an empty one and then report its
// emptiness as evidence that no background terminals exist.
func TestCommandSessionStatusLineDoesNotCreateAStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "command-sessions.db")

	line, err := commandSessionStatusLine(dbPath, time.Now)
	if err != nil {
		t.Fatalf("commandSessionStatusLine: %v", err)
	}
	if !strings.Contains(line, "no receipt store yet") {
		t.Errorf("line = %q, want it to disclose the missing store", line)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("status created a receipt store at %s", dbPath)
	}

	seedRetainedSession(t, dbPath, 4242)
	line, err = commandSessionStatusLine(dbPath, time.Now)
	if err != nil {
		t.Fatalf("commandSessionStatusLine: %v", err)
	}
	if !strings.Contains(line, "Retained command sessions: 1") {
		t.Errorf("line = %q, want the retained count", line)
	}
}
