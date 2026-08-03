package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// OpKind classifies a TaskProvider operation for per-kind deadlines.
type OpKind string

const (
	OpGet      OpKind = "get"
	OpList     OpKind = "list"
	OpMutate   OpKind = "mutate" // ClaimTask, UpdateStatus
	OpComment  OpKind = "comment"
	OpReadback OpKind = "readback"
)

// Default per-operation deadlines. Chosen so a hung board (observed 30–90s
// stalls) fails closed well before a worker lane parks indefinitely, while
// still allowing slow-but-alive pagination and readback.
const (
	DefaultGetDeadline      = 15 * time.Second
	DefaultListDeadline     = 30 * time.Second
	DefaultMutateDeadline   = 20 * time.Second
	DefaultCommentDeadline  = 15 * time.Second
	DefaultReadbackDeadline = 15 * time.Second
)

// Deadlines holds per-operation bounds. Zero fields resolve to defaults via
// For / Normalize so callers can override a single op without restating all.
type Deadlines struct {
	Get      time.Duration
	List     time.Duration
	Mutate   time.Duration
	Comment  time.Duration
	Readback time.Duration
}

// DefaultDeadlines returns the repository-safe defaults for every op kind.
func DefaultDeadlines() Deadlines {
	return Deadlines{
		Get:      DefaultGetDeadline,
		List:     DefaultListDeadline,
		Mutate:   DefaultMutateDeadline,
		Comment:  DefaultCommentDeadline,
		Readback: DefaultReadbackDeadline,
	}
}

// Normalize fills zero fields with defaults. Negative values are treated as
// zero (defaulted) so a misconfigured config cannot disable the bound.
func (d Deadlines) Normalize() Deadlines {
	def := DefaultDeadlines()
	if d.Get <= 0 {
		d.Get = def.Get
	}
	if d.List <= 0 {
		d.List = def.List
	}
	if d.Mutate <= 0 {
		d.Mutate = def.Mutate
	}
	if d.Comment <= 0 {
		d.Comment = def.Comment
	}
	if d.Readback <= 0 {
		d.Readback = def.Readback
	}
	return d
}

// For returns the deadline for op, defaulting zero fields.
func (d Deadlines) For(op OpKind) time.Duration {
	n := d.Normalize()
	switch op {
	case OpGet:
		return n.Get
	case OpList:
		return n.List
	case OpMutate:
		return n.Mutate
	case OpComment:
		return n.Comment
	case OpReadback:
		return n.Readback
	default:
		return n.Get
	}
}

// Max returns the longest configured deadline (after normalize). Useful as an
// HTTP client safety-net timeout independent of a specific op context.
func (d Deadlines) Max() time.Duration {
	n := d.Normalize()
	max := n.Get
	for _, v := range []time.Duration{n.List, n.Mutate, n.Comment, n.Readback} {
		if v > max {
			max = v
		}
	}
	return max
}

// WithOpDeadline derives a child context bounded by the op deadline.
// If the parent already has a nearer deadline, that nearer bound wins
// (context.WithTimeout still respects parent).
//
// Always pair with defer cancel(). Never pass context.Background() across a
// provider boundary without this wrapper — that is the FAC-150 hang class.
func WithOpDeadline(ctx context.Context, d Deadlines, op OpKind) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, d.For(op))
}

// TimeoutError is a typed provider-operation timeout / cancellation failure.
// Callers must treat it as a hard error: never map it to empty success,
// zero tasks, free capacity, or board advancement.
type TimeoutError struct {
	Provider string
	Op       string
	Kind     OpKind
	Deadline time.Duration
	// Cause is typically context.DeadlineExceeded or context.Canceled.
	Cause error
}

func (e *TimeoutError) Error() string {
	if e == nil {
		return "<nil timeout error>"
	}
	var b string
	if e.Provider != "" {
		b = e.Provider + ": "
	}
	if e.Op != "" {
		b += e.Op + ": "
	}
	kind := string(e.Kind)
	if kind == "" {
		kind = "op"
	}
	msg := fmt.Sprintf("%s deadline exceeded", kind)
	if e.Deadline > 0 {
		msg = fmt.Sprintf("%s deadline exceeded after %s", kind, e.Deadline)
	}
	if e.Cause != nil {
		if errors.Is(e.Cause, context.Canceled) && !errors.Is(e.Cause, context.DeadlineExceeded) {
			msg = fmt.Sprintf("%s canceled", kind)
			if e.Deadline > 0 {
				msg = fmt.Sprintf("%s canceled (deadline was %s)", kind, e.Deadline)
			}
		}
		return b + msg + ": " + e.Cause.Error()
	}
	return b + msg
}

func (e *TimeoutError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Timeout reports true so net.Error and timeout-aware callers can classify it.
func (e *TimeoutError) Timeout() bool { return true }

// IsTimeout reports whether err is (or wraps) a provider TimeoutError or a
// context deadline/cancel. Prefer this over raw errors.Is for lane BLOCKED
// projection (provider_timeout).
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	var te *TimeoutError
	if errors.As(err, &te) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// AsTimeout wraps err as *TimeoutError when it represents a deadline/cancel,
// preserving an existing *TimeoutError. Non-timeout errors are returned as-is.
func AsTimeout(provider, op string, kind OpKind, deadline time.Duration, err error) error {
	if err == nil {
		return nil
	}
	var te *TimeoutError
	if errors.As(err, &te) {
		if te.Provider == "" {
			te.Provider = provider
		}
		if te.Op == "" {
			te.Op = op
		}
		if te.Kind == "" {
			te.Kind = kind
		}
		if te.Deadline == 0 {
			te.Deadline = deadline
		}
		return te
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &TimeoutError{
			Provider: provider,
			Op:       op,
			Kind:     kind,
			Deadline: deadline,
			Cause:    err,
		}
	}
	return err
}

// ClassifyContextErr maps a finished context error to a TimeoutError when
// appropriate. Returns nil when ctx is still live (err should come from the
// call, not the context).
func ClassifyContextErr(ctx context.Context, provider, op string, kind OpKind, deadline time.Duration) error {
	if ctx == nil {
		return nil
	}
	err := ctx.Err()
	if err == nil {
		return nil
	}
	return AsTimeout(provider, op, kind, deadline, err)
}
