package herdr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/mail"
)

const (
	// StatusQueuedDurable is a successful busy-path receipt: the payload is
	// on the mailbox, and it has not been consumed by the pane.
	StatusQueuedDurable = "queued-durable"
	queueSenderDefault  = "herd-send"
)

// ErrNotIdleBoundary is returned when a drain is asked to surface mail while
// the recipient is still working, starting, or otherwise not at a safe turn.
var ErrNotIdleBoundary = errors.New("herdr drain: recipient is not at an idle/done turn boundary")

// SendResult is the native send receipt. EnvelopeID is set only when a
// routine payload was durably queued rather than submitted to the pane.
type SendResult struct {
	Status     string
	EnvelopeID string
}

var queueMailboxHook func() *mail.Mailbox

var queuedProveTimeout = 30 * time.Second

// SetQueuedProveTimeoutForTest shortens drain consumption polling. Restore
// with the returned func.
func SetQueuedProveTimeoutForTest(d time.Duration) func() {
	prev := queuedProveTimeout
	if d <= 0 {
		queuedProveTimeout = 30 * time.Second
	} else {
		queuedProveTimeout = d
	}
	return func() { queuedProveTimeout = prev }
}

// SetQueueMailbox injects the durable mailbox used by routine busy delivery.
// Restore with the returned func.
func SetQueueMailbox(box *mail.Mailbox) func() {
	prev := queueMailboxHook
	if box == nil {
		queueMailboxHook = nil
	} else {
		queueMailboxHook = func() *mail.Mailbox { return box }
	}
	return func() { queueMailboxHook = prev }
}

func resolveQueueMailbox() (*mail.Mailbox, error) {
	if queueMailboxHook != nil {
		return queueMailboxHook(), nil
	}
	path, err := mail.ResolveControlFile(".")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return mail.NewMailbox(path), nil
}

func queueSender() string {
	if lane := strings.TrimSpace(os.Getenv("HERD_LANE")); lane != "" {
		return lane
	}
	return queueSenderDefault
}

// immediateDeliveryAllowed reports whether a live agent status is a safe
// kickoff boundary. working, starting, unknown, and any other value are not
// permission to preempt.
func immediateDeliveryAllowed(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "idle", "done":
		return true
	default:
		return false
	}
}

func queueRoutineLocked(ctx context.Context, recipient, body string) (SendResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	box, err := resolveQueueMailbox()
	if err != nil {
		return SendResult{}, fmt.Errorf("queue routine delivery for %s: %w", recipient, err)
	}
	if box == nil {
		return SendResult{}, fmt.Errorf("agent '%s' is not idle; durable queue mailbox is required", recipient)
	}
	env, err := box.QueueRoutine(ctx, queueSender(), recipient, body)
	if err != nil {
		return SendResult{}, fmt.Errorf("queue routine delivery for %s: %w", recipient, err)
	}
	return SendResult{Status: StatusQueuedDurable, EnvelopeID: env.ID}, nil
}

// DrainResult is one envelope considered at an idle/done boundary.
type DrainResult struct {
	EnvelopeID   string
	Delivered    bool
	Acknowledged bool
}

// DrainQueuedAtBoundary surfaces pending routine envelopes at a live idle or
// done turn. It re-reads safe-boundary authority before each message, so a
// recipient that starts working after the first delivery is not prompted again.
// Acknowledgment follows a task-bound consumption receipt, not AgentPrompt or
// a working status alone. Prompt/Enter failure, a staged composer, or unknown
// consumption leave the envelope pending (at-least-once re-delivery, not
// exactly-once side effects).
func DrainQueuedAtBoundary(target, workspace string, box *mail.Mailbox) ([]DrainResult, error) {
	return drainQueuedAtBoundary(target, workspace, box, nil)
}

func drainQueuedAtBoundary(target, workspace string, box *mail.Mailbox, afterDeliver func(*mail.Envelope) error) ([]DrainResult, error) {
	if box == nil {
		return nil, fmt.Errorf("herdr drain: mailbox is required")
	}
	var out []DrainResult
	for {
		resolved, err := requireAgentWorkspaceIn(target, workspace)
		if err != nil {
			return out, err
		}
		resolvedTarget := resolved.Name
		if resolvedTarget == "" {
			resolvedTarget = target
		}
		if !immediateDeliveryAllowed(resolved.Status) {
			return out, fmt.Errorf("%w (status %q)", ErrNotIdleBoundary, resolved.Status)
		}
		pending, err := box.PendingQueued(resolvedTarget)
		if err != nil {
			return out, err
		}
		if len(pending) == 0 {
			return out, nil
		}
		env := pending[0]
		if env == nil {
			return out, fmt.Errorf("herdr drain: nil pending envelope")
		}
		result := DrainResult{EnvelopeID: env.ID}
		delivered, submitErr := submitRoutineAtIdle(resolved, resolvedTarget, env.Body, workspace, queuedProveTimeout)
		result.Delivered = delivered
		if submitErr != nil {
			out = append(out, result)
			return out, fmt.Errorf("drain envelope %s: %w", env.ID, submitErr)
		}
		if afterDeliver != nil {
			if hookErr := afterDeliver(env); hookErr != nil {
				out = append(out, result)
				return out, hookErr
			}
		}
		if err := box.MarkHandled(resolvedTarget, env.ID); err != nil {
			out = append(out, result)
			return out, fmt.Errorf("ack envelope %s: %w", env.ID, err)
		}
		result.Acknowledged = true
		out = append(out, result)
	}
}

func submitRoutineAtIdle(resolved AgentEntry, target, text, workspace string, timeout time.Duration) (bool, error) {
	baselinePane := ""
	if resolved.PaneID != "" {
		pane, err := PaneRead(resolved.PaneID, 120)
		if err != nil {
			return false, fmt.Errorf("agent '%s' pre-drain pane readback failed: %w", target, err)
		}
		baselinePane = pane
	}
	if _, err := AgentPrompt(target, text, false); err != nil {
		return false, err
	}
	if err := SendKeys(target, "Enter"); err != nil {
		return true, err
	}
	if err := proveRoutineConsumption(resolved, target, text, workspace, baselinePane, timeout); err != nil {
		return true, err
	}
	return true, nil
}

func proveRoutineConsumption(resolved AgentEntry, target, text, workspace, baselinePane string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	poll := 2 * time.Second
	if timeout < poll {
		poll = timeout
	}
	deadline := time.Now().Add(timeout)
	last := "unknown"
	lastPane := ""
	staged := false
	for {
		st, err := liveStatusScopedIn(target, workspace)
		if err == nil {
			last = st
			pane, paneErr := PaneRead(resolved.PaneID, 120)
			if paneErr == nil {
				lastPane = pane
				staged = strings.Contains(strings.ToLower(pane), "pasted text") && !taskTextObserved(text, pane)
				// Presence after this submit is the task-bound receipt. A count
				// increase is unnecessary: a crash-before-ack retry may already
				// show the same payload from the earlier prompt. Working status
				// alone is not a receipt.
				if taskTextObserved(text, pane) {
					return nil
				}
				if (st == "working" || st == "done") && !harnessEchoesPrompt(resolved.Kind) && paneAdvanced(baselinePane, pane) {
					return nil
				}
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(poll)
	}
	if staged || strings.Contains(strings.ToLower(lastPane), "pasted text") {
		return errQueuedStaged(target, last)
	}
	return errQueuedUnobserved(target, last)
}
