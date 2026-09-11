package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// brokerArgsTimeout bounds every child run: if argument validation ever
// regresses far enough to start serving, the context terminates the child
// instead of hanging the suite, and the test fails on the timeout.
const brokerArgsTimeout = 60 * time.Second

// brokerArgsEnv execs the herd binary with cwd in the supplied non-git
// temporary directory and a fully sanitized environment: inherited git,
// herd root, and broker variables are dropped so the fixture cannot resolve
// live canonical state, and the only socket override is the sentinel path.
// Any code path that survives argument validation therefore exits
// deterministically at canonicalHerdRoot (git common dir lookup) before
// touching config, provider, socket, or process state.
func brokerArgsEnvIn(t *testing.T, dir string) (sentinel string, env []string) {
	t.Helper()
	sentinel = filepath.Join(dir, "sentinel.sock")
	if err := os.WriteFile(sentinel, []byte("sentinel-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sentinel, []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + dir,
		"HERD_BROKER_SOCK=" + sentinel,
	}
}

func runBrokerArgsCase(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), brokerArgsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, buildHerd(t), args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("herd %v: exceeded %s — argument validation failed to stop before broker startup\n%s", args, brokerArgsTimeout, out)
	}
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run herd %v: %v\n%s", args, err, out)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

func assertSentinelUntouched(t *testing.T, sentinel string) {
	t.Helper()
	body, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("sentinel file was removed or mutated by broker startup: %v", err)
	}
	if string(body) != "sentinel-body" {
		t.Fatalf("sentinel file content changed: %q", string(body))
	}
}

// TestBrokerArgsRejectsUnknownSubcommands is the FAC-806 hermetic CLI
// regression: unsupported words previously fell through to runBrokerServe
// (herd broker status started a persistent server). Rejection must happen
// with an actionable nonzero error before any mutation.
func TestBrokerArgsRejectsUnknownSubcommands(t *testing.T) {
	cases := [][]string{
		{"broker", "status"},
		{"broker", "stat"},
		{"broker", "stop"},
		{"broker", "extra"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		sentinel, env := brokerArgsEnvIn(t, dir)
		out, code := runBrokerArgsCase(t, dir, env, tc...)
		if code == 0 {
			t.Fatalf("herd %v: exit 0, want nonzero\n%s", tc, out)
		}
		if !strings.Contains(out, "unknown broker subcommand") {
			t.Fatalf("herd %v: actionable unknown-subcommand error missing, got:\n%s", tc, out)
		}
		assertSentinelUntouched(t, sentinel)
	}
}

// TestBrokerArgsRejectsExtraPositionals covers positional arguments before
// and after valid flags on both serve and ensure paths.
func TestBrokerArgsRejectsExtraPositionals(t *testing.T) {
	for _, tc := range [][]string{
		{"broker", "serve", "extra"},
		{"broker", "serve", "--socket", filepath.Join(t.TempDir(), "x.sock"), "extra"},
		{"broker", "--socket", filepath.Join(t.TempDir(), "x.sock"), "extra"},
		{"broker", "ensure", "extra"},
		{"broker", "ensure", "--socket", filepath.Join(t.TempDir(), "x.sock"), "extra"},
	} {
		dir := t.TempDir()
		sentinel, env := brokerArgsEnvIn(t, dir)
		out, code := runBrokerArgsCase(t, dir, env, tc...)
		if code == 0 {
			t.Fatalf("herd %v: exit 0, want nonzero\n%s", tc, out)
		}
		if !strings.Contains(out, "unexpected argument") {
			t.Fatalf("herd %v: actionable unexpected-argument error missing, got:\n%s", tc, out)
		}
		assertSentinelUntouched(t, sentinel)
	}
}

// TestBrokerArgsPreservesDocumentedForms proves valid invocation forms keep
// their routing: bare broker, broker serve, broker ensure, and flags resolve
// to the serve/ensure paths and stop at the canonical-root lookup. Requiring
// the git-common-dir failure signature pins the INTENDED stop point, so an
// unrelated parser regression that rejects valid forms cannot pass. The
// non-git cwd keeps every path before any server start or socket mutation.
func TestBrokerArgsPreservesDocumentedForms(t *testing.T) {
	for _, tc := range [][]string{
		{"broker"},
		{"broker", "serve"},
		{"broker", "--socket", filepath.Join(t.TempDir(), "x.sock")},
		{"broker", "ensure"},
	} {
		dir := t.TempDir()
		sentinel, env := brokerArgsEnvIn(t, dir)
		out, code := runBrokerArgsCase(t, dir, env, tc...)
		if code == 0 {
			t.Fatalf("herd %v: expected the canonical-root error in a non-git cwd, got exit 0\n%s", tc, out)
		}
		if !strings.Contains(out, "git common dir") {
			t.Fatalf("herd %v: did not stop at the canonical-root lookup (misrouted or rejected):\n%s", tc, out)
		}
		if strings.Contains(out, "unknown broker subcommand") || strings.Contains(out, "unexpected argument") {
			t.Fatalf("herd %v: documented form rejected: %s", tc, out)
		}
		assertSentinelUntouched(t, sentinel)
	}
}

// TestBrokerArgsHelpPreserved covers the documented help surfaces.
func TestBrokerArgsHelpPreserved(t *testing.T) {
	for _, tc := range [][]string{
		{"broker", "-h"},
		{"broker", "--help"},
		{"broker", "serve", "-h"},
		{"broker", "ensure", "-h"},
	} {
		dir := t.TempDir()
		_, env := brokerArgsEnvIn(t, dir)
		out, code := runBrokerArgsCase(t, dir, env, tc...)
		if code != 0 {
			t.Fatalf("herd %v: help exit %d, want 0\n%s", tc, code, out)
		}
		if !strings.Contains(out, "Usage") {
			t.Fatalf("herd %v: help output missing usage:\n%s", tc, out)
		}
	}
}
