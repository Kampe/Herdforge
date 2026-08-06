package scope

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Resolver computes an AdmissionScope against a real git working copy.
type Resolver struct {
	RepoRoot string
}

func NewResolver(repoRoot string) (*Resolver, error) {
	if strings.TrimSpace(repoRoot) == "" {
		return nil, errors.New("scope: repo root is required")
	}
	return &Resolver{RepoRoot: repoRoot}, nil
}

// Resolve fetches the target branch's current remote tip (never trusting a
// stale local ref), resolves the merge base against the candidate, and
// captures the ordered commit chain, changed-path set, and range diff digest
// between them. candidateRef is optional; when set, its tip must resolve to
// candidateSHA or Resolve fails closed with ErrForcePushed.
func (r *Resolver) Resolve(ctx context.Context, repository, targetBranch, candidateRef, candidateSHA string) (*AdmissionScope, error) {
	if err := requireSHA(candidateSHA); err != nil {
		return nil, err
	}
	if strings.TrimSpace(targetBranch) == "" {
		return nil, errors.New("scope: target branch is required")
	}

	targetSHA, err := r.resolveTargetSHA(ctx, targetBranch)
	if err != nil {
		return nil, err
	}
	if err := r.requireCommit(ctx, candidateSHA); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCandidateMissing, candidateSHA, err)
	}
	if candidateRef != "" {
		if err := r.requireRefAt(ctx, candidateRef, candidateSHA); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrForcePushed, err)
		}
	}
	candidateTree, err := r.run(ctx, "rev-parse", "--verify", candidateSHA+"^{tree}")
	if err != nil {
		return nil, fmt.Errorf("scope: resolve candidate tree: %w", err)
	}
	mergeBase, err := r.run(ctx, "merge-base", targetSHA, candidateSHA)
	if err != nil {
		return nil, fmt.Errorf("scope: resolve merge base: %w", err)
	}
	commits, err := r.revList(ctx, mergeBase, candidateSHA)
	if err != nil {
		return nil, err
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("scope: no commits between merge base %s and candidate %s", mergeBase, candidateSHA)
	}
	paths, err := r.changedPaths(ctx, mergeBase, candidateSHA)
	if err != nil {
		return nil, err
	}
	diffDigest, err := r.diffDigest(ctx, mergeBase, candidateSHA)
	if err != nil {
		return nil, err
	}

	out := AdmissionScope{
		Version:       Version1,
		Repository:    repository,
		TargetBranch:  targetBranch,
		TargetSHA:     targetSHA,
		CandidateRef:  candidateRef,
		CandidateSHA:  candidateSHA,
		CandidateTree: candidateTree,
		MergeBase:     mergeBase,
		Commits:       commits,
		ChangedPaths:  paths,
		DiffDigest:    diffDigest,
	}
	out.Digest = computeDigest(out)
	return &out, nil
}

// VerifyCurrent recomputes recorded's scope fresh against the live
// repository and fails closed on the first invariant that no longer holds:
// target-branch advance, merge-base change, commit-set change (added,
// reordered, or squashed commits), force-push (candidate ref moved off the
// recorded sha), changed-path drift, or a diff-digest mismatch. recorded
// must self-validate first, so a tampered or hand-built scope is rejected
// before it is ever trusted as a baseline.
func (r *Resolver) VerifyCurrent(ctx context.Context, recorded AdmissionScope) error {
	if err := recorded.SelfValidate(); err != nil {
		return err
	}
	fresh, err := r.Resolve(ctx, recorded.Repository, recorded.TargetBranch, recorded.CandidateRef, recorded.CandidateSHA)
	if err != nil {
		return err
	}
	switch {
	case fresh.TargetSHA != recorded.TargetSHA:
		return fmt.Errorf("%w: %s -> %s", ErrTargetAdvanced, recorded.TargetSHA, fresh.TargetSHA)
	case fresh.MergeBase != recorded.MergeBase:
		return fmt.Errorf("%w: %s -> %s", ErrMergeBaseChanged, recorded.MergeBase, fresh.MergeBase)
	case !equalSlices(fresh.Commits, recorded.Commits):
		return fmt.Errorf("%w: recorded %d commits, recomputed %d", ErrCommitSetChanged, len(recorded.Commits), len(fresh.Commits))
	case !equalSlices(fresh.ChangedPaths, recorded.ChangedPaths):
		return fmt.Errorf("%w: recorded %d paths, recomputed %d", ErrPathSetChanged, len(recorded.ChangedPaths), len(fresh.ChangedPaths))
	case fresh.DiffDigest != recorded.DiffDigest:
		return fmt.Errorf("%w: range diff digest drifted", ErrScopeMismatch)
	}
	return nil
}

