package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// resolveReviewTaskRef resolves the identity a verdict artifact carries.
// The command selector may be a branch (for example herd/fac-755), but the
// artifact must name the provider's exact closeable card ref. Board lookup is
// scoped to the configured project; missing, ambiguous, cross-project, and
// non-closeable identities refuse.
func resolveReviewTaskRef(ctx context.Context, tasks provider.TaskProvider, projectID, selector string) (*provider.Task, error) {
	projectID = strings.TrimSpace(projectID)
	selector = strings.TrimSpace(selector)
	if projectID == "" {
		return nil, fmt.Errorf("review task identity: project context is required")
	}
	if selector == "" {
		return nil, fmt.Errorf("review task identity: candidate ref or branch is required")
	}

	want := reviewTaskSelectorRef(selector)
	if want == "" {
		return nil, fmt.Errorf("review task identity: %q is neither a closeable card ref nor a branch carrying one", selector)
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
	if err != nil || task == nil {
		if raw := rawCandidateCardRef(selector); raw != "" && !strings.EqualFold(raw, want) {
			if t2, err2 := tasks.GetTask(ctx, raw); err2 == nil && t2 != nil {
				task, err = t2, nil
			}
		}
	}
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

func reviewTaskSelectorRef(selector string) string {
	if ref := reviewledger.CloseableCardRef(selector); ref != "" {
		return ref
	}
	// A branch is only a selector. It is never returned as packet identity.
	return drainCandidateRef(selector)
}

func rawCandidateCardRef(selector string) string {
	if ref := reviewledger.CloseableCardRef(selector); ref != "" {
		return ref
	}
	m := drainRefToken.FindStringSubmatch(strings.TrimSpace(selector))
	if m == nil {
		return ""
	}
	return reviewledger.CloseableCardRef(strings.ToUpper(m[1]) + "-" + m[2])
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
