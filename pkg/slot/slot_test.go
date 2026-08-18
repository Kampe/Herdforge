package slot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
