package stash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePushArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		args    []string
		wantMsg string
		wantP   []string
		errSub  string
	}{
		{name: "plain-empty", args: nil},
		{name: "push-msg", args: []string{"-m", "wip tests+impl"}, wantMsg: "wip tests+impl"},
		{name: "scoped", args: []string{"--", "src/impl.go"}, wantP: []string{"src/impl.go"}},
		{name: "msg-and-scoped", args: []string{"-m", "x", "--", "a.go", "b.go"}, wantMsg: "x", wantP: []string{"a.go", "b.go"}},
		{name: "empty-m", args: []string{"-m"}, errSub: "-m needs a message"},
		{name: "empty-m-value", args: []string{"-m", ""}, errSub: "-m needs a message"},
		{name: "empty-pathset", args: []string{"--"}, errSub: "-- needs at least one path"},
		{name: "refused-u", args: []string{"-u"}, errSub: "Refusing rather than ignoring"},
		{name: "refused-include-untracked", args: []string{"--include-untracked"}, errSub: "not implemented"},
		{name: "refused-extra-bare", args: []string{"a.go"}, errSub: "Refusing rather than ignoring"},
		{name: "refused-after-m", args: []string{"-m", "x", "extra"}, errSub: "Refusing rather than ignoring"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, paths, err := ParsePushArgs(tc.args)
			if tc.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("err=%v want substring %q", err, tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if msg != tc.wantMsg {
				t.Fatalf("msg=%q want %q", msg, tc.wantMsg)
			}
			if strings.Join(paths, ",") != strings.Join(tc.wantP, ",") {
				t.Fatalf("paths=%v want %v", paths, tc.wantP)
			}
		})
	}
}

