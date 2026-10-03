package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// resolveReviewTaskRef resolves the identity a verdict artifact carries.
// An exact PREFIX-NUMBER selector is the card. Any other selector (including
// exclusive worktree branches such as fix/fac607-diagnostics-3342) is identity
// from a verified TASK-CONTEXT on that branch, never a guessed token from the
// name. Board lookup is scoped to the configured project; missing, ambiguous,
// cross-project, and non-closeable identities refuse.
func resolveReviewTaskRef(ctx context.Context, tasks provider.TaskProvider, projectID, selector, repoRoot, sha string) (*provider.Task, error) {
	projectID = strings.TrimSpace(projectID)
	selector = strings.TrimSpace(selector)
	if projectID == "" {
		return nil, fmt.Errorf("review task identity: project context is required")
	}
	if selector == "" {
		return nil, fmt.Errorf("review task identity: candidate ref or branch is required")
	}

	want, identErr := reviewTaskSelectorRef(selector, repoRoot, sha)
	if identErr != nil {
		return nil, identErr
	}
	if want == "" {
		return nil, fmt.Errorf("review task identity: %q is neither a closeable card ref nor a branch with authenticated task context", selector)
	}

	var matches []*provider.Task
	for _, status := range []string{provider.StatusInProgress, provider.StatusInReview} {
		listed, err := tasks.ListTasks(ctx, projectID, status)
		if err != nil {
			return nil, fmt.Errorf("review task identity: list %s tasks: %w", status, err)
		}
		for _, task := range listed {
			if task == nil || !sameCloseableCardRef(task.Ref, want) {
				continue
			}
			if strings.TrimSpace(task.ProjectID) != projectID {
				return nil, fmt.Errorf("review task identity: card %q belongs to project %q, not configured project %q", task.Ref, task.ProjectID, projectID)
			}
			matches = append(matches, task)
		}
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("review task identity: ambiguous provider cards for %s", want)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}

	// Some providers expose GetTask for cards that are not in the in-progress
	// or in-review listings. Still validate identity and project before the
	// packet may carry the ref.
	task, err := tasks.GetTask(ctx, want)
	if err != nil {
		return nil, fmt.Errorf("review task identity: card %s not found: %w", want, err)
	}
	if task == nil {
		return nil, fmt.Errorf("review task identity: card %s not found", want)
	}
	if strings.TrimSpace(task.ProjectID) != projectID {
		return nil, fmt.Errorf("review task identity: card %q belongs to project %q, not configured project %q", task.Ref, task.ProjectID, projectID)
	}
	if got := reviewledger.CloseableCardRef(task.Ref); got == "" {
		return nil, fmt.Errorf("review task identity: provider returned non-closeable ref %q", task.Ref)
	} else if !sameCloseableCardRef(got, want) {
		return nil, fmt.Errorf("review task identity: provider ref %q does not match requested %q", task.Ref, want)
	}
	return task, nil
}

func reviewTaskSelectorRef(selector, repoRoot, sha string) (string, error) {
	if ref := reviewledger.CloseableCardRef(selector); ref != "" {
		return ref, nil
	}
	// A branch is only a selector. Card identity comes from authenticated
	// context on that branch, never from the first PREFIX-NUMBER token in
	// the name (FAC-845: fix/fac607-diagnostics-3342 is FAC-607, not
	// DIAGNOSTICS-3342).
	return authenticatedBranchTaskRef(repoRoot, selector, sha)
}

func authenticatedBranchTaskRef(repoRoot, selector, sha string) (string, error) {
	selector = strings.TrimSpace(selector)
	repoRoot = strings.TrimSpace(repoRoot)
	sha = strings.TrimSpace(sha)
	if selector == "" {
		return "", fmt.Errorf("review task identity: candidate ref or branch is required")
	}
	if repoRoot == "" {
		return "", fmt.Errorf("review task identity: %q is not a closeable card ref and has no repository to authenticate a branch identity (refusing to guess a card from the name)", selector)
	}
	dir, err := checkedOutBranchWorktree(repoRoot, selector)
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", fmt.Errorf("review task identity: %q is not a closeable card ref and no worktree is checked out on that branch (refusing to guess a card from the name)", selector)
	}
	if sha != "" && !headMatchesSHA(dir, sha) {
		return "", fmt.Errorf("review task identity: branch %q HEAD does not match candidate %s (refusing to guess)", selector, shortSHA(sha))
	}
	tc, err := dispatch.ReadTaskContext(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("review task identity: branch %q has no TASK-CONTEXT (refusing to guess a card from the name)", selector)
		}
		return "", fmt.Errorf("review task identity: branch %q TASK-CONTEXT unreadable (refusing to guess): %w", selector, err)
	}
	verifier, err := dispatch.LoadVerifier(repoRoot)
	if err != nil {
		return "", fmt.Errorf("review task identity: cannot authenticate TASK-CONTEXT for %q: %w", selector, err)
	}
	if err := verifier.Verify(tc); err != nil {
		return "", fmt.Errorf("review task identity: TASK-CONTEXT for %q failed authentication (refusing to guess): %w", selector, err)
	}
	card := reviewledger.CloseableCardRef(tc.TaskRef)
	if card == "" {
		return "", fmt.Errorf("review task identity: authenticated context on %q has non-closeable task_ref %q", selector, tc.TaskRef)
	}
	if cand := strings.TrimSpace(tc.CandidateSHA); cand != "" && sha != "" && !identitySHAMatches(cand, sha) {
		return "", fmt.Errorf("review task identity: authenticated context on %q is for candidate %s, not %s", selector, shortSHA(cand), shortSHA(sha))
	}
	return card, nil
}

func identitySHAMatches(got, want string) bool {
	got, want = strings.TrimSpace(got), strings.TrimSpace(want)
	if got == "" || want == "" {
		return false
	}
	if strings.EqualFold(got, want) {
		return true
	}
	if len(got) < 12 || len(want) < 12 {
		return false
	}
	gl, wl := strings.ToLower(got), strings.ToLower(want)
	return strings.HasPrefix(gl, wl) || strings.HasPrefix(wl, gl)
}

func sameCloseableCardRef(a, b string) bool {
	cardA := reviewledger.CloseableCardRef(a)
	cardB := reviewledger.CloseableCardRef(b)
	if cardA == "" || cardB == "" {
		return false
	}
	return strings.EqualFold(hsync.NormalizeRef(cardA), hsync.NormalizeRef(cardB))
}

// reviewPacketTaskIdentity is the exact closeable card a packet may prefill.
// Provider refs are canonicalized; a branch or free-form label refuses.
func reviewPacketTaskIdentity(task *provider.Task) (string, error) {
	if task == nil {
		return "", fmt.Errorf("review task identity: provider returned no card")
	}
	card := reviewledger.CloseableCardRef(task.Ref)
	if card == "" {
		return "", fmt.Errorf("review task identity: provider returned non-closeable ref %q", task.Ref)
	}
	return card, nil
}

// reviewLaunchRecordBinding keeps the launched reviewer identity on the
// candidate selector and the ledger Task on the closeable card. Mixing them
// makes Admit look up SHA+Reviewer and miss the launch row.
func reviewLaunchRecordBinding(selector, packetTask, sha string) (reviewer, task string, err error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", "", fmt.Errorf("review launch identity: candidate selector is required")
	}
	task = reviewledger.CloseableCardRef(packetTask)
	if task == "" {
		return "", "", fmt.Errorf("review launch identity: task %q is not a closeable card ref", packetTask)
	}
	return reviewAgentName(selector, sha), task, nil
}
