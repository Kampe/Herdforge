package stash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// PushOptions controls a push. Empty Message defaults to "WIP on <branch>".
// Empty ScopedPaths means plain (whole-worktree) save via `git stash create`.
type PushOptions struct {
	Message     string
	ScopedPaths []string
	// Stderr receives the NOT-saving notice for paths absent from HEAD.
	// nil → os.Stderr.
	Stderr io.Writer
}

// pushArgRefusal is the fail-closed copy for leftover / unknown push args.
// Push REVERTS what it saves; a dropped argument would silently sweep files
// the caller never named (2026-07-24: api-crusher lost 1 of 4 edited files).
const pushArgRefusal = `push takes -m <msg> and -- <paths>; got '%s'.
Refusing rather than ignoring it: push REVERTS what it saves, have a dropped
argument sweeps files you did not name.
-u/--include-untracked is not implemented: untracked files are always left in
place, exactly like git stash's own default.`

// ParsePushArgs matches the zsh port's strict push grammar:
//
//	push [-m <msg>] [-- <paths>...]
//
// Empty -m value, bare path args, -u/--include-untracked, and `--` with zero
// paths are all hard errors — never silently ignored.
func ParsePushArgs(args []string) (msg string, paths []string, err error) {
	msgSet := false
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-m":
			if i+1 >= len(args) {
				return "", nil, errors.New("-m needs a message")
			}
			i++
			if args[i] == "" {
				return "", nil, errors.New("-m needs a message")
			}
			msg = args[i]
			msgSet = true
			i++
		case a == "--":
			paths = append(paths, args[i+1:]...)
			if len(paths) == 0 {
				return "", nil, errors.New("-- needs at least one path")
			}
			return msg, paths, nil
		case a == "-u" || a == "--include-untracked":
			return "", nil, fmt.Errorf(pushArgRefusal, a)
		case strings.HasPrefix(a, "-"):
			return "", nil, fmt.Errorf(pushArgRefusal, a)
		default:
			// Bare path without -- : refuse. Never treat as implicit pathspec.
			return "", nil, fmt.Errorf(pushArgRefusal, strings.Join(args[i:], " "))
		}
	}
	_ = msgSet
	return msg, nil, nil
}

// Push saves this worktree's tracked changes under the private namespace and
// reverts the worktree (or only ScopedPaths) to HEAD.
//
// Returns the full ref written. ErrNoChanges when there is nothing to save
// (exit-0 at the CLI). On revert failure the ref is KEPT and the error names
// the recover path (`herd stash apply`).
func (r Repo) Push(msg string) (string, error) {
	return r.PushOpts(context.Background(), PushOptions{Message: msg})
}

// PushOpts is the full push entrypoint (plain or scoped).
func (r Repo) PushOpts(ctx context.Context, opts PushOptions) (string, error) {
	msg := opts.Message
	if msg == "" {
		branch := r.BranchContext(ctx)
		if branch == "" {
			branch = "detached"
		}
		msg = "WIP on " + branch
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	var sha string
	var err error
	if len(opts.ScopedPaths) == 0 {
		sha, err = r.plainStashCreate(ctx, msg)
	} else {
		sha, err = r.scopedStashCreate(ctx, msg, opts.ScopedPaths, stderr)
	}
	if err != nil {
		return "", err
	}
	if sha == "" {
		return "", ErrNoChanges
	}

	ref, err := r.nextRef(ctx)
	if err != nil {
		return "", err
	}
	// update-ref is the write; only if it STORED do we then revert.
	if _, err := r.git(ctx, "update-ref", ref, sha); err != nil {
		return "", fmt.Errorf("could not write %s (WIP NOT reverted, still in worktree): %w", ref, err)
	}
	if _, err := r.git(ctx, "rev-parse", "--verify", "--quiet", ref); err != nil {
		return "", fmt.Errorf("ref %s did not store; NOT reverting worktree", ref)
	}

	if len(opts.ScopedPaths) > 0 {
		// Revert only the named paths that were in HEAD (scoped save path).
		inHead, _ := r.splitInHead(ctx, opts.ScopedPaths)
		if err := r.revertWorktree(ctx, inHead); err != nil {
			// Ref is KEPT — the only copy of the work lives there now.
			return ref, fmt.Errorf("stored %s but could not revert %v; recover with: herd stash apply: %w", ref, inHead, err)
		}
		fmt.Fprintf(stderr, "herd-stash: saved %s %q scoped to: %s; only those paths reverted to HEAD\n",
			ref, msg, strings.Join(inHead, " "))
	} else {
		if err := r.revertWorktree(ctx, nil); err != nil {
			return ref, fmt.Errorf("stored %s but could not revert; recover with: herd stash apply: %w", ref, err)
		}
		fmt.Fprintf(stderr, "herd-stash: saved %s %q; worktree reverted to HEAD (untracked left in place)\n", ref, msg)
	}
	return ref, nil
}

// revertHook, when non-nil, replaces the real worktree revert (tests only).
var revertHook func(ctx context.Context, r Repo, scopedPaths []string) error

// revertWorktree restores HEAD for scoped paths, or reset --hard when scoped is nil.
func (r Repo) revertWorktree(ctx context.Context, scopedPaths []string) error {
	if revertHook != nil {
		return revertHook(ctx, r, scopedPaths)
	}
	if scopedPaths != nil {
		if len(scopedPaths) == 0 {
			return nil
		}
		args := append([]string{"checkout", "HEAD", "--"}, scopedPaths...)
		_, err := r.git(ctx, args...)
		return err
	}
	_, err := r.git(ctx, "reset", "--hard", "HEAD")
	return err
}

// plainStashCreate uses `git stash create`, which builds a stash-shaped commit
// WITHOUT touching refs/stash — that is what makes this safe across lanes.
func (r Repo) plainStashCreate(ctx context.Context, msg string) (string, error) {
	// stash create prints nothing (and exits 0) when there are no changes.
	// Fail closed on non-zero exit: empty output with err is a real failure
	// (cancel, exec, signal), not "nothing to save".
	cmd := execCommandContext(ctx, "git", "stash", "create", msg)
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("git stash create: %w", err)
		}
		return "", fmt.Errorf("git stash create: %w: %s", err, text)
	}
	return text, nil
}

