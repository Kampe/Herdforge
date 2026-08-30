package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewlaunch"
)

func fenceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERD_HOST_FENCE_DIR", dir)
	return dir
}

func TestHostFenceRoundTrips(t *testing.T) {
	fenceDir(t)
	if _, fenced, err := readHostFence("wsl-box"); err != nil || fenced {
		t.Fatalf("a host with no fence file read as fenced: %v %v", fenced, err)
	}
	if err := writeHostFence(hostFence{Host: "wsl-box", Reason: "banner exchange timeout", FencedAt: "2026-08-26T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	f, fenced, err := readHostFence("wsl-box")
	if err != nil || !fenced || f.Reason != "banner exchange timeout" {
		t.Fatalf("fence did not survive a round trip: %+v %v %v", f, fenced, err)
	}
	if err := clearHostFence("wsl-box"); err != nil {
		t.Fatal(err)
	}
	if _, fenced, _ := readHostFence("wsl-box"); fenced {
		t.Fatal("fence survived an explicit recovery")
	}
}

func TestUnreadableFenceCountsAsFenced(t *testing.T) {
	// The file exists to stop launches. Failing to parse it must not be the
	// thing that lets one through -- that is absence-as-permission, and it
	// would fail open at exactly the moment the fence matters most.
	dir := fenceDir(t)
	if err := os.WriteFile(filepath.Join(dir, "wsl-box.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, fenced, err := readHostFence("wsl-box")
	if err != nil || !fenced {
		t.Fatalf("a malformed fence file opened the host: %+v %v %v", f, fenced, err)
	}
}

func TestFenceOnOneHostDoesNotFenceAnother(t *testing.T) {
	fenceDir(t)
	if err := writeHostFence(hostFence{Host: "wsl-box", Reason: "down"}); err != nil {
		t.Fatal(err)
	}
	if _, fenced, _ := readHostFence("other-box"); fenced {
		t.Fatal("fencing one review host fenced an unrelated one")
	}
}

// FAC-635: this exercises the shipped review-host command path, not only the
// package validator. Drift must stop before the launcher reaches capacity
// claim, worktree preparation, lease creation, or tab creation.
func TestReviewHostVersionGateRefusesDriftBeforeDispatch(t *testing.T) {
	dir := fenceDir(t)
	prior := reviewHostVersionCheck
	t.Cleanup(func() { reviewHostVersionCheck = prior })

	local := "4336f1db222d98678881e4d42a43ed108e4f6cdb"
	remote := "29e2d50d2579ee0b94f04ffded528c2f17e16e95"
	called := 0
	reviewHostVersionCheck = func(_ context.Context, host, remoteHerd, requiredCommand string) (reviewHostVersionEvidence, error) {
		called++
		if host != "w4" || remoteHerd != "$HOME/Projects/Herdforge/bin/herd" || requiredCommand != "capacity" {
			t.Fatalf("version gate received wrong launch contract: host=%q binary=%q command=%q", host, remoteHerd, requiredCommand)
		}
		return reviewHostVersionEvidence{}, &reviewlaunch.VersionDriftError{
			Requirement: reviewlaunch.VersionRequirement{RequiredCommand: requiredCommand, LocalRevision: local, RemoteRevision: remote},
			Cause:       "launcher and remote herd revisions differ",
		}
	}

	err := runReviewHostFence([]string{"--host", "w4", "--check-version", "--require-command", "capacity", "--remote-herd", "$HOME/Projects/Herdforge/bin/herd"})
	if err == nil {
		t.Fatal("drifted remote herd reached dispatch")
	}
	if called != 1 {
		t.Fatalf("version gate calls = %d, want exactly one", called)
	}
	for _, want := range []string{"capacity", local, remote, "version drift"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("shipped-path drift diagnostic omitted %q: %v", want, err)
		}
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("version refusal mutated host state before dispatch: %v", entries)
	}
}

func TestReviewHostVersionGateRejectsNonPositiveTimeoutWithoutProbe(t *testing.T) {
	fenceDir(t)
	prior := reviewHostVersionCheck
	t.Cleanup(func() { reviewHostVersionCheck = prior })
	reviewHostVersionCheck = func(context.Context, string, string, string) (reviewHostVersionEvidence, error) {
		return reviewHostVersionEvidence{}, errors.New("probe must not run")
	}
	if err := runReviewHostFence([]string{"--check-version", "--timeout", "0s"}); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("non-positive timeout did not fail before probing: %v", err)
	}
}

func TestCheckReviewHostVersionStopsStaleBinaryBeforeRequiredCommand(t *testing.T) {
	priorRevision := reviewHostLocalRevision
	priorCommand := reviewHostRemoteCommand
	t.Cleanup(func() {
		reviewHostLocalRevision = priorRevision
		reviewHostRemoteCommand = priorCommand
	})

	local := "4336f1db222d98678881e4d42a43ed108e4f6cdb"
	remote := "29e2d50d2579ee0b94f04ffded528c2f17e16e95"
	reviewHostLocalRevision = func() (string, error) { return local, nil }
	var calls [][]string
	reviewHostRemoteCommand = func(_ context.Context, host, remoteHerd string, args ...string) ([]byte, error) {
		call := append([]string{host, remoteHerd}, args...)
		calls = append(calls, call)
		if len(args) == 1 && args[0] == "--version" {
			return []byte("herd version 0.2.0-dev (revision " + remote + ", build time unknown)\n"), nil
		}
		return nil, errors.New("required command was reached after stale revision")
	}

	_, err := checkReviewHostVersion(context.Background(), "w4", "$HOME/Projects/Herdforge/bin/herd", "capacity")
	if err == nil {
		t.Fatal("stale remote herd reached the required command")
	}
	if len(calls) != 1 {
		t.Fatalf("remote calls = %v, want only the read-only version probe and no capacity command or claim", calls)
	}
	wantCall := []string{"w4", "$HOME/Projects/Herdforge/bin/herd", "--version"}
	if strings.Join(calls[0], "\x00") != strings.Join(wantCall, "\x00") {
		t.Fatalf("first remote call = %v, want %v", calls[0], wantCall)
	}
	for _, want := range []string{"capacity", local, remote, "version drift"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("pre-command drift diagnostic omitted %q: %v", want, err)
		}
	}
}
