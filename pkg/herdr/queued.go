package herdr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/mail"
)

const (
	// StatusQueuedDurable is a successful busy-path receipt: the payload is
	// on the mailbox, and it has not been consumed by the pane.
	StatusQueuedDurable = "queued-durable"
	// queueSenderDefault is the shared UNBOUND sender. One definition, in
	// pkg/mail, because both layers have to refuse it for the same reason.
	queueSenderDefault = mail.AnonymousIssuer
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
	if lane := boundIssuer(); lane != "" {
		return lane
	}
	return queueSenderDefault
}

// boundIssuer is the caller's VERIFIED coordinator identity, or empty when
// the invocation carries none.
//
// HERD_LANE is the native lane binding (role-inject exports it, laneenv
// carries it, the feedback path already validates replies against it). An
// invocation without it is anonymous: it queues perfectly well as
// mail.AnonymousIssuer, which is why ordinary FIFO send stays compatible, but
// it is not an identity and must never authorize retiring someone else's
// queued work. A lane that literally names itself the unbound default is
// treated as unbound too, so the fallback cannot be spoofed into an identity.
func boundIssuer() string {
	lane := strings.TrimSpace(os.Getenv("HERD_LANE"))
	if lane == "" || lane == mail.AnonymousIssuer {
		return ""
	}
	return lane
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

// TargetBinding is the opaque identity of one live delivery target: which
// canonical repository, which workspace, which agent name, and — crucially —
// which TERMINAL GENERATION and harness session.
//
// A recipient name is not a recipient. When a host reboots, or an operator
// relaunches a wedged lane, the new session answers to the same name while
// owning none of the old session's work. herdr's terminal_id is the
// generation token that separates them, so it is what makes "this agent, now"
// provable. Binding is hashed rather than stored raw: it becomes durable
// mailbox evidence, and no absolute path belongs in that.
//
// An empty result means the target cannot be identified (no terminal id, or
// no resolvable repository root). Callers must treat that as "unbound", never
// as a match: an unidentifiable target can still be QUEUED to, but it can
// never authorize retiring another session's queued work.
func TargetBinding(resolved AgentEntry) string {
	terminal := strings.TrimSpace(resolved.TerminalID)
	// The harness SESSION is required, not merely folded in when present. A
	// terminal generation identifies a tab, and the same tab can host a
	// restarted harness that has not reported a session yet — same
	// terminal_id, entirely different agent, none of the old work. Without
	// the agent's own session value there is no proof of WHICH agent answers,
	// so the target is unbound and scoped operations must refuse it.
	session := strings.TrimSpace(resolved.Session.Value)
	if terminal == "" || session == "" {
		return ""
	}
	root, _, err := gitroot.ProjectRoot(context.Background(), ".")
	if err != nil || strings.TrimSpace(root) == "" {
		return ""
	}
	canonical := strings.Join([]string{
		strings.TrimSpace(root),
		strings.TrimSpace(resolved.Workspace),
		strings.TrimSpace(resolved.Name),
		terminal,
		session,
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return "tb-" + hex.EncodeToString(sum[:12])
}

func queueRoutineLocked(ctx context.Context, resolved AgentEntry, recipient, body string) (SendResult, error) {
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
	env, err := box.QueueRoutine(ctx, queueSender(), recipient, TargetBinding(resolved), body)
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
// Ordinary report mail is eligible alongside queued-durable mail; authenticated
// control and callback envelopes remain on their native consumers.
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
		pending, err := box.PendingRoutine(resolvedTarget)
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

// SupersedeResult reports one explicit operator supersession: the pending
// envelopes from the same issuer that were made ineligible, and the durable
// identity of the replacement payload that took their place.
type SupersedeResult struct {
	SupersededIDs []string
	EnvelopeID    string
	Idempotent    bool
}

// SupersedeAndQueueRoutine replaces a coordinator's stale pending prompts for
// one live target with a fresh payload. The recipient is resolved against the
// live fleet first, so an unknown or ambiguous target is refused before
// anything is touched. Only the recipient's pending queued-routine envelopes
// from THIS issuer (queueSender) are retired: control, callback, and ordinary
// mail are structurally out of scope, and the replacement is delivered by the
// existing idle-boundary drain with its existing consumption verification.
// The pane is never written to here; a busy recipient simply receives the
// replacement at its next boundary instead of the stale payload.
func SupersedeAndQueueRoutine(ctx context.Context, target, workspace, body string) (SupersedeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resolved, err := requireAgentWorkspaceIn(target, workspace)
	if err != nil {
		return SupersedeResult{}, fmt.Errorf("herdr supersede: %w", err)
	}
	recipient := resolved.Name
	if recipient == "" {
		recipient = target
	}
	issuer := boundIssuer()
	if issuer == "" {
		return SupersedeResult{}, fmt.Errorf("herdr supersede for %s: this invocation has no bound coordinator identity (HERD_LANE); refusing to retire queued work as the shared anonymous sender", recipient)
	}
	binding := TargetBinding(resolved)
	if binding == "" {
		return SupersedeResult{}, fmt.Errorf("herdr supersede for %s: the live target reports no harness session for its terminal; refusing to retire queued work on a pane generation alone", recipient)
	}
	box, err := resolveQueueMailbox()
	if err != nil {
		return SupersedeResult{}, fmt.Errorf("herdr supersede for %s: %w", recipient, err)
	}
	if box == nil {
		return SupersedeResult{}, fmt.Errorf("herdr supersede for %s: durable queue mailbox is required", recipient)
	}
	out, err := box.SupersedePendingRoutine(ctx, issuer, recipient, binding, body, "")
	if err != nil {
		return SupersedeResult{}, fmt.Errorf("herdr supersede for %s: %w", recipient, err)
	}
	if out.EnvelopeID == "" {
		return SupersedeResult{}, fmt.Errorf("herdr supersede for %s: no replacement identity was produced", recipient)
	}
	// Zero victims is a legitimate outcome, not a failure: the operator
	// supplied a real assignment and it is now durably queued. Refusing here
	// would DISCARD that assignment because nothing stale happened to be
	// pending — the opposite of what this command is for.
	return SupersedeResult{
		SupersededIDs: out.SupersededIDs,
		EnvelopeID:    out.EnvelopeID,
		Idempotent:    out.Idempotent,
	}, nil
}
