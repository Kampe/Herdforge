package main

import (
	"os"
	"strings"
	"testing"
)

// TestPoolReviewerIsNotHardwiredToOneHarness is the FAC-574 gate.
//
// The pool launched OpenCode with an Ollama proxy model unconditionally, so no
// native-Claude route existed through the exact-SHA pool lifecycle and a rate
// limited proxy killed exact review entirely. A reviewer harness must come from
// the router, never from a literal in this file.
func TestPoolReviewerIsNotHardwiredToOneHarness(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// Comments legitimately name the old default when explaining the fix, so
	// only inspect code lines.
	for i, line := range strings.Split(body, "\n") {
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "//") {
			continue
		}
		for _, banned := range []string{"litellm/", "ollama", "\"opencode\""} {
			if strings.Contains(strings.ToLower(code), banned) {
				t.Errorf("review_pool.go:%d hardwires a reviewer surface (%q): %s", i+1, banned, code)
			}
		}
	}
	if !strings.Contains(body, "router.NewRouter") {
		t.Error("pool reviewer must resolve its harness through the router")
	}
}

// A model without its surface is not a route; accepting it would silently pair
// an operator's model with whatever provider the router happened to pick.
func TestModelWithoutProviderIsRejected(t *testing.T) {
	if _, err := resolvePoolReviewer("", "claude-sonnet-5", ""); err == nil {
		t.Fatal("expected --model without --provider to be refused")
	}
}

// The launch must resolve before any tab or lease side effect, so an
// unroutable reviewer cannot orphan a tab or strand a warm-pool lease.
func TestHarnessResolvesBeforeSideEffects(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	resolve := strings.Index(body, "resolvePoolReviewer(*provider")
	create := strings.Index(body, "herdr.TabCreate(")
	if resolve < 0 || create < 0 {
		t.Fatal("expected both the reviewer resolve and the tab create in the launch path")
	}
	if resolve > create {
		t.Error("reviewer harness must resolve before the tab is created")
	}
}
