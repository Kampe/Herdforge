package config

import (
	"path/filepath"
	"testing"
)

// TestPathForIsRootAware is the FAC-574 gate.
//
// DefaultConfigPath already existed, but callers that needed a root prefix could
// not express it and hand-joined the segments instead — five of them. Location
// divergence has already produced two consumer-visible defects here (the handoff
// mailbox and the review corpus each resolving differently by working
// directory), and the config is what tells a lane which fleet it belongs to.
func TestPathForIsRootAware(t *testing.T) {
	if got, want := PathFor("/tmp/repo"), filepath.Join("/tmp/repo", ".herd", "herd.yaml"); got != want {
		t.Errorf("PathFor(root) = %q, want %q", got, want)
	}
	// An empty root must yield the repo-relative default, so this is a drop-in
	// for both call shapes and a caller cannot accidentally produce "/.herd/...".
	if got := PathFor(""); got != DefaultConfigPath {
		t.Errorf("PathFor(\"\") = %q, want the repo-relative default %q", got, DefaultConfigPath)
	}
	if got := PathFor("   "); got != DefaultConfigPath {
		t.Errorf("blank root must behave as empty, got %q", got)
	}
}

// The two forms must agree, or routing a caller through PathFor would silently
// change which file it reads.
func TestPathForAgreesWithTheDefault(t *testing.T) {
	if PathFor(".") != filepath.Join(".", DefaultConfigPath) {
		t.Error("PathFor(\".\") must be the default under \".\"")
	}
}
