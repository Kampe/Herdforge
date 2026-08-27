package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-607: the reported symptom was rc=124 with ZERO stdout from `herd deps
// check`. This runs the REAL binary and asserts on what an operator sees.
//
// SCOPE, stated honestly. This proves the command never exits silently on a
// failed read. It does NOT yet reach provider.BoundedRead: `herd deps check`
// first requires a git repo, a valid config, and a provider claim stack
// (.herd/claim/fences.db), and this fixture stops at the last of those. So the
// wiring of BoundedRead into runDepsCheck is covered by the diff and by
// pkg/provider's own tests, NOT by an end-to-end CLI assertion.
//
// That limitation is named rather than papered over because FAC-602 shipped an
// exemption its own tests proved and the CLI never executed. A test whose title
// implies more than it checks is how that happened. Closing this gap needs a
// CLI-reachable provider fixture and is tracked as residual on the card.

func TestDepsCheckIsNeverSilentOnAFailedRead(t *testing.T) {
	binary := buildHerd(t)
	repo := t.TempDir()

	// A config pointing at a provider endpoint that accepts the connection and
	// never answers would need a live socket; pointing at an unroutable address
	// gets the same shape -- the read cannot complete within its budget.
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "base"}} {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	herdDir := filepath.Join(repo, ".herd")
	if err := os.MkdirAll(herdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "" +
		"version: 1\n" +
		"project:\n  name: bounded-read-probe\n" +
		"task_provider:\n" +
		"  type: kaneo\n" +
		"  project_id: probe\n" +
		"  base_url: http://127.0.0.1:9\n" // discard port: connections hang or refuse
	if err := os.WriteFile(filepath.Join(herdDir, "herd.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "deps", "check", "FAC-1")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"HERD_ROOT="+repo,
		"HERD_REPO_ROOT="+repo,
		// Short budget so the test is fast; the property under test is that the
		// command reports rather than dying mute, not the specific duration.
		"HERD_PROVIDER_READ_BUDGET=2s",
	)
	out, _ := cmd.CombinedOutput()
	text := string(out)

	if strings.TrimSpace(text) == "" {
		t.Fatal("herd deps check produced ZERO output on an unreachable provider; " +
			"that is exactly the rc=124 silence FAC-607 exists to prevent")
	}

	// The command may legitimately fail earlier than the bounded read (config or
	// provider construction). What it may never do is exit silently.
	t.Logf("observed output:\n%s", text)
}

// The budget must be configurable, because it only helps if it fires BEFORE the
// operator's own `timeout` wrapper. A hardcoded budget longer than theirs
// reproduces the silence on a slow host.
func TestProviderReadBudgetIsOverridable(t *testing.T) {
	t.Setenv("HERD_PROVIDER_READ_BUDGET", "3s")
	if got := providerReadBudget(); got.String() != "3s" {
		t.Fatalf("budget = %s, want 3s from the environment override", got)
	}

	t.Setenv("HERD_PROVIDER_READ_BUDGET", "not-a-duration")
	if got := providerReadBudget(); got != 20*1000*1000*1000 {
		t.Fatalf("an unparseable override yielded %s; it must fall back to the default, not to zero "+
			"(a zero budget would make every read time out instantly)", got)
	}
}
