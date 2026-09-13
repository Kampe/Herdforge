package mergeadmit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// FAC-831 / review 6817b6221d2d: the producer proof path was not physically
// bounded.
//
// Gate.ProveLanded passed context.Background(), so the context-aware layer below
// it had no deadline at all, and every bound that did exist was local: the
// 64-replay cap in integrationCommitFor limits qualifying replay attempts only,
// not the range walk that precedes it, nor the per-commit patch scans, nor the
// number of git subprocesses, nor how many bytes any one of them may print. A
// large or adversarial reachable history could therefore monopolise the machine
// during reconcile. A per-command timeout would not fix that either: ten
// thousand fast commands are unbounded work made of bounded steps.
//
// The budget below is one shared, finite allowance for a whole proof. It rides
// the context, so it reaches every existing helper without changing their
// signatures, and every exhaustion is an explicit fail-closed error rather than
// a truncated answer that could read as proof.

// Budget defaults. These are fixed constants, never derived from the host: a
// bound that depends on the machine is not reproducible, and CI would then be
// proving something different from what an operator runs.
const (
	// DefaultProofDeadline bounds one whole proof, shared by every command in
	// it rather than restarting per command.
	DefaultProofDeadline = 2 * time.Minute
	// defaultProofMaxCommands bounds how many git subprocesses one proof may
	// start.
	defaultProofMaxCommands = 512
	// defaultProofMaxOutputBytes bounds a SINGLE command's stdout. Output is
	// refused at the boundary, never truncated into a shorter answer.
	defaultProofMaxOutputBytes = 32 << 20
	// defaultProofMaxRangeCommits bounds how many commits one range may
	// materialise.
	defaultProofMaxRangeCommits = 5000
)

var (
	// ErrProofBudgetCommands means the proof asked for more git subprocesses
	// than its allowance.
	ErrProofBudgetCommands = errors.New("mergeadmit proof: git command budget exhausted")
	// ErrProofBudgetOutput means one command printed more than its allowance.
	ErrProofBudgetOutput = errors.New("mergeadmit proof: git output budget exhausted")
	// ErrProofBudgetRange means a commit range is larger than the allowance.
	ErrProofBudgetRange = errors.New("mergeadmit proof: commit range budget exhausted")
)

// ProofBudget is one proof's finite allowance. Zero fields take the defaults,
// so a caller can tighten one bound without restating the others.
//
// It is injectable so a test can drive a REAL proof into a REAL exhaustion with
// a tiny allowance, deterministically and without assuming anything about the
// machine's speed, memory or history size.
type ProofBudget struct {
	Deadline        time.Duration
	MaxCommands     int
	MaxOutputBytes  int
	MaxRangeCommits int
}

func (b ProofBudget) withDefaults() ProofBudget {
	if b.Deadline <= 0 {
		b.Deadline = DefaultProofDeadline
	}
	if b.MaxCommands <= 0 {
		b.MaxCommands = defaultProofMaxCommands
	}
	if b.MaxOutputBytes <= 0 {
		b.MaxOutputBytes = defaultProofMaxOutputBytes
	}
	if b.MaxRangeCommits <= 0 {
		b.MaxRangeCommits = defaultProofMaxRangeCommits
	}
	return b
}

// proofLedger is the live consumption of one budget. It is per-proof state, so
// a Gate serving repeated proofs never carries one run's spend into the next.
type proofLedger struct {
	limits ProofBudget
	mu     sync.Mutex
	spent  int
}

type proofBudgetKey struct{}

// withProofBudget installs a budget and its deadline on ctx. The returned
// cancel MUST be called by the caller.
func withProofBudget(ctx context.Context, b ProofBudget) (context.Context, context.CancelFunc) {
	limits := b.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, limits.Deadline)
	return context.WithValue(ctx, proofBudgetKey{}, &proofLedger{limits: limits}), cancel
}

// ensureProofBudget installs a default budget when none is present, so no
// exported entry point can run unbounded. An already-budgeted context keeps its
// allowance: a nested call must not silently get a fresh one.
func ensureProofBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	if ledgerFrom(ctx) != nil {
		return ctx, func() {}
	}
	return withProofBudget(ctx, ProofBudget{})
}

func ledgerFrom(ctx context.Context) *proofLedger {
	l, _ := ctx.Value(proofBudgetKey{}).(*proofLedger)
	return l
}

// spendCommand charges one git subprocess against the budget.
func (l *proofLedger) spendCommand(args []string) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spent >= l.limits.MaxCommands {
		return fmt.Errorf("%w: %d commands already run, refusing `git %s`",
			ErrProofBudgetCommands, l.spent, joinArgs(args))
	}
	l.spent++
	return nil
}

func (l *proofLedger) maxOutputBytes() int {
	if l == nil {
		return defaultProofMaxOutputBytes
	}
	return l.limits.MaxOutputBytes
}

func (l *proofLedger) maxRangeCommits() int {
	if l == nil {
		return defaultProofMaxRangeCommits
	}
	return l.limits.MaxRangeCommits
}

func joinArgs(args []string) string {
	const max = 120
	s := ""
	for i, a := range args {
		if i > 0 {
			s += " "
		}
		s += a
	}
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// boundedBuffer accepts at most limit bytes and then refuses, so an oversized
// producer is stopped at the boundary instead of being buffered whole and
// judged afterwards. Write returns an error on overflow, which makes the
// command fail rather than yielding a silently shortened answer.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	if w.over {
		return 0, ErrProofBudgetOutput
	}
	if w.buf.Len()+len(p) > w.limit {
		w.over = true
		return 0, ErrProofBudgetOutput
	}
	return w.buf.Write(p)
}

func (w *boundedBuffer) Bytes() []byte { return w.buf.Bytes() }

func (w *boundedBuffer) overflowed() bool { return w.over }

// isProofBudgetError reports whether err is one of this package's finite
// allowances refusing, as opposed to a genuine content or repository answer.
//
// It exists because a budget refusal that gets flattened into a descriptive
// message stops being recognisable to errors.Is, and then reads as an ordinary
// failure of whatever step happened to be running. CI 34741746509 caught
// exactly that: resolveCommit reported "base revision does not resolve" for a
// run that had simply spent its allowance.
func isProofBudgetError(err error) bool {
	return errors.Is(err, ErrProofBudgetCommands) ||
		errors.Is(err, ErrProofBudgetOutput) ||
		errors.Is(err, ErrProofBudgetRange)
}

// proofRunAborted reports whether err means the run must STOP rather than be
// answered. A cancelled context and an exhausted allowance are both "I could
// not look", never "I looked and the answer is no".
func proofRunAborted(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return isProofBudgetError(err)
}

// resolveFailure turns a failed revision resolution into the error the caller
// should see, WITHOUT losing what actually went wrong.
//
// CI 34741746509: resolveCommit reported "base revision does not resolve" for a
// run that had merely spent its allowance, because it dropped the cause. Both
// halves below are needed and neither is sufficient alone — a budget refusal is
// returned as itself so it is not buried under a resolution message, and every
// other cause is wrapped with %w so no sentinel further down is lost either.
func resolveFailure(role, rev, repoDir string, err error) error {
	if isProofBudgetError(err) {
		return err
	}
	return fmt.Errorf("%s revision %q does not resolve to a commit in %s: %w", role, rev, repoDir, err)
}
