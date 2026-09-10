package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/security"
)

// TestPreflightGatesOnWorkerBrokerability is the FAC-579 gate.
//
// The previous preflight ran a real generation as a child of THIS process and
// reported Available; the pane launch then failed with "pane is at a login or
// authentication screen". Both were true — an in-process probe proves the
// coordinator's credential context, not the pane's. A preflight that cannot
// observe the boundary it claims to check is worse than none: it converts an
// honest failure into a false pass.
func TestPreflightGatesOnWorkerBrokerability(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := funcBody(string(src), "func preflightReviewerReadiness")
	if !ok {
		t.Fatal("cannot locate preflightReviewerReadiness")
	}
	if !strings.Contains(body, "DiagnoseKindAuthReadiness") {
		t.Error("preflight must gate on worker credential brokerability, the boundary the launch hits")
	}
	// The brokerability gate must come FIRST: an in-process probe must never be
	// able to stand in for it.
	gate := strings.Index(body, "DiagnoseKindAuthReadiness")
	probe := strings.Index(body, "ProbeProviderModel")
	if probe >= 0 && gate > probe {
		t.Error("brokerability must be checked before the in-process probe, never after")
	}
	if !strings.Contains(body, "hostcreds diagnose") {
		t.Error("the refusal should point at the matching diagnostic command")
	}
}

// A kind that is not brokerable must be refused with the real blocker and an
// action, before any lease or tab.
func TestUnbrokerableKindIsRefusedWithAction(t *testing.T) {
	d := security.DiagnoseKindAuthReadiness("unknown-kind")
	if d.Brokerable {
		t.Fatal("unknown-kind must not be brokerable")
	}
	err := preflightReviewerReadiness(poolReviewer{
		Kind: "unknown-kind", Provider: "unknown-provider", Model: "unknown-model",
	})
	if err == nil {
		t.Fatal("a kind whose worker credentials cannot be brokered must be refused")
	}
	msg := err.Error()
	for _, want := range []string{d.Blocker, "No lease or tab was created", "hostcreds diagnose"} {
		if want != "" && !strings.Contains(msg, want) {
			t.Errorf("refusal must include %q, got: %v", want, msg)
		}
	}
	// It must say plainly why an interactive login is not enough, since that is
	// the exact confusion this defect produced.
	if !strings.Contains(msg, "different credential context") {
		t.Error("refusal should explain that the pane runs in a different credential context")
	}
}

// FAC-791: OpenCode is a supported native vendor harness and passes the brokerability preflight gate.
func TestOpenCodeReviewerPassesAuthPreflight(t *testing.T) {
	d := security.DiagnoseKindAuthReadiness("opencode")
	if !d.Brokerable {
		t.Fatalf("opencode must be brokerable via native harness auth, got: %+v", d)
	}
	if d.AuthorityClass != "native" {
		t.Errorf("authority class = %q, want native", d.AuthorityClass)
	}
	if d.ReasonCode != "native_auth" {
		t.Errorf("reason code = %q, want native_auth", d.ReasonCode)
	}
	if d.Class != security.KindAuthOK {
		t.Errorf("class = %q, want ok", d.Class)
	}
}

// TestPreflightSupportedKindsBrokerable asserts that all supported vendor harnesses
// pass the auth brokerability gate.
func TestPreflightSupportedKindsBrokerable(t *testing.T) {
	// 1. When claude is logged in, all 5 supported kinds pass.
	restoreLoggedIn := security.SetHarnessLoginProbeForTesting(func(ctx context.Context, kind string) ([]byte, error) {
		return []byte(`{"loggedIn": true}`), nil
	})
	for _, kind := range []string{"claude", "codex", "grok", "agy", "opencode"} {
		d := security.DiagnoseKindAuthReadiness(kind)
		if !d.Brokerable {
			t.Errorf("harness %s must be brokerable via native auth, got %+v", kind, d)
		}
	}
	restoreLoggedIn()

	// 2. When claude is logged out, claude is refused while the other 4 kinds pass.
	restoreLoggedOut := security.SetHarnessLoginProbeForTesting(func(ctx context.Context, kind string) ([]byte, error) {
		return []byte(`{"loggedIn": false}`), nil
	})
	defer restoreLoggedOut()

	for _, kind := range []string{"claude", "codex", "grok", "agy", "opencode"} {
		d := security.DiagnoseKindAuthReadiness(kind)
		if kind == "claude" {
			if d.Brokerable {
				t.Errorf("%s is logged out and must not be brokerable: %+v", kind, d)
			}
			if d.ReasonCode != "harness_not_logged_in" {
				t.Errorf("claude reason = %q, want harness_not_logged_in", d.ReasonCode)
			}
		} else {
			if !d.Brokerable {
				t.Errorf("harness %s must remain brokerable, got %+v", kind, d)
			}
		}
	}
}

// TestPreflightRefusesLoggedOutClaudeWithoutLeaseOrTab asserts that a logged-out
// claude harness is refused before any provider probe or lease/tab creation.
func TestPreflightRefusesLoggedOutClaudeWithoutLeaseOrTab(t *testing.T) {
	restore := security.SetHarnessLoginProbeForTesting(func(ctx context.Context, kind string) ([]byte, error) {
		return []byte(`{"loggedIn": false}`), nil
	})
	defer restore()

	d := security.DiagnoseKindAuthReadiness("claude")
	if d.Brokerable {
		t.Fatalf("logged-out claude must not be brokerable: %+v", d)
	}

	err := preflightReviewerReadiness(poolReviewer{
		Kind: "claude", Provider: "anthropic", Model: "claude-3-5-sonnet",
	})
	if err == nil {
		t.Fatal("preflightReviewerReadiness must return an error for logged-out claude")
	}

	msg := err.Error()
	for _, want := range []string{
		"FAC-576 BLOCKED: the claude harness is installed but not logged in",
		"No lease or tab was created.",
		"log in with the claude CLI on this host; no API key is used or wanted",
		"hostcreds diagnose --kind claude",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message must contain %q, got: %s", want, msg)
		}
	}
}

// TestPreflightRefusesLoggedOutClaudeFakeBinaryOnPATH asserts that an executable
// claude CLI on PATH returning `{"loggedIn": false}` triggers the logged-out
// refusal in preflightReviewerReadiness without creating leases or tabs.
func TestPreflightRefusesLoggedOutClaudeFakeBinaryOnPATH(t *testing.T) {
	tmpDir := t.TempDir()
	fakeClaude := filepath.Join(tmpDir, "claude")
	script := "#!/bin/sh\necho '{\"loggedIn\":false}'\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+origPath)

	// Ensure no in-process hook overrides the CLI execution.
	restore := security.SetHarnessLoginProbeForTesting(nil)
	defer restore()

	if state := security.HarnessLoginState("claude"); state != security.HarnessLoggedOut {
		t.Fatalf("HarnessLoginState with fake binary on PATH = %q, want %q", state, security.HarnessLoggedOut)
	}

	err := preflightReviewerReadiness(poolReviewer{
		Kind: "claude", Provider: "anthropic", Model: "claude-3-5-sonnet",
	})
	if err == nil {
		t.Fatal("preflightReviewerReadiness must fail on logged-out fake claude binary")
	}
	if !strings.Contains(err.Error(), "FAC-576 BLOCKED: the claude harness is installed but not logged in") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "No lease or tab was created.") {
		t.Errorf("refusal must state no lease or tab was created: %v", err)
	}
}
