package slot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/laneenv"
)

func TestPackageSlotIsolationAvoidsOuterHeldDefault(t *testing.T) {
	requireHeldMarkerAbsent(t)

	outer := filepath.Join(t.TempDir(), "outer-host-slots")
	host, err := New(outer, 1)
	if err != nil {
		t.Fatal(err)
	}
	held, err := host.Acquire(context.Background(), "outer-managed-verifier", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := held.Release(); err != nil {
			t.Errorf("release outer slot: %v", err)
		}
	})

	t.Setenv(EnvDirectory, outer)
	t.Setenv(EnvCount, "1")

	busy, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if busy.Directory() != filepath.Clean(outer) {
		t.Fatalf("watched RED setup: Default dir=%q, want outer %q", busy.Directory(), outer)
	}
	started := time.Now()
	if _, err := busy.Acquire(context.Background(), "in-process-verifier", 200*time.Millisecond); err == nil {
		t.Fatal("watched RED: in-process Default acquire succeeded against the outer held slot")
	}
	if waited := time.Since(started); waited < 150*time.Millisecond {
		t.Fatalf("watched RED: in-process acquire returned in %s without waiting on the outer slot", waited)
	}

	restore, err := laneenv.IsolateDefaultSlotDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)

	isolated, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if isolated.Directory() == filepath.Clean(outer) {
		t.Fatal("package isolation reused the outer managed-verifier slot directory")
	}
	started = time.Now()
	lease, err := isolated.Acquire(context.Background(), "in-process-isolated", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("isolated Default acquire failed: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Errorf("release isolated slot: %v", err)
		}
	})
	if waited := time.Since(started); waited >= 150*time.Millisecond {
		t.Fatalf("isolated Default still waited %s on the outer slot", waited)
	}
}

func requireHeldMarkerAbsent(t *testing.T) {
	t.Helper()
	if err := os.Unsetenv(EnvHeld); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(EnvHeld) == "1" {
		t.Fatal("HERD_HEAVY_PHASE_SLOT_HELD=1 would make a real acquire a no-op")
	}
}

func TestSemaphoreNPlusOneTimesOutAndReleaseUnblocks(t *testing.T) {
	requireHeldMarkerAbsent(t)
	s, err := New(filepath.Join(t.TempDir(), "slots"), 2)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Acquire(context.Background(), "a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Acquire(context.Background(), "b", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(context.Background(), "c", 30*time.Millisecond); err == nil {
		t.Fatal("N+1 acquire unexpectedly succeeded")
	}
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	c, err := s.Acquire(context.Background(), "c", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Release()
	_ = c.Release()
}

func TestSemaphoreRecoversDeadHolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "slots")
	s, err := New(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0", "holder"), []byte("pid=999999999\npurpose=crashed\ntoken=old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := s.Acquire(context.Background(), "recovered", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(l.slot, "holder"))), "purpose=recovered") {
		t.Fatal("stale holder was not replaced")
	}
	_ = l.Release()
}

// TestSemaphoreReclaimsReusedPID reproduces CHA-2054: a vanished holder whose
// PID has since been reused by an unrelated live process (this test process
// itself) must not be mistaken for the original holder still running. Without
// a start-time cross-check, syscall.Kill(pid, 0) alone reports "alive" and
// Acquire blocks for the full maxAge even though the real holder is long gone.
func TestSemaphoreReclaimsReusedPID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "slots")
	s, err := New(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	s.SetMaxAge(time.Hour) // would block for an hour if PID liveness were trusted alone
	if err := os.MkdirAll(filepath.Join(dir, "0"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Our own PID is genuinely alive, but the recorded start time belongs to
	// a different (vanished) process -- simulating PID reuse after the real
	// holder exited.
	holder := fmt.Sprintf("pid=%d\npurpose=stale\ntoken=old\nstart=Mon Jan  1 00:00:00 1990\n", os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "0", "holder"), []byte(holder), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := s.Acquire(context.Background(), "reclaimed", time.Second)
	if err != nil {
		t.Fatalf("reused-PID holder was not reclaimed promptly: %v", err)
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(l.slot, "holder"))), "purpose=reclaimed") {
		t.Fatal("stale holder with reused PID was not replaced")
	}
	_ = l.Release()
}

func TestSemaphoreWithReleasesAfterFailure(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "slots"), 1)
	if err != nil {
		t.Fatal(err)
	}
	want := context.Canceled
	if err := s.With(context.Background(), "failure", time.Second, func() error { return want }); err != want {
		t.Fatalf("With error = %v, want %v", err, want)
	}
	if _, err := s.Acquire(context.Background(), "after", time.Second); err != nil {
		t.Fatalf("slot leaked after callback failure: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
