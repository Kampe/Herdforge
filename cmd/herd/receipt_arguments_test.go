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

// FAC-814: herd receipt issue --role integration FAC-613 <worktree> --dry-run
// returned exit 0 and actually issued a real lease claim. flag.Parse stops
// at the first non-flag argument, so a trailing --dry-run after the two
// documented positionals (<ref> <worktree>) is never parsed as a flag -- it
// silently becomes a third, ignored positional, and the old fs.NArg() < 2
// check accepted 3 positionals as satisfying "at least 2". No --dry-run flag
// is defined on any of issue/recover/release: any trailing or unknown extra
// must be rejected before canonicalHerdRoot/config/task-provider/lease code
// ever runs.
//
// Each case runs in a non-git, config-less temp directory. The distinguishing
// proof of zero effect is which error text appears: the usage line means the
// argument gate refused before any provider/lease/session code ran; any
// other error (e.g. "is not a readable worktree", "no fence broker", a task
// provider or config error) would mean the rejected arguments were not
// caught and the process proceeded into real backing-state access.
func receiptArgsEnvIn(t *testing.T, dir string) []string {
	t.Helper()
	return []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
}

func runReceiptArgsCase(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, buildHerd(t), args...)
	cmd.Dir = dir
	cmd.Env = receiptArgsEnvIn(t, dir)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("herd %v: exceeded 60s\n%s", args, out)
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

func TestReceiptArgsRejectsTrailingDryRun(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "issue trailing --dry-run",
			args: []string{"receipt", "issue", "--role", "integration", "FAC-613", "wt", "--dry-run"},
			want: "usage: herd receipt issue",
		},
		{
			name: "recover trailing --dry-run",
			args: []string{"receipt", "recover", "FAC-613", "wt", "--dry-run"},
			want: "usage: herd receipt recover",
		},
		{
			name: "release trailing --dry-run",
			args: []string{"receipt", "release", "--role", "integration", "FAC-613", "wt", "--dry-run"},
			want: "usage: herd receipt release",
		},
		{
			name: "issue leading --dry-run",
			args: []string{"receipt", "issue", "--dry-run", "--role", "integration", "FAC-613", "wt"},
			want: "flag provided but not defined",
		},
		{
			name: "issue trailing unknown option",
			args: []string{"receipt", "issue", "--role", "integration", "FAC-613", "wt", "--unsupported-flag"},
			want: "usage: herd receipt issue",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out, code := runReceiptArgsCase(t, dir, tc.args...)
			if code == 0 {
				t.Fatalf("herd %v: exit 0, want nonzero\n%s", tc.args, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("herd %v: expected %q, got:\n%s", tc.args, tc.want, out)
			}
			// Zero-effect proof: the argument gate refuses before
			// canonicalHerdRoot ever runs, so no .herd state of any kind is
			// created in the temp HOME/cwd this case ran in.
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatalf("read temp dir: %v", readErr)
			}
			for _, e := range entries {
				if e.Name() == ".herd" {
					t.Fatalf("argument rejection must precede any state write, found: %s", filepath.Join(dir, e.Name()))
				}
			}
		})
	}
}

// Documented valid shapes must keep reaching real processing (proceeding
// past the argument gate to the next real check: an unreadable worktree,
// since none of these temp dirs is a git repository).
func TestReceiptArgsPreservesDocumentedForms(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "issue documented form", args: []string{"receipt", "issue", "--role", "integration", "FAC-613", "wt"}},
		{name: "recover documented form", args: []string{"receipt", "recover", "FAC-613", "wt"}},
		{name: "release documented form", args: []string{"receipt", "release", "--role", "integration", "FAC-613", "wt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out, code := runReceiptArgsCase(t, dir, tc.args...)
			if code == 0 {
				t.Fatalf("herd %v: exit 0 in a non-git config-less dir, want a downstream failure\n%s", tc.args, out)
			}
			if strings.Contains(out, "usage: herd receipt") {
				t.Fatalf("documented form incorrectly rejected at the argument gate: %s", out)
			}
		})
	}
}
