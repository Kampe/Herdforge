package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
)

func TestRootGitConfigGuardConcurrentOtherLaneUpstreamIsUnknown(t *testing.T) {
	_, worker, otherLane := newLinkedWorktreeGuardFixture(t)
	before, err := readRootGitState(worker)
	if err != nil {
		t.Fatal(err)
	}

	runGuardFixtureGit(t, otherLane, "branch", "--set-upstream-to=main")
	report, err := compareRootGitState(worker, before)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.failures) != 0 {
		t.Fatalf("concurrent other-lane upstream failed guard: %+v", report.failures)
	}
	if len(report.unknown) == 0 {
		t.Fatal("concurrent other-lane upstream was not reported UNKNOWN")
	}
}

func TestRootGitConfigGuardUnknownReportsExactDelta(t *testing.T) {
	_, worker, otherLane := newLinkedWorktreeGuardFixture(t)
	before, err := readRootGitState(worker)
	if err != nil {
		t.Fatal(err)
	}

	runGuardFixtureGit(t, otherLane, "branch", "--set-upstream-to=main")
	report, err := compareRootGitState(worker, before)
	if err != nil {
		t.Fatal(err)
	}
	want := rootGitDelta{key: "branch.other-lane.merge", before: "<unset>", after: "refs/heads/main"}
	if !containsRootGitDelta(report.unknown, want) {
		t.Fatalf("UNKNOWN deltas = %+v, want %+v", report.unknown, want)
	}
	var output bytes.Buffer
	if failed := writeRootGitGuardReport(&output, report); failed {
		t.Fatalf("UNKNOWN report failed guard: %s", output.String())
	}
	wantOutput := `UNKNOWN: key="branch.other-lane.merge" before="<unset>" after="refs/heads/main"`
	if !strings.Contains(output.String(), wantOutput) {
		t.Fatalf("UNKNOWN output = %q, want exact delta %q", output.String(), wantOutput)
	}
}

func TestRootGitConfigGuardGenuineMutationFailsWithExactDelta(t *testing.T) {
	_, worker, _ := newLinkedWorktreeGuardFixture(t)
	before, err := readRootGitState(worker)
	if err != nil {
		t.Fatal(err)
	}

	runGuardFixtureGit(t, worker, "config", "herd.guard-test-corruption", "true")
	report, err := compareRootGitState(worker, before)
	if err != nil {
		t.Fatal(err)
	}
	want := rootGitDelta{key: "herd.guard-test-corruption", before: "<unset>", after: "true"}
	if !containsRootGitDelta(report.failures, want) {
		t.Fatalf("failure deltas = %+v, want %+v", report.failures, want)
	}
	if len(report.unknown) != 0 {
		t.Fatalf("genuine mutation reported UNKNOWN: %+v", report.unknown)
	}
	var output bytes.Buffer
	if failed := writeRootGitGuardReport(&output, report); !failed {
		t.Fatalf("genuine mutation did not fail guard: %s", output.String())
	}
	wantOutput := `failed: key="herd.guard-test-corruption" before="<unset>" after="true"`
	if !strings.Contains(output.String(), wantOutput) {
		t.Fatalf("failure output = %q, want exact delta %q", output.String(), wantOutput)
	}
}

func TestRootGitConfigGuardPreservesExistingCorruptionDetection(t *testing.T) {
	tests := []struct {
		name   string
		before []string
		after  []string
		want   rootGitDelta
	}{
		{
			name:   "changed value",
			before: []string{"config", "herd.guard-existing", "original"},
			after:  []string{"config", "herd.guard-existing", "changed"},
			want:   rootGitDelta{key: "herd.guard-existing", before: "original", after: "changed"},
		},
		{
			name:   "removed value",
			before: []string{"config", "herd.guard-existing", "original"},
			after:  []string{"config", "--unset", "herd.guard-existing"},
			want:   rootGitDelta{key: "herd.guard-existing", before: "original", after: "<unset>"},
		},
		{
			name:   "changed remote",
			before: []string{"remote", "add", "origin", "../before.git"},
			after:  []string{"remote", "set-url", "origin", "../after.git"},
			want:   rootGitDelta{key: "remote.origin.url", before: "../before.git", after: "../after.git"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, worker, _ := newLinkedWorktreeGuardFixture(t)
			runGuardFixtureGit(t, worker, tc.before...)
			before, err := readRootGitState(worker)
			if err != nil {
				t.Fatal(err)
			}
			runGuardFixtureGit(t, worker, tc.after...)
			report, err := compareRootGitState(worker, before)
			if err != nil {
				t.Fatal(err)
			}
			if !containsRootGitDelta(report.failures, tc.want) {
				t.Fatalf("failure deltas = %+v, want %+v", report.failures, tc.want)
			}
			if len(report.unknown) != 0 {
				t.Fatalf("corruption reported UNKNOWN: %+v", report.unknown)
			}
		})
	}
}

func newLinkedWorktreeGuardFixture(t *testing.T) (root, worker, otherLane string) {
	t.Helper()
	root = t.TempDir()
	runGuardFixtureGit(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGuardFixtureGit(t, root, "add", "tracked.txt")
	runGuardFixtureGit(t, root, "commit", "-m", "fixture")

	worker = filepath.Join(t.TempDir(), "worker")
	otherLane = filepath.Join(t.TempDir(), "other-lane")
	runGuardFixtureGit(t, root, "worktree", "add", "-b", "worker", worker)
	runGuardFixtureGit(t, root, "worktree", "add", "-b", "other-lane", otherLane)
	return root, worker, otherLane
}

func runGuardFixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func containsRootGitDelta(deltas []rootGitDelta, want rootGitDelta) bool {
	for _, delta := range deltas {
		if delta == want {
			return true
		}
	}
	return false
}
