package worktree

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakePoolGit(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func seededReleasePool(t *testing.T, gitPath string) (*Pool, string, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	pool := NewPool(root, filepath.Join(root, "pool"), 0)
	pool.GitPath = gitPath
	diagnostics := &bytes.Buffer{}
	pool.Diagnostics = diagnostics
	leaseID := "pool-01-exact-lease"
	slotPath := filepath.Join(pool.Root, "pool-01")
	if err := os.MkdirAll(slotPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.writeState(poolState{Version: 1, Slots: []PoolSlot{{
		Name: "pool-01", Path: slotPath, Purpose: "review-fac-682",
		LeaseID: leaseID, LeasedAt: time.Unix(1, 0).UTC(),
	}}}); err != nil {
		t.Fatal(err)
	}
	return pool, leaseID, diagnostics
}

// TestPoolReleaseUsesExitAndPorcelainStdout is the FAC-682 contract: only a
// successful Git exit plus empty porcelain stdout is clean. Successful stderr
// remains visible diagnostic output; real dirt and Git errors retain the lease.
func TestPoolReleaseUsesExitAndPorcelainStdout(t *testing.T) {
	git := fakePoolGit(t, `
case " $* " in
  *" status --porcelain "*)
    printf '%s' "$FAKE_STATUS_STDOUT"
    printf '%s' "$FAKE_STATUS_STDERR" >&2
    exit "${FAKE_STATUS_EXIT:-0}"
    ;;
  *) exit 0 ;;
esac
`)
	tests := []struct {
		name       string
		stdout     string
		stderr     string
		exit       string
		wantErr    string
		wantDiag   string
		wantLeased bool
	}{
		{
			name:     "clean stdout with warning releases",
			stderr:   "warning: ignoring unknown index extension IEOT\n",
			wantDiag: "unknown index extension IEOT",
		},
		{
			name:   "porcelain dirt retains lease",
			stdout: " M candidate.go\n", stderr: "diagnostic beside dirt\n",
			wantErr: "remains dirty", wantDiag: "diagnostic beside dirt", wantLeased: true,
		},
		{
			name:   "nonzero status retains lease",
			stderr: "fatal: index is unreadable\n", exit: "7",
			wantErr: "exit status 7", wantLeased: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FAKE_STATUS_STDOUT", tt.stdout)
			t.Setenv("FAKE_STATUS_STDERR", tt.stderr)
			t.Setenv("FAKE_STATUS_EXIT", tt.exit)
			pool, leaseID, diagnostics := seededReleasePool(t, git)
			err := pool.Release(context.Background(), leaseID)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("release refused: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("release error = %v, want %q", err, tt.wantErr)
			}
			if tt.wantDiag != "" && !strings.Contains(diagnostics.String(), tt.wantDiag) {
				t.Errorf("diagnostic %q was discarded: %q", tt.wantDiag, diagnostics.String())
			}
			slots, slotsErr := pool.Slots()
			if slotsErr != nil {
				t.Fatal(slotsErr)
			}
			if got := slots[0].LeaseID != ""; got != tt.wantLeased {
				t.Errorf("lease held = %v, want %v; slot=%+v", got, tt.wantLeased, slots[0])
			}
			if tt.wantLeased && slots[0].LeaseID != leaseID {
				t.Errorf("failure changed exact lease identity: got %q want %q", slots[0].LeaseID, leaseID)
			}
			if !tt.wantLeased && slots[0].ReleasedLeaseID != leaseID {
				t.Errorf("released identity = %q, want %q", slots[0].ReleasedLeaseID, leaseID)
			}
		})
	}
}

func TestPoolReleaseRetryIsExactAndCannotClearNewLease(t *testing.T) {
	git := fakePoolGit(t, `exit 0`)
	pool, oldLeaseID, _ := seededReleasePool(t, git)
	ctx := context.Background()
	if err := pool.Release(ctx, oldLeaseID); err != nil {
		t.Fatal(err)
	}
	if err := pool.Release(ctx, oldLeaseID); err != nil {
		t.Fatalf("exact release retry must be idempotent: %v", err)
	}
	newLease, err := pool.Lease(ctx, "review-next")
	if err != nil {
		t.Fatalf("admit after release: %v", err)
	}
	if newLease.LeaseID == oldLeaseID {
		t.Fatal("new admission reused the released lease identity")
	}
	if err := pool.Release(ctx, oldLeaseID); err != nil {
		t.Fatalf("stale exact retry should remain idempotent: %v", err)
	}
	if err := pool.Release(ctx, "pool-01-wrong-lease"); err == nil {
		t.Fatal("unknown lease identity must fail closed")
	}
	slots, err := pool.Slots()
	if err != nil {
		t.Fatal(err)
	}
	if slots[0].LeaseID != newLease.LeaseID || slots[0].Purpose != "review-next" {
		t.Fatalf("stale or wrong release altered the new lease: %+v", slots[0])
	}
}

func TestPoolConcurrentReleaseAndAdmissionRemainSerialized(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "status-ready")
	proceed := filepath.Join(t.TempDir(), "status-proceed")
	git := fakePoolGit(t, `
case " $* " in
  *" status --porcelain "*)
    if [ ! -e "$FAKE_PROCEED" ]; then
      printf 'ready\n' > "$FAKE_READY"
      while [ ! -e "$FAKE_PROCEED" ]; do sleep 0.01; done
    fi
    exit 0
    ;;
  *) exit 0 ;;
esac
`)
	t.Setenv("FAKE_READY", ready)
	t.Setenv("FAKE_PROCEED", proceed)
	pool, oldLeaseID, _ := seededReleasePool(t, git)
	ctx := context.Background()
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- pool.Release(ctx, oldLeaseID) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("release did not reach the controlled git status")
		}
		time.Sleep(10 * time.Millisecond)
	}
	type leaseResult struct {
		lease *PoolSlot
		err   error
	}
	leaseDone := make(chan leaseResult, 1)
	go func() {
		lease, err := pool.Lease(ctx, "review-concurrent")
		leaseDone <- leaseResult{lease: lease, err: err}
	}()
	if err := os.WriteFile(proceed, []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-releaseDone; err != nil {
		t.Fatalf("release: %v", err)
	}
	admitted := <-leaseDone
	if admitted.err != nil {
		t.Fatalf("admission after serialized release: %v", admitted.err)
	}
	if admitted.lease == nil || admitted.lease.LeaseID == "" || admitted.lease.LeaseID == oldLeaseID {
		t.Fatalf("concurrent admission identity is invalid: %+v", admitted.lease)
	}
	if err := pool.Release(ctx, oldLeaseID); err != nil {
		t.Fatalf("old release retry: %v", err)
	}
	slots, err := pool.Slots()
	if err != nil {
		t.Fatal(err)
	}
	if slots[0].LeaseID != admitted.lease.LeaseID {
		t.Fatalf("old release retry cleared concurrent admission: %+v", slots[0])
	}
}
