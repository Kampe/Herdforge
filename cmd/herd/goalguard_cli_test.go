package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/goalguard"
)

func TestGoalGuardCLISetCheckAndClear(t *testing.T) {
	state := filepath.Join(t.TempDir(), "goal.json")
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"herd", "goal-guard", "--set", "--state", state, "--lane", "forge-worker", "--task", "FAC-308", "--owner", "coordinator", "--generation", "4", "--max", "1"}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}

	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()
	input, err := os.CreateTemp(t.TempDir(), "evidence-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.WriteString(`{"lane":"forge-worker","task":"FAC-308","owner":"coordinator","generation":4,"lease_held":true,"now":"2026-08-16T02:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	os.Stdin = input
	os.Args = []string{"herd", "goal-guard", "--check", "--state", state}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}

	os.Args = []string{"herd", "goal-guard", "--clear", "--state", state}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("clear left state behind: %v", err)
	}
}

func TestGoalGuardCLIMalformedEvidenceFailsClosed(t *testing.T) {
	state := filepath.Join(t.TempDir(), "goal.json")
	oldArgs, oldStdin := os.Args, os.Stdin
	defer func() { os.Args, os.Stdin = oldArgs, oldStdin }()
	os.Args = []string{"herd", "goal-guard", "--check", "--state", state}
	input, err := os.CreateTemp(t.TempDir(), "evidence-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.WriteString(strings.Repeat("{", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	os.Stdin = input
	if err := runGoalGuard(); err == nil {
		t.Fatal("malformed evidence must fail closed")
	}
	_ = input.Close()
}

func TestGoalGuardStopHookAllowsStopWhenLeaseIsLost(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "goal.json")
	dbPath := filepath.Join(dir, ".herd", "launch-claims.db")
	t.Setenv("HERD_LEASE_DB", dbPath)

	store, err := claim.NewSQLiteLeaseStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireWithIdentity(context.Background(), claim.LeaseKey{
		Repo: "repo", Provider: "kaneo", Project: "project", TaskRef: "FAC-472",
	}, "owner-472", "worker", "", "repo", "worker", "forge-worker", time.Now(), time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, changed, err := store.Release(context.Background(), lease.LeaseKey, lease.OwnerID, lease.Generation, time.Now()); err != nil || !changed {
		_ = store.Close()
		t.Fatalf("release lease: changed=%v err=%v", changed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	oldArgs, oldStdin, oldStdout := os.Args, os.Stdin, os.Stdout
	defer func() { os.Args, os.Stdin, os.Stdout = oldArgs, oldStdin, oldStdout }()
	os.Args = []string{"herd", "goal-guard", "--set", "--state", state, "--lane", "forge-worker", "--task", "FAC-472", "--owner", "coordinator", "--generation", strconv.FormatInt(lease.Generation, 10), "--max", "1"}
	if err := runGoalGuard(); err != nil {
		t.Fatal(err)
	}

	input, err := os.CreateTemp(dir, "stop-hook-input-*")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	os.Stdin = input
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	os.Args = []string{"herd", "goal-guard", "--stop-hook", "--state", state}
	if err := runGoalGuard(); err != nil {
		_ = write.Close()
		_ = read.Close()
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	var decision struct {
		Continue bool   `json:"continue"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(read).Decode(&decision); err != nil {
		t.Fatal(err)
	}
	if decision.Continue || decision.Reason != "lease_lost" {
		t.Fatalf("stop-hook decision = %+v, want non-continuing lease_lost", decision)
	}
}

func TestGoalGuardStopHookRejectsContinuationWithoutGrant(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "goal.json")
	dbPath := filepath.Join(dir, ".herd", "launch-claims.db")
	t.Setenv("HERD_LEASE_DB", dbPath)
	store, err := claim.NewSQLiteLeaseStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireWithIdentity(context.Background(), claim.LeaseKey{Repo: "repo", Provider: "kaneo", Project: "project", TaskRef: "FAC-525"}, "owner-525", "worker", "", "repo", "worker", "forge-worker", time.Now().UTC(), time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	defer store.Close()
	s, err := goalguard.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Set(goalguard.Goal{Lane: "forge-worker", Task: "FAC-525", Owner: "coordinator", Generation: lease.Generation, MaxContinuations: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	err = runGoalGuardStopHook(s, nil)
	_ = write.Close()
	os.Stdout = oldStdout
	_ = read.Close()
	if err == nil || !strings.Contains(err.Error(), "no verifiable authority envelope") {
		t.Fatalf("missing grant result=%v, want loud refusal", err)
	}
}
