package park

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	exec.Command("git", "-C", dir, "config", "user.email", "test@test").Run()
	exec.Command("git", "-C", dir, "config", "user.name", "Test").Run()
	exec.Command("git", "-C", dir, "config", "commit.gpgSign", "false").Run()
	exec.Command("git", "-C", dir, "config", "tag.gpgSign", "false").Run()
	exec.Command("git", "-C", dir, "config", "gpg.program", "/bin/false").Run()
	exec.Command("git", "-C", dir, "config", "user.signingkey", "").Run()
	exec.Command("git", "-C", dir, "checkout", "-b", "main").Run()
	return dir
}

func gitExec(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeCommit(t *testing.T, dir, file, content, msg string) string {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	gitExec(t, dir, "add", file)
	gitExec(t, dir, "-c", "user.signingkey=", "commit", "--no-gpg-sign", "-m", msg)
	return gitExec(t, dir, "rev-parse", "HEAD")
}

func shaOf(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func shortSHA(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", ref).CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse --short %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func TestPark_ParksAndTags(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	c1 := writeCommit(t, dir, "a.txt", "a", "first")
	writeCommit(t, dir, "b.txt", "b", "second")

	t.Run("park at HEAD", func(t *testing.T) {
		res, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "my-feature", "HEAD", "park the feature")
		if err == nil {
			t.Fatal("expected push failure (no remote)")
		}
		if res.Tag != "parked/my-feature" {
			t.Errorf("Tag = %q, want parked/my-feature", res.Tag)
		}
		if res.ShortSHA != shortSHA(t, dir, "HEAD") {
			t.Errorf("ShortSHA = %q, want %q", res.ShortSHA, shortSHA(t, dir, "HEAD"))
		}
	})

	t.Run("park with explicit SHA", func(t *testing.T) {
		res, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "explicit", c1, "park explicit")
		if err == nil {
			t.Fatal("expected push failure (no remote)")
		}
		if res.Tag != "parked/explicit" {
			t.Errorf("Tag = %q", res.Tag)
		}
	})

	t.Run("error on non-commit", func(t *testing.T) {
		_, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "bad", "deadbeef", "msg")
		if err == nil {
			t.Fatal("expected error for invalid SHA")
		}
	})

	t.Run("error on empty message", func(t *testing.T) {
		_, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "feat", "HEAD", "  ")
		if err == nil {
			t.Fatal("expected error for empty message")
		}
	})
}

func TestPark_Slugify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Hello World", "hello-world"},
		{"UPPER_CASE", "upper-case"},
		{"special chars!@#$", "special-chars"},
		{"  trim  ", "trim"},
		{"a", "a"},
	}
	for _, tc := range tests {
		got := Slugify(tc.in)
		if got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPark_PushFailure(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	writeCommit(t, dir, "x", "x", "x")

	// No remote configured -> push should fail
	res, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "nopush", "HEAD", "will fail push")
	if err == nil {
		t.Fatal("expected push failure")
	}
	if !strings.Contains(err.Error(), "push") {
		t.Errorf("error should mention push, got: %v", err)
	}
	_ = res
}

func TestList(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	writeCommit(t, dir, "a", "a", "a")
	Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "one", "HEAD", "first")
	writeCommit(t, dir, "b", "b", "b")
	Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "two", "HEAD", "second")

	result, err := List(ctx, dir, ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(result.Commits) != 2 {
		t.Errorf("got %d commits, want 2", len(result.Commits))
	}
}

func TestAudit(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	writeCommit(t, dir, "a", "a", "a")
	Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "testable", "HEAD", "testable park")

	result, err := Audit(ctx, dir)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1", result.Total)
	}
	// LocalTagOnly because no remote
	if len(result.Entries) == 1 && result.Entries[0].Durability != LocalTagOnly {
		t.Errorf("durability = %v, want LocalTagOnly", result.Entries[0].Durability)
	}
	if !VerifyAuditExit(result) {
		t.Errorf("VerifyAuditExit = false, want true (exposed-only check)")
	}
}

func TestHygiene(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	writeCommit(t, dir, "a", "a", "a")
	Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "active", "HEAD", "active park")

	result, err := Hygiene(ctx, dir)
	if err != nil {
		t.Fatalf("Hygiene: %v", err)
	}
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1", result.Total)
	}
	if !VerifyHygieneExit(result) {
		t.Errorf("VerifyHygieneExit for active-only should be true")
	}
}

func TestReap(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	writeCommit(t, dir, "a", "a", "a")
	Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "to-reap", "HEAD", "will be reaped")

	// Reap requires bin/herd-gc which doesn't exist in test env
	_, err := Reap(ctx, dir, "parked/to-reap", ReapDryRun)
	if err == nil {
		t.Fatal("expected error (bin/herd-gc not available in test env)")
	}
	if err != ErrGCNotFound {
		t.Fatalf("expected ErrGCNotFound, got: %v", err)
	}
}

func TestReap_GCNotFound(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	writeCommit(t, dir, "a", "a", "a")

	_, err := Reap(ctx, dir, "parked/nonexistent", ReapDryRun)
	if err == nil {
		t.Fatal("expected error for nonexistent tag")
	}
	if err != ErrGCNotFound {
		t.Fatalf("expected ErrGCNotFound, got: %v", err)
	}
}