func requireSHA(sha string) error {
	if len(sha) != 40 {
		return fmt.Errorf("scope: candidate sha must be a full 40-character commit sha, got %q", sha)
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return fmt.Errorf("scope: candidate sha must be hexadecimal: %w", err)
	}
	return nil
}

func (r *Resolver) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Resolver) runBytes(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.RepoRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// resolveTargetSHA fetches origin/<targetBranch> and returns its tip. It
// never trusts the local checkout's idea of the branch, so a stale or
// force-pushed local mirror cannot poison the scope (mirrors
// worktree.ResolveImmutableBase). Fetch is tolerated to fail offline as long
// as origin/<targetBranch> already exists locally.
func (r *Resolver) resolveTargetSHA(ctx context.Context, targetBranch string) (string, error) {
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", "--quiet", "origin", targetBranch)
	fetchCmd.Dir = r.RepoRoot
	fetchOut, fetchErr := fetchCmd.CombinedOutput()

	ref := "origin/" + targetBranch
	sha, err := r.run(ctx, "rev-parse", "--verify", ref)
	if err != nil {
		if fetchErr != nil {
			return "", fmt.Errorf("scope: resolve target %s after fetch error: fetch=%v (%s); %w",
				ref, fetchErr, strings.TrimSpace(string(fetchOut)), err)
		}
		return "", fmt.Errorf("scope: resolve target %s: %w", ref, err)
	}
	return sha, nil
}

func (r *Resolver) requireCommit(ctx context.Context, sha string) error {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", sha+"^{commit}")
	cmd.Dir = r.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w (%s)", sha, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *Resolver) requireRefAt(ctx context.Context, ref, sha string) error {
	got, err := r.run(ctx, "rev-parse", "--verify", ref)
	if err != nil {
		return fmt.Errorf("resolve ref %s: %w", ref, err)
	}
	if got != sha {
		return fmt.Errorf("ref %s resolves to %s, expected %s", ref, got, sha)
	}
	return nil
}

// revList returns the ordered commit chain strictly after mergeBase, up to
// and including candidateSHA, oldest first.
func (r *Resolver) revList(ctx context.Context, mergeBase, candidateSHA string) ([]string, error) {
	out, err := r.run(ctx, "rev-list", "--reverse", mergeBase+".."+candidateSHA)
	if err != nil {
		return nil, fmt.Errorf("scope: list commits %s..%s: %w", mergeBase, candidateSHA, err)
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

func (r *Resolver) changedPaths(ctx context.Context, mergeBase, candidateSHA string) ([]string, error) {
	out, err := r.run(ctx, "diff", "--name-only", mergeBase+".."+candidateSHA)
	if err != nil {
		return nil, fmt.Errorf("scope: changed paths %s..%s: %w", mergeBase, candidateSHA, err)
	}
	if out == "" {
		return []string{}, nil
	}
	paths := strings.Split(out, "\n")
	sort.Strings(paths)
	return paths, nil
}

func (r *Resolver) diffDigest(ctx context.Context, mergeBase, candidateSHA string) (string, error) {
	data, err := r.runBytes(ctx, "diff", mergeBase+".."+candidateSHA)
	if err != nil {
		return "", fmt.Errorf("scope: diff digest %s..%s: %w", mergeBase, candidateSHA, err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