// scopedStashCreate builds a stash-shaped commit through a TEMP index so the
// real index is never touched. Only paths present in HEAD are saved; paths
// absent from HEAD (new/untracked) are left exactly as they are.
func (r Repo) scopedStashCreate(ctx context.Context, msg string, paths []string, stderr io.Writer) (string, error) {
	headSHA, err := r.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("herd-stash: HEAD must exist: %w", err)
	}
	inHead, notInHead := r.splitInHead(ctx, paths)
	if len(notInHead) > 0 {
		fmt.Fprintf(stderr, "herd-stash: NOT saving %d path(s) absent from HEAD (new/untracked): %s\n",
			len(notInHead), strings.Join(notInHead, " "))
		fmt.Fprintln(stderr, "  Left exactly as they are, the git stash default...")
	}
	if len(inHead) == 0 {
		fmt.Fprintln(stderr, "herd-stash: nothing to save: every named path is absent from HEAD")
		return "", ErrNoChanges
	}

	tmp, err := os.CreateTemp("", "herd-stash-index-*")
	if err != nil {
		return "", fmt.Errorf("herd-stash: temp index: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath) // always, even on mid-step failure

	env := []string{"GIT_INDEX_FILE=" + tmpPath}
	if _, err := r.gitEnv(ctx, env, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	addArgs := append([]string{"add", "-u", "--"}, inHead...)
	if _, err := r.gitEnv(ctx, env, addArgs...); err != nil {
		return "", err
	}
	wTree, err := r.gitEnv(ctx, env, "write-tree")
	if err != nil {
		return "", err
	}
	headTree, err := r.git(ctx, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", err
	}
	if wTree == headTree {
		return "", fmt.Errorf("%w in: %s", ErrNoChanges, strings.Join(inHead, " "))
	}

	// Index-state commit: parent-tree == HEAD^{tree} so replay comes out unstaged
	// (exactly what red-then-green wants). Parent1 of stash = base, parent2 = index.
	branch := r.BranchContext(ctx)
	if branch == "" {
		branch = "detached"
	}
	iCommit, err := r.git(ctx, "commit-tree", headTree, "-p", headSHA, "-m", "index on "+branch)
	if err != nil {
		return "", err
	}
	sha, err := r.git(ctx, "commit-tree", wTree, "-p", headSHA, "-p", iCommit, "-m", msg)
	if err != nil {
		return "", err
	}
	return sha, nil
}

// splitInHead partitions paths into those present in HEAD vs not.
func (r Repo) splitInHead(ctx context.Context, paths []string) (inHead, notInHead []string) {
	for _, p := range paths {
		out, err := r.git(ctx, "ls-tree", "--name-only", "HEAD", "--", p)
		if err == nil && strings.TrimSpace(out) != "" {
			inHead = append(inHead, p)
		} else {
			notInHead = append(notInHead, p)
		}
	}
	return inHead, notInHead
}