func TestPushScopedRevertsOnlyNamedPaths(t *testing.T) {
	r, dir := newRepo(t)
	// Two tracked files; edit both; scope only one.
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("other-base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "other.go")
	run(t, dir, "commit", "-q", "-m", "other")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("impl-dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("other-dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ref, err := r.PushOpts(context.Background(), PushOptions{
		Message:     "scoped",
		ScopedPaths: []string{"f.txt"},
		Stderr:      &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref == "" {
		t.Fatal("expected a ref")
	}
	// f.txt reverted; other.go still dirty.
	body, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(body) != "base\n" {
		t.Fatalf("scoped path must revert, got %q", body)
	}
	other, _ := os.ReadFile(filepath.Join(dir, "other.go"))
	if string(other) != "other-dirty\n" {
		t.Fatalf("unscoped path must stay dirty, got %q", other)
	}
	// Shared stack still empty.
	if shared := run(t, dir, "stash", "list"); shared != "" {
		t.Fatalf("shared stack must stay empty, got %q", shared)
	}
}

func TestPushScopedNotInHeadLeftInPlace(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("tracked-dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "brand-new.go"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ref, err := r.PushOpts(context.Background(), PushOptions{
		Message:     "split",
		ScopedPaths: []string{"f.txt", "brand-new.go"},
		Stderr:      &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref == "" {
		t.Fatal("expected a ref for the in-HEAD path")
	}
	stderr := buf.String()
	if !strings.Contains(stderr, "NOT saving") || !strings.Contains(stderr, "brand-new.go") {
		t.Fatalf("expected NOT-saving note for brand-new.go, got %q", stderr)
	}
	// tracked path reverted
	body, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(body) != "base\n" {
		t.Fatalf("in-HEAD path must revert, got %q", body)
	}
	// untracked stays
	newBody, err := os.ReadFile(filepath.Join(dir, "brand-new.go"))
	if err != nil {
		t.Fatalf("untracked path must remain: %v", err)
	}
	if string(newBody) != "new\n" {
		t.Fatalf("untracked content changed: %q", newBody)
	}
}

func TestPushScopedAllNotInHead(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "ghost.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, err := r.PushOpts(context.Background(), PushOptions{
		ScopedPaths: []string{"ghost.go"},
		Stderr:      &buf,
	})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("want ErrNoChanges, got %v", err)
	}
	if !strings.Contains(buf.String(), "every named path is absent from HEAD") {
		t.Fatalf("want absent-from-HEAD notice, got %q", buf.String())
	}
	if refs, _ := r.Entries(); len(refs) != 0 {
		t.Fatalf("must not create ref, got %v", refs)
	}
}

func TestPushScopedNoTrackedChanges(t *testing.T) {
	r, dir := newRepo(t)
	// Path in HEAD but clean — nothing to save.
	var buf bytes.Buffer
	_, err := r.PushOpts(context.Background(), PushOptions{
		ScopedPaths: []string{"f.txt"},
		Stderr:      &buf,
	})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("want ErrNoChanges, got %v", err)
	}
	if refs, _ := r.Entries(); len(refs) != 0 {
		t.Fatalf("must not create ref, got %v", refs)
	}
	_ = dir
}

func TestPushMsgExact(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	// Plain path uses `git stash create`, which rewrites the subject to
	// "On <branch>: <msg>" — assert the caller message is preserved in that form.
	ref, err := r.PushOpts(context.Background(), PushOptions{
		Message: "wip tests+impl",
		Stderr:  &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	sub := run(t, dir, "log", "-1", "--format=%s", ref)
	if !strings.Contains(sub, "wip tests+impl") {
		t.Fatalf("subject=%q must carry caller message", sub)
	}
	// Scoped path uses commit-tree with the raw message — exact subject.
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("dirty2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref2, err := r.PushOpts(context.Background(), PushOptions{
		Message:     "wip tests+impl",
		ScopedPaths: []string{"f.txt"},
		Stderr:      &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	sub2 := run(t, dir, "log", "-1", "--format=%s", ref2)
	if sub2 != "wip tests+impl" {
		t.Fatalf("scoped subject=%q want exact message", sub2)
	}
}

func TestPushDefaultMessage(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ref, err := r.PushOpts(context.Background(), PushOptions{Stderr: &buf})
	if err != nil {
		t.Fatal(err)
	}
	sub := run(t, dir, "log", "-1", "--format=%s", ref)
	// git stash create wraps as "On feature: WIP on feature".
	if !strings.Contains(sub, "WIP on feature") {
		t.Fatalf("default subject=%q must contain WIP on feature", sub)
	}
}

func TestPushRevertFailureKeepsRef(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Inject a failing revert AFTER the ref has been stored. The entry must
	// remain so the caller can recover with herd stash apply.
	revertHook = func(ctx context.Context, r Repo, scoped []string) error {
		return errors.New("simulated revert failure")
	}
	t.Cleanup(func() { revertHook = nil })

	var buf bytes.Buffer
	ref, err := r.PushOpts(context.Background(), PushOptions{Message: "keep", Stderr: &buf})
	if err == nil {
		t.Fatal("expected revert failure")
	}
	if !strings.Contains(err.Error(), "recover with: herd stash apply") {
		t.Fatalf("error must name recover path, got %v", err)
	}
	if strings.Contains(err.Error(), "bin/herd-stash") {
		t.Fatalf("must not name non-existent bin/herd-stash binary: %v", err)
	}
	if ref == "" {
		t.Fatal("ref must be returned even on revert failure")
	}
	// Ref KEPT — resolvable and listed.
	if sha := run(t, dir, "rev-parse", ref); sha == "" {
		t.Fatal("ref must still resolve")
	}
	refs, _ := r.Entries()
	if len(refs) != 1 || refs[0] != ref {
		t.Fatalf("entry must be kept, got %v", refs)
	}
	// Worktree still dirty (revert did not run).
	body, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(body) != "dirty\n" {
		t.Fatalf("worktree must still hold WIP after failed revert, got %q", body)
	}
}

// TestPlainStashCreateFailsClosedOnGitError proves a non-zero `git stash create`
// with empty output is a hard error — never remapped to ErrNoChanges / exit 0.
// The previous swallow treated empty+err as "nothing to save" while the worktree
// stayed dirty (fail-open against CLAUDE.md hard rule #2).
func TestPlainStashCreateFailsClosedOnGitError(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("still-dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := execCommandContext
	t.Cleanup(func() { execCommandContext = orig })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Simulate cancel/exec failure: non-zero exit, empty stdout/stderr.
		return exec.CommandContext(ctx, "sh", "-c", "exit 1")
	}

	ref, err := r.PushOpts(context.Background(), PushOptions{Message: "must-fail"})
	if err == nil {
		t.Fatal("git stash create failure must not succeed")
	}
	if errors.Is(err, ErrNoChanges) {
		t.Fatalf("must not remap git failure to ErrNoChanges (exit 0 path), got %v", err)
	}
	if !strings.Contains(err.Error(), "git stash create") {
		t.Fatalf("error must name the failing step, got %v", err)
	}
	if ref != "" {
		t.Fatalf("no ref on create failure, got %q", ref)
	}
	if refs, _ := r.Entries(); len(refs) != 0 {
		t.Fatalf("must not write a ref after create failure, got %v", refs)
	}
	// Worktree still dirty — nothing was saved or reverted.
	body, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(body) != "still-dirty\n" {
		t.Fatalf("worktree must stay dirty after create failure, got %q", body)
	}
}

func TestCounterMonotonicPerWorktree(t *testing.T) {
	r, dir := newRepo(t)
	var buf bytes.Buffer
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Repeat("z", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		ref, err := r.PushOpts(context.Background(), PushOptions{Message: "n", Stderr: &buf})
		if err != nil {
			t.Fatal(err)
		}
		wantSuffix := fmt.Sprintf("/%d", i)
		if !strings.HasSuffix(ref, wantSuffix) {
			t.Fatalf("after push %d: ref %q must end with %s", i, ref, wantSuffix)
		}
		_, n, err := r.PeekNewest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n != i {
			t.Fatalf("after push %d: PeekNewest n=%d want %d", i, n, i)
		}
	}
	_, n, err := r.PeekNewest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("newest n=%d want 2", n)
	}
	// All three refs exist under private ns only.
	listed := run(t, dir, "for-each-ref", "--format=%(refname)", NamespaceRoot)
	lines := strings.Split(listed, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 private refs, got %q", listed)
	}
	if shared := run(t, dir, "stash", "list"); shared != "" {
		t.Fatalf("shared stack must stay empty")
	}
}
