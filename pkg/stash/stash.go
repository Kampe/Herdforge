// Package stash ports bin/herd-stash: a worktree-scoped stash.
//
// Bare `git stash` keeps ONE shared stack (refs/stash) across every worktree of
// a repository, so a `git stash pop` in one lane can grab another lane's entry.
// chainseer hit this twice on 2026-07-24, silently swapping WIP between lanes.
// Herdforge runs far more concurrent worktrees than chainseer, so the hazard is
// strictly worse here.
//
// Each worktree's stashes are stored as commits under a per-lane ref namespace,
// refs/herd-stash/<worktree>/<n>, never touching the shared stack. Two lanes
// physically cannot collide because each writes only its own namespace.
//
// Mirrors `git stash` DEFAULT: tracked and staged changes only. Untracked files
// are left in place, exactly like git's own behaviour without -u.
package stash

import (
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

var unsafeRefChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Repo runs git in one worktree.
type Repo struct{ Dir string }

func (r Repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// WorktreeID is the ref-name-safe identity of this worktree.
func (r Repo) WorktreeID() (string, error) {
	top, err := r.git("rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return unsafeRefChars.ReplaceAllString(filepath.Base(top), "_"), nil
}

// Namespace is this worktree's private ref namespace.
func (r Repo) Namespace() (string, error) {
	id, err := r.WorktreeID()
	if err != nil {
		return "", err
	}
	return NamespaceRoot + "/" + id, nil
}

// Branch reports the current branch, or "" when detached.
func (r Repo) Branch() string {
	b, err := r.git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || b == "HEAD" {
		return ""
	}
	return b
}

// entryNum extracts the trailing <n> of a namespaced ref.
func entryNum(ref string) (int, bool) {
	n, err := strconv.Atoi(ref[strings.LastIndex(ref, "/")+1:])
	return n, err == nil
}

// Entries lists this worktree's stash refs, oldest first. Sorted NUMERICALLY:
// lexical order would put /10 before /9 and pop the wrong entry.
func (r Repo) Entries() ([]string, error) {
	ns, err := r.Namespace()
	if err != nil {
		return nil, err
	}
	out, err := r.git("for-each-ref", "--format=%(refname)", ns)
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

// Newest returns the most recent entry.
func (r Repo) Newest() (string, error) {
	refs, err := r.Entries()
	if err != nil {
		return "", err
	}
	if len(refs) == 0 {
		id, _ := r.WorktreeID()
		return "", fmt.Errorf("herd-stash: no entries for worktree %q", id)
	}
	return refs[len(refs)-1], nil
}

// nextRef is the next free slot in this worktree's namespace.
func (r Repo) nextRef() (string, error) {
	ns, err := r.Namespace()
	if err != nil {
		return "", err
	}
	refs, err := r.Entries()
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

// SharedStackConflict reports entries on the SHARED stack made on this branch.
// Those are exactly the racy entries herd-stash replaces; ignoring them would
// leave the cross-lane hazard in place, so callers refuse rather than proceed.
func (r Repo) SharedStackConflict() []string {
	branch := r.Branch()
	if branch == "" {
		return nil
	}
	out, err := r.git("stash", "list")
	if err != nil {
		return nil
	}
	var hits []string
	// Case-insensitive: git writes "On <branch>:" for `stash push` and
	// "WIP on <branch>:" for a bare `git stash`. chainseer used grep -iF.
	needle := strings.ToLower("on " + branch + ":")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(strings.ToLower(line), needle) {
			hits = append(hits, strings.TrimSpace(line))
		}
	}
	return hits
}

// Push saves this worktree's tracked changes and reverts to HEAD.
// Returns the ref written, or "" when there was nothing to save.
func (r Repo) Push(msg string) (string, error) {
	if msg == "" {
		branch := r.Branch()
		if branch == "" {
			branch = "detached"
		}
		msg = "WIP on " + branch
	}
	// `git stash create` builds a stash-shaped commit WITHOUT touching
	// refs/stash — that is what makes this safe alongside other lanes.
	sha, err := r.git("stash", "create", msg)
	if err != nil {
		return "", err
	}
	if sha == "" {
		return "", nil
	}
	ref, err := r.nextRef()
	if err != nil {
		return "", err
	}
	if _, err := r.git("update-ref", ref, sha); err != nil {
		return "", fmt.Errorf("could not write %s (WIP NOT reverted, still in worktree): %w", ref, err)
	}
	// Read the ref back BEFORE destroying the working tree. If the ref did not
	// store, reverting would discard the only copy of the work.
	if _, err := r.git("rev-parse", "--verify", "--quiet", ref); err != nil {
		return "", fmt.Errorf("ref %s did not store; NOT reverting worktree", ref)
	}
	if _, err := r.git("reset", "--hard", "HEAD"); err != nil {
		return ref, fmt.Errorf("stored %s but could not revert; recover with: herd stash apply: %w", ref, err)
	}
	return ref, nil
}

// Apply replays the newest entry. When drop is true the entry is removed
// afterwards (pop). A conflicting apply always KEEPS the entry.
func (r Repo) Apply(drop bool) (string, error) {
	ref, err := r.Newest()
	if err != nil {
		return "", err
	}
	sha, err := r.git("rev-parse", ref)
	if err != nil {
		return "", err
	}
	if _, err := r.git("stash", "apply", sha); err != nil {
		return ref, fmt.Errorf("apply of %s hit a conflict; entry KEPT. Resolve, then re-run pop (or git update-ref -d %s)", ref, ref)
	}
	if drop {
		if _, err := r.git("update-ref", "-d", ref); err != nil {
			return ref, fmt.Errorf("applied %s but could not drop it: %w", ref, err)
		}
	}
	return ref, nil
}
