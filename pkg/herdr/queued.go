package herdr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

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

func resolveQueueMailbox() *mail.Mailbox {
	if queueMailboxHook != nil {
		return queueMailboxHook()
	}
	root := strings.TrimSpace(os.Getenv("HERD_ROOT"))
	if root == "" {
		root = "."
	}
	return mail.NewMailbox(mail.CallbackMailPath(root))
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
	box := resolveQueueMailbox()
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

// DrainQueuedAtBoundary surfaces pending routine envelopes onto an idle or
// done recipient, then durably acknowledges each successful surface. It does
// not stop a still-running command: a non-idle recipient is refused with
// ErrNotIdleBoundary and no pane writes. Acknowledgment is MarkHandled, not
// the mailbox seen-set; a crash after deliver and before ack can re-surface
// the same envelope (at-least-once, not exactly-once side effects).
func DrainQueuedAtBoundary(target, workspace string, box *mail.Mailbox) ([]DrainResult, error) {
	return drainQueuedAtBoundary(target, workspace, box, nil)
}

func drainQueuedAtBoundary(target, workspace string, box *mail.Mailbox, afterDeliver func(*mail.Envelope) error) ([]DrainResult, error) {
	if box == nil {
		return nil, fmt.Errorf("herdr drain: mailbox is required")
	}
	resolved, err := requireAgentWorkspaceIn(target, workspace)
	if err != nil {
		return nil, err
	}
	resolvedTarget := resolved.Name
	if resolvedTarget == "" {
		resolvedTarget = target
	}
	if !immediateDeliveryAllowed(resolved.Status) {
		return nil, fmt.Errorf("%w (status %q)", ErrNotIdleBoundary, resolved.Status)
	}
	pending, err := box.PendingQueued(resolvedTarget)
	if err != nil {
		return nil, err
	}
	out := make([]DrainResult, 0, len(pending))
	for _, env := range pending {
		if env == nil {
			continue
		}
		result := DrainResult{EnvelopeID: env.ID}
		if _, err := AgentPrompt(resolvedTarget, env.Body, false); err != nil {
			return out, fmt.Errorf("drain envelope %s: %w", env.ID, err)
		}
		_ = SendKeys(resolvedTarget, "Enter")
		result.Delivered = true
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
	return out, nil
}
