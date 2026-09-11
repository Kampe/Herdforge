package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// brokerArgsSentinelEnv runs the herd binary with cwd set to the supplied
// non-git temporary directory, so any code path that survives argument
// validation deterministically exits at canonicalHerdRoot before touching
// config, provider, socket, or process state. The sentinel file at the
// socket override path proves no broker startup ever mutated it.
func brokerArgsSentinelEnvIn(t *testing.T, dir string) (sentinel string, env []string) {
	t.Helper()
	sentinel = filepath.Join(dir, "sentinel.sock")
	if err := os.WriteFile(sentinel, []byte("sentinel-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sentinel, append(os.Environ(), "HERD_BROKER_SOCK="+sentinel)
}

func runBrokerArgsCase(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(buildHerd(t), args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
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
		sentinel, env := brokerArgsSentinelEnvIn(t, dir)
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
	cases := [][]string{
		{"broker", "serve", "extra"},
		{"broker", "serve", "--socket", "/tmp/x.sock", "extra"},
		{"broker", "--socket", "/tmp/x.sock", "extra"},
		{"broker", "ensure", "extra"},
		{"broker", "ensure", "--socket", "/tmp/x.sock", "extra"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		sentinel, env := brokerArgsSentinelEnvIn(t, dir)
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
// to the serve/ensure paths (no unknown-subcommand rejection). The non-git
// cwd stops every path at canonicalHerdRoot with a generic root error, so no
// server starts and no socket is touched.
func TestBrokerArgsPreservesDocumentedForms(t *testing.T) {
	cases := [][]string{
		{"broker"},
		{"broker", "serve"},
		{"broker", "--socket", "/tmp/x.sock"},
		{"broker", "ensure"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		_, env := brokerArgsSentinelEnvIn(t, dir)
		out, code := runBrokerArgsCase(t, dir, env, tc...)
		if code == 0 {
			t.Fatalf("herd %v: expected the generic hereditary root error in a non-git cwd, got exit 0\n%s", tc, out)
		}
		if strings.Contains(out, "unknown broker subcommand") || strings.Contains(out, "unexpected argument") {
			t.Fatalf("herd %v: documented form rejected: %s", tc, out)
		}
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
		_, env := brokerArgsSentinelEnvIn(t, dir)
		out, code := runBrokerArgsCase(t, dir, env, tc...)
		if code != 0 {
			t.Fatalf("herd %v: help exit %d, want 0\n%s", tc, code, out)
		}
		if !strings.Contains(out, "Usage") {
			t.Fatalf("herd %v: help output missing usage:\n%s", tc, out)
		}
	}
}
