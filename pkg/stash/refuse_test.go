package stash

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestRefuseSharedStackBranchMatch(t *testing.T) {
	// Integration: real git stash list on a fixture.
	r, dir := newRepo(t)
	if err := writeFile(t, dir, "f.txt", "racy\n"); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "stash", "push", "-m", "racy-entry")

	hits, err := RefuseSharedStack(context.Background(), dir, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("same branch must hit")
	}
	// Case-insensitive: "On feature:" / "WIP on feature:" both match needle.
	joined := strings.ToLower(strings.Join(hits, "\n"))
	if !strings.Contains(joined, "on feature:") {
		t.Fatalf("hits must mention on feature:, got %v", hits)
	}

	// Different branch → no false positive.
	other, err := RefuseSharedStack(context.Background(), dir, "totally-other")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("other branch must not match, got %v", other)
	}

	// Empty / HEAD branch → no hits.
	none, err := RefuseSharedStack(context.Background(), dir, "")
	if err != nil || len(none) != 0 {
		t.Fatalf("empty branch: hits=%v err=%v", none, err)
	}
	none, err = RefuseSharedStack(context.Background(), dir, "HEAD")
	if err != nil || len(none) != 0 {
		t.Fatalf("HEAD branch: hits=%v err=%v", none, err)
	}

	// Detached Repo.SharedStackConflict via Branch() == "".
	run(t, dir, "checkout", "--detach", "HEAD")
	if hits := r.SharedStackConflict(); len(hits) != 0 {
		t.Fatalf("detached must not refuse, got %v", hits)
	}

	msg := FormatSharedStackRefusal("feature", hits)
	if !strings.Contains(msg, "migrate them off the shared stack") {
		t.Fatalf("migrate hint missing: %s", msg)
	}
	if !strings.Contains(msg, "bin/herd-stash push -m migrated") {
		t.Fatalf("exact migrate command missing: %s", msg)
	}
}

func TestRefuseSharedStackStubList(t *testing.T) {
	// Unit seam: stub git stash list output without a real repo.
	orig := execCommandContext
	t.Cleanup(func() { execCommandContext = orig })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Echo a canned stash list via a shell that always succeeds.
		script := `echo 'stash@{0}: WIP on main: deadbeef wip'; echo 'stash@{1}: On feature-x: 1a2b3c4 something'`
		return exec.CommandContext(ctx, "sh", "-c", script)
	}

	hits, err := RefuseSharedStack(context.Background(), "/tmp", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "on main:") && !strings.Contains(strings.ToLower(hits[0]), "on main:") {
		t.Fatalf("want one main hit, got %v", hits)
	}

	fx, err := RefuseSharedStack(context.Background(), "/tmp", "feature-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(fx) != 1 {
		t.Fatalf("want one feature-x hit, got %v", fx)
	}

	none, err := RefuseSharedStack(context.Background(), "/tmp", "unrelated")
	if err != nil || len(none) != 0 {
		t.Fatalf("unrelated must be empty, got %v err=%v", none, err)
	}
}

func writeFile(t *testing.T, dir, name, body string) error {
	t.Helper()
	return osWriteFile(dir, name, body)
}
