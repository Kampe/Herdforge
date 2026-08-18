package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPoolLeaseReleaseAndDirtyRefusal(t *testing.T) {
	tests := []struct {
		name  string
		dirty bool
		want  string
	}{
		{name: "clean slot leases", want: "lease"},
		{name: "dirty slot refuses", dirty: true, want: "dirty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			initRepo(t, root)
			pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
			if err := pool.Ensure(context.Background()); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if tc.dirty {
				if err := os.WriteFile(filepath.Join(pool.Root, "pool-01", "dirty.txt"), []byte("dirty"), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Lease(context.Background(), "review"); err == nil || !strings.Contains(err.Error(), "dirty") {
					t.Fatalf("Lease error = %v, want dirty refusal", err)
				}
				return
			}
			lease, err := pool.Lease(context.Background(), "review")
			if err != nil {
				t.Fatalf("Lease: %v", err)
			}
			if lease.Purpose != "review" || lease.LeaseID == "" {
				t.Fatalf("unexpected lease: %+v", lease)
			}
			if err := os.WriteFile(filepath.Join(lease.Path, "generated.txt"), []byte("generated"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := pool.Release(context.Background(), lease.LeaseID); err != nil {
				t.Fatalf("Release: %v", err)
			}
			again, err := pool.Lease(context.Background(), "second")
			if err != nil {
				t.Fatalf("re-lease: %v", err)
			}
			if again.Purpose != "second" {
				t.Fatalf("purpose = %q, want second", again.Purpose)
			}
		})
	}
}

func TestPoolLeaseIsDeterministicAndExhausts(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 2)
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, err := pool.Lease(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := pool.Lease(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "pool-01" || b.Name != "pool-02" {
		t.Fatalf("lease order = %s, %s", a.Name, b.Name)
	}
	if _, err := pool.Lease(context.Background(), "c"); err == nil {
		t.Fatal("expected exhausted pool refusal")
	}
}

func TestSeedCloneCopiesWarmTree(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	if err := os.MkdirAll(filepath.Join(source, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "dist.js"), []byte("built"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SeedClone(context.Background(), source, destination); err != nil {
		t.Fatalf("SeedClone: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "dist.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "built" {
		t.Fatalf("copied data = %q", data)
	}
}
