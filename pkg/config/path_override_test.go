package config

import (
	"os"
	"path/filepath"
	"testing"
)

// FAC-600: PathFor builds an ABSOLUTE path, and LoadConfig only consults
// HERD_CONFIG_PATH when handed the relative DefaultConfigPath. So every caller
// resolving through PathFor silently bypassed the operator override, which is
// the entire purpose of that variable.
//
// The consequence was that a second host could not be configured at all.
// .herd/herd.yaml is tracked and pins a host-specific fleet.herdr_workspace, so
// a WSL review node was refused with "HERD_WORKSPACE=w2, registered
// workspace=wB; refusing cross-workspace mutation" no matter which profile it
// selected. The only remaining option was editing a tracked file per host, which
// conflicts on every pull.
func TestPathForHonorsOperatorOverride(t *testing.T) {
	t.Setenv("HERD_CONFIG_PATH", "/somewhere/else/herd.wsl.yaml")
	if got := PathFor("/repo/root"); got != "/somewhere/else/herd.wsl.yaml" {
		t.Errorf("PathFor must return the operator profile, got %q", got)
	}
	// An empty root must not resurrect the repo default either.
	if got := PathFor(""); got != "/somewhere/else/herd.wsl.yaml" {
		t.Errorf("PathFor(\"\") must return the operator profile, got %q", got)
	}
}

// Without the override, behaviour is exactly as before: a root-relative join,
// and an empty root yields the repo default. This is the property every existing
// caller depends on.
func TestPathForUnchangedWithoutOverride(t *testing.T) {
	os.Unsetenv("HERD_CONFIG_PATH")
	want := filepath.Join("/repo/root", DefaultConfigPath)
	if got := PathFor("/repo/root"); got != want {
		t.Errorf("PathFor = %q want %q", got, want)
	}
	if got := PathFor(""); got != DefaultConfigPath {
		t.Errorf("PathFor(\"\") = %q want %q", got, DefaultConfigPath)
	}
}

// Whitespace is not a selection. A variable set to blanks must fall through to
// the repo default rather than resolve to a nonsense path, or a stray export in
// a shell profile would silently break every config lookup.
func TestPathForIgnoresBlankOverride(t *testing.T) {
	t.Setenv("HERD_CONFIG_PATH", "   ")
	want := filepath.Join("/repo/root", DefaultConfigPath)
	if got := PathFor("/repo/root"); got != want {
		t.Errorf("blank override must be ignored, got %q want %q", got, want)
	}
}

// RuntimeConfigPath and PathFor must agree on which profile is selected, or the
// two resolution paths disagree about which fleet a lane belongs to — the exact
// class of divergence that has already produced consumer-visible defects here.
func TestPathForAgreesWithRuntimeConfigPath(t *testing.T) {
	t.Setenv("HERD_CONFIG_PATH", "/profiles/wsl.yaml")
	if RuntimeConfigPath() != PathFor("/any/root") {
		t.Errorf("divergent resolution: RuntimeConfigPath=%q PathFor=%q",
			RuntimeConfigPath(), PathFor("/any/root"))
	}
}
