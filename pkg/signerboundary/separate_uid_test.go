package signerboundary

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestSeparateUID_UnprovisionedBlocks(t *testing.T) {
	t.Setenv(EnvSignerUID, "")
	t.Setenv(EnvRequesterUID, "")
	t.Setenv(EnvBuilderUID, "")
	_, err := RequireTopology()
	if err == nil {
		t.Fatal("expected block")
	}
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Logf("blocked with: %v", err)
	}
}

func TestSeparateUID_SameUIDRejected(t *testing.T) {
	me := os.Getuid()
	t.Setenv(EnvSignerUID, fmt.Sprintf("%d", me))
	t.Setenv(EnvRequesterUID, fmt.Sprintf("%d", me))
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me+2))
	_, err := RequireTopology()
	if err == nil {
		t.Fatal("S==R must fail")
	}
}

func TestSeparateUID_LiveSuite_BlockedUnlessProvisioned(t *testing.T) {
	// Previously every branch was t.Log/return, so it asserted nothing.
	// Unprovisioned topology must be an explicit BLOCKED, never a soft pass.
	t.Setenv(EnvSignerUID, "")
	t.Setenv(EnvRequesterUID, "")
	t.Setenv(EnvBuilderUID, "")
	t.Setenv(EnvSocketGID, "")
	if _, err := RequireTopology(); err == nil {
		t.Fatal("unprovisioned topology must fail closed")
	} else if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("want ErrUnsupportedPlatform BLOCKED, got %v", err)
	}
	if err := RequireLaunchReady(t.TempDir()); err == nil {
		t.Fatal("launch must not be ready without a provisioned three-UID topology")
	}
}
