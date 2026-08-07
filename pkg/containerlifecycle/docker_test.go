package containerlifecycle

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// fakeExec builds an execCommandContext replacement that runs a small
// shell script instead of the real docker binary, so these tests never
// touch a live Docker daemon.
func fakeExec(t *testing.T, script string) {
	t.Helper()
	old := execCommandContext
	t.Cleanup(func() { execCommandContext = old })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		full := append([]string{"-c", script, name}, args...)
		return exec.CommandContext(ctx, "sh", full...)
	}
}

func TestDockerRemoveExactID(t *testing.T) {
	var gotArgs []string
	old := execCommandContext
	t.Cleanup(func() { execCommandContext = old })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgs = append([]string{}, args...)
		return exec.CommandContext(ctx, "true")
	}
	if err := DockerRemove(context.Background(), "abc123"); err != nil {
		t.Fatalf("DockerRemove: %v", err)
	}
	want := []string{"rm", "--force", "abc123"}
	if len(gotArgs) != len(want) {
		t.Fatalf("args = %v, want %v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args = %v, want %v", gotArgs, want)
		}
	}
}

// TestDockerRemoveSurfacesNoSuchContainerAsAnError proves DockerRemove no
// longer decides "already gone = success" by pattern-matching stderr
// text itself — that decision now belongs to EnsureCleanup's
// independent absence check (see cleanup_test.go), which is the
// authoritative signal regardless of docker's exact error wording.
func TestDockerRemoveSurfacesNoSuchContainerAsAnError(t *testing.T) {
	fakeExec(t, `echo "Error: No such container: abc123" 1>&2; exit 1`)
	if err := DockerRemove(context.Background(), "abc123"); err == nil {
		t.Fatal("expected DockerRemove to surface the error, not swallow it")
	}
}

func TestDockerRemoveSurfacesRealFailures(t *testing.T) {
	fakeExec(t, `echo "Cannot connect to the Docker daemon" 1>&2; exit 1`)
	if err := DockerRemove(context.Background(), "abc123"); err == nil {
		t.Fatal("expected error for a real docker failure")
	}
}

func TestDockerRemoveRejectsEmptyID(t *testing.T) {
	if err := DockerRemove(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty container id")
	}
}

func TestDockerAbsentTrueWhenNoSuchContainer(t *testing.T) {
	fakeExec(t, `echo "Error: No such object: abc123" 1>&2; exit 1`)
	absent, err := DockerAbsent(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("DockerAbsent: %v", err)
	}
	if !absent {
		t.Fatal("expected absent=true")
	}
}

func TestDockerAbsentFalseWhenPresent(t *testing.T) {
	fakeExec(t, `echo '[{"Id":"abc123"}]'; exit 0`)
	absent, err := DockerAbsent(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("DockerAbsent: %v", err)
	}
	if absent {
		t.Fatal("expected absent=false when inspect succeeds")
	}
}

// TestDockerListAllPassesNoTrunc proves `docker ps -a` is invoked with
// --no-trunc: without it, docker truncates IDs to 12 characters, which
// would never string-match the full IDs receipts are registered under,
// silently reporting every owned container as unowned.
func TestDockerListAllPassesNoTrunc(t *testing.T) {
	var gotArgs []string
	old := execCommandContext
	t.Cleanup(func() { execCommandContext = old })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgs = append([]string{}, args...)
		return exec.CommandContext(ctx, "true")
	}
	if _, err := DockerListAll(context.Background()); err != nil {
		t.Fatalf("DockerListAll: %v", err)
	}
	found := false
	for _, a := range gotArgs {
		if a == "--no-trunc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("args = %v, want --no-trunc present", gotArgs)
	}
}

func TestDockerListAllParsesRows(t *testing.T) {
	fakeExec(t, `printf 'abc\tfac174-hermetic:v1\tExited (0) 2 days ago\tfac174-h1\t2026-08-04 04:59:37 -0500 CDT\n'`)
	rows, err := DockerListAll(context.Background())
	if err != nil {
		t.Fatalf("DockerListAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	r := rows[0]
	if r.ID != "abc" || r.Image != "fac174-hermetic:v1" || !strings.Contains(r.Status, "Exited") || r.Names != "fac174-h1" {
		t.Fatalf("row = %+v", r)
	}
}

func TestDockerListAllEmpty(t *testing.T) {
	fakeExec(t, `printf ''`)
	rows, err := DockerListAll(context.Background())
	if err != nil {
		t.Fatalf("DockerListAll: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
}
