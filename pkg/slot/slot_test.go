package slot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if err := os.Unsetenv(EnvHeld); err != nil {
		fmt.Fprintf(os.Stderr, "clear inherited %s: %v\n", EnvHeld, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestSemaphoreInheritedHeldMarkerStillAcquiresRealSlot(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "slots"), 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Acquire(context.Background(), "inherited-marker-regression", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Errorf("release real slot: %v", err)
		}
	})
	if lease.held || lease.slot == "" {
		t.Fatal("inherited held marker bypassed real filesystem acquisition")
	}
	if _, err := os.Stat(filepath.Join(lease.slot, "holder")); err != nil {
		t.Fatalf("real slot holder was not created: %v", err)
	}
}

func TestSemaphoreNPlusOneTimesOutAndReleaseUnblocks(t *testing.T) {
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
