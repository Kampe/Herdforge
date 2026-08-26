package main

import (
	"os"
	"path/filepath"
	"testing"
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
