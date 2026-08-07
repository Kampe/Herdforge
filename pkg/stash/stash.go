// Package stash ports bin/herd-stash: a worktree-scoped stash.
//
// Bare `git stash` keeps ONE shared stack (refs/stash) across every worktree of
// a repository, so a `git stash pop` in one lane can grab another lane's entry.
// That hit twice on 2026-07-24, silently swapping WIP between lanes. Herdforge
// runs far more concurrent worktrees, so the hazard is strictly worse here.
//
// Each worktree's stashes are stored as commits under a per-lane ref namespace,
// refs/herd-stash/<worktree-basename>/<n>, never touching the shared stack.
// Two lanes physically cannot collide because each writes only its own prefix.
//
// Mirrors `git stash` DEFAULT: tracked and staged changes only. Untracked
// files are left in place. -u/--include-untracked is refused, never ignored.
package stash

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// NamespaceRoot is the ref prefix that keeps lanes apart.
const NamespaceRoot = "refs/herd-stash"

// Sentinel errors. CLI maps ErrNoChanges to exit 0 (nothing to save is fine).
var (
	ErrNoChanges = errors.New("no local (tracked) changes to save")
	ErrNoEntries = errors.New("no herd-stash entries for worktree")
)

var unsafeRefChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// sanitizeBase turns a worktree basename into a ref-name-safe id.
// Everything outside [A-Za-z0-9._-] becomes '_'. Empty input stays empty.
func sanitizeBase(raw string) string {
	return unsafeRefChars.ReplaceAllString(raw, "_")
}

// Repo runs git in one worktree. Dir is the worktree path (git -C).
type Repo struct{ Dir string }

func (r Repo) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// gitEnv runs git with extra env vars (used for GIT_INDEX_FILE temp index).
// env overlays os.Environ so GIT_INDEX_FILE wins over any inherited value.
func (r Repo) gitEnv(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(environ(), env...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// WorktreeID is the ref-name-safe identity of this worktree.
// Same basename → same namespace (parallel checkouts of identically-named
// directories intentionally collide; distinct basenames get distinct prefixes).
func (r Repo) WorktreeID() (string, error) {
	return r.WorktreeIDContext(context.Background())
}

// WorktreeIDContext is the context-aware form of WorktreeID.
func (r Repo) WorktreeIDContext(ctx context.Context) (string, error) {
	top, err := r.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return sanitizeBase(filepath.Base(top)), nil
}

// Namespace is this worktree's private ref namespace.
func (r Repo) Namespace() (string, error) {
	return r.NamespaceContext(context.Background())
}

// NamespaceContext is the context-aware form of Namespace.
func (r Repo) NamespaceContext(ctx context.Context) (string, error) {
	id, err := r.WorktreeIDContext(ctx)
	if err != nil {
		return "", err
	}
	return NamespaceRoot + "/" + id, nil
}

// Branch reports the current branch, or "" when detached.
func (r Repo) Branch() string {
	return r.BranchContext(context.Background())
}

// BranchContext is the context-aware form of Branch.
func (r Repo) BranchContext(ctx context.Context) string {
	b, err := r.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || b == "HEAD" {
		return ""
	}
	return b
}

// entryNum extracts the trailing <n> of a namespaced ref.
func entryNum(ref string) (int, bool) {
	i := strings.LastIndex(ref, "/")
	if i < 0 || i+1 >= len(ref) {
		return 0, false
	}
	n, err := strconv.Atoi(ref[i+1:])
	return n, err == nil
}

// Entries lists this worktree's stash refs, oldest first. Sorted NUMERICALLY:
// lexical order would put /10 before /9 and pop the wrong entry.
func (r Repo) Entries() ([]string, error) {
	return r.EntriesContext(context.Background())
}

// EntriesContext is the context-aware form of Entries.
func (r Repo) EntriesContext(ctx context.Context) ([]string, error) {
	ns, err := r.NamespaceContext(ctx)
	if err != nil {
		return nil, err
	}
	out, err := r.git(ctx, "for-each-ref", "--format=%(refname)", ns)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			refs = append(refs, line)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		a, aok := entryNum(refs[i])
		b, bok := entryNum(refs[j])
		if aok && bok {
			return a < b
		}
		return refs[i] < refs[j]
	})
	return refs, nil
}

// Newest returns the most recent entry (highest <n>).
func (r Repo) Newest() (string, error) {
	return r.NewestContext(context.Background())
}

// NewestContext is the context-aware form of Newest.
func (r Repo) NewestContext(ctx context.Context) (string, error) {
	refs, err := r.EntriesContext(ctx)
	if err != nil {
		return "", err
	}
	if len(refs) == 0 {
		id, _ := r.WorktreeIDContext(ctx)
		return "", fmt.Errorf("%w %q", ErrNoEntries, id)
	}
	return refs[len(refs)-1], nil
}

// PeekNewest returns the newest ref and its numeric index.
func (r Repo) PeekNewest(ctx context.Context) (ref string, n int, err error) {
	ref, err = r.NewestContext(ctx)
	if err != nil {
		return "", 0, err
	}
	n, ok := entryNum(ref)
	if !ok {
		return ref, 0, fmt.Errorf("herd-stash: malformed ref %q", ref)
	}
	return ref, n, nil
}

// nextRef is the next free slot in this worktree's namespace.
func (r Repo) nextRef(ctx context.Context) (string, error) {
	ns, err := r.NamespaceContext(ctx)
	if err != nil {
		return "", err
	}
	refs, err := r.EntriesContext(ctx)
	if err != nil {
		return "", err
	}
	n := 0
	if len(refs) > 0 {
		if last, ok := entryNum(refs[len(refs)-1]); ok {
			n = last + 1
		} else {
			n = len(refs)
		}
	}
	return fmt.Sprintf("%s/%d", ns, n), nil
}

// ListEntry is one row of `herd stash list` (newest first).
type ListEntry struct {
	Ref     string // full ref
	Short   string // ref without NamespaceRoot/
	Summary string // `git log -1 --format=%h %cr %s`
}

// List returns this worktree's entries newest-first with short summaries.
func (r Repo) List(ctx context.Context) ([]ListEntry, error) {
	refs, err := r.EntriesContext(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ListEntry, 0, len(refs))
	for i := len(refs) - 1; i >= 0; i-- {
		ref := refs[i]
		sum, err := r.git(ctx, "log", "-1", "--format=%h %cr %s", ref)
		if err != nil {
			sum = ""
		}
		out = append(out, ListEntry{
			Ref:     ref,
			Short:   strings.TrimPrefix(ref, NamespaceRoot+"/"),
			Summary: sum,
		})
	}
	return out, nil
}
