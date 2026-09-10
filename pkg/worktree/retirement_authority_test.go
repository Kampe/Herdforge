package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNativePoolCreationAuthority_DefaultRelativeRootAuthorizesOwnPool is the
// exact reviewer probe (review-fac-708-916d80890d78, P1): with the documented
// default repository root "." and no environment override, a pool created by
// the native primitives carries relative pool.json slot paths, while git
// registers the slot under an absolute path. The authority must bind its own
// legitimate pool through one canonical identity -- not reject it as
// unregistered.
func TestNativePoolCreationAuthority_DefaultRelativeRootAuthorizesOwnPool(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	t.Chdir(root)

	pool := NewPool(".", filepath.Join(".herd", "pool-fac-relative"), 1)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure with default relative root: %v", err)
	}

	authority := NewNativePoolCreationAuthority(".")
	if err := authority.AuthorizePoolRoot(filepath.Join(".herd", "pool-fac-relative")); err != nil {
		t.Fatalf("default relative repository root rejected its own native pool: %v", err)
	}

	// The same authorization must hold when the caller passes the absolute
	// spelling of the same pool root: one identity, either spelling.
	if err := authority.AuthorizePoolRoot(filepath.Join(root, ".herd", "pool-fac-relative")); err != nil {
		t.Fatalf("absolute spelling of the same native pool must authorize identically: %v", err)
	}
}

// TestNativePoolCreationAuthority_UnregisteredSlotRetains pins the
// fail-closed side of the canonicalization: a foreign pool root with no
// native state still refuses, whatever spelling it uses; and a
// native-schema root whose recorded slot has no git registration in this
// repository refuses too -- canonicalization must not loosen the binding.
func TestNativePoolCreationAuthority_UnregisteredSlotRetains(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	t.Chdir(root)

	authority := NewNativePoolCreationAuthority(".")

	foreign := filepath.Join(".herd", "pool-fac-foreign")
	if err := os.MkdirAll(filepath.Join(root, foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	err := authority.AuthorizePoolRoot(foreign)
	if err == nil || !strings.Contains(err.Error(), "no valid native pool.json") {
		t.Fatalf("foreign root without native state must refuse, got %v", err)
	}

	// Native-schema but unbound: the recorded slot is not a registered
	// worktree of this repository.
	unbound := filepath.Join(".herd", "pool-fac-unbound")
	if err := os.MkdirAll(filepath.Join(root, unbound), 0o755); err != nil {
		t.Fatal(err)
	}
	state := `{"version":1,"slots":[{"name":"pool-01","path":"` + filepath.Join(unbound, "pool-01") + `"}]}`
	if err := os.WriteFile(filepath.Join(root, unbound, "pool.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	err = authority.AuthorizePoolRoot(unbound)
	if err == nil || !strings.Contains(err.Error(), "no git registration") {
		t.Fatalf("native-schema root with unregistered slot must refuse, got %v", err)
	}
}
