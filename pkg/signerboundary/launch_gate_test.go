package signerboundary

import (
	"testing"
)

func TestRequireLaunchReady_FailClosedWhenUnprovisioned(t *testing.T) {
	t.Setenv(KeyDirEnv, t.TempDir())
	// Always fail-closed — no soft-open.
	if err := RequireLaunchReady(t.TempDir()); err == nil {
		t.Fatal("unprovisioned must fail closed")
	}
}

func TestEnforceAtLaunch_FailClosed(t *testing.T) {
	t.Setenv(KeyDirEnv, t.TempDir())
	if err := EnforceAtLaunch(t.TempDir()); err == nil {
		t.Fatal("must fail closed")
	}
}

func TestBuilderTabEnv_NotAuthority(t *testing.T) {
	// HERD_ROLE is diagnostic only; presence does not grant sign rights.
	env := BuilderTabEnv()
	found := false
	for _, e := range env {
		if e == "HERD_ROLE=agent" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected diagnostic HERD_ROLE=agent")
	}
}
