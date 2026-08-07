package shot

// FAC-89 — the bounded ONE-TASK lane.
//
// `herd shot <task-ref>` drives exactly one card through eligibility, atomic
// claim + isolated dispatch, the completion callback, exact-SHA verification,
// and handoff to review. Then it stops: it never merges and never marks a card
// Done. Every exit path — including every failure — emits the same structured
// Evidence packet, so a script or a recovery sweep can tell which stage the
// shot reached and what durable state it left behind.
//
// This file wires primitives that already exist (pkg/eligibility, pkg/dispatch,
// pkg/mail callbacks, pkg/verifier receipts, pkg/reviewledger). It owns the
// ORDER and the fail-closed gates between them, nothing else. The external
// boundaries are function fields so a hermetic test can drive every stage
// without a board, a fleet, or a provider.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/eligibility"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

// Stage names the furthest point one shot reached. It is the primary recovery
// signal: the stage says which side effects can already exist.
type Stage string

const (
	StageLock        Stage = "lock"
	StageEligibility Stage = "eligibility"
	StageDispatch    Stage = "dispatch"
	StageIsolation   Stage = "isolation"
	StageCallback    Stage = "callback"
	StageVerify      Stage = "verify"
	StageReview      Stage = "review"
)

// DefaultCallbackTimeout bounds the wait for the builder's completion
// callback. A shot is bounded by definition; there is no "wait forever".
const DefaultCallbackTimeout = 15 * time.Minute

var (
	taskRefRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)
	exactSHA  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// IsTaskRef reports whether s is a board task reference (FAC-89, ENG-12).
// The CLI uses it to tell `herd shot FAC-89` (bounded task) from
// `herd shot <prompt words>` (the headless prompt lane above).
func IsTaskRef(s string) bool { return taskRefRE.MatchString(strings.TrimSpace(s)) }

// DispatchFacts is what the atomic claim + isolated dispatch stage reports.
// Branch is what dispatch BELIEVES it created; the isolation stage re-reads the
// real branch from the worktree and refuses a mismatch.
type DispatchFacts struct {
	Worktree        string
	Branch          string
	BaseSHA         string
	LeaseGeneration int64
	Lane            string
	Launched        bool
}

// Evidence is the structured result of one shot. It is emitted on success and
// on every failure, so `herd shot --json` is script-consumable either way.
type Evidence struct {
	TaskRef             string `json:"task_ref"`
	Stage               Stage  `json:"stage"`
	OK                  bool   `json:"ok"`
	Lane                string `json:"lane,omitempty"`
	Worktree            string `json:"worktree,omitempty"`
	Branch              string `json:"branch,omitempty"`
	BaseSHA             string `json:"base_sha,omitempty"`
	CandidateSHA        string `json:"candidate_sha,omitempty"`
	LeaseGeneration     int64  `json:"lease_generation,omitempty"`
	ReceiptDigest       string `json:"receipt_digest,omitempty"`
	VerificationOutcome string `json:"verification_outcome,omitempty"`
	// Recoverable reports that this shot left durable claim/worktree/lease
	// state behind. false means nothing was claimed and a retry is clean.
	Recoverable bool   `json:"recoverable"`
	Error       string `json:"error,omitempty"`
}

// TaskRequest is one bounded run. Root is the repository whose .herd tree owns
// the single-flight lock.
type TaskRequest struct {
	TaskRef         string
	Lane            string
	Root            string
	CallbackTimeout time.Duration
}

// TaskRun holds the external boundaries of the bounded lane. Every field is
// mandatory: a nil seam is a wiring bug, not a reason to skip a gate.
type TaskRun struct {
	// Eligible answers the board-eligibility question for exactly this ref.
	Eligible func(ctx context.Context, ref string) (eligibility.Result, error)
	// Dispatch performs the atomic claim and the isolated launch.
	Dispatch func(ctx context.Context, ref, lane string) (DispatchFacts, error)
	// Await blocks until the builder posts a callback for this ref/lease.
	Await func(ctx context.Context, ref string, lease int64) (mail.Callback, error)
	// Verify runs the exact-SHA verification and returns its receipt.
	Verify func(ctx context.Context, dir string, req verifier.VerificationRequest) (*verifier.Receipt, error)
	// Handoff admits the verified candidate to review. It must not merge.
	Handoff func(ctx context.Context, ev Evidence) error
}

// Run drives one task and returns its evidence. The returned Evidence is
// always populated, including alongside a non-nil error.
func (r *TaskRun) Run(ctx context.Context, req TaskRequest) (Evidence, error) {
	ref := strings.TrimSpace(req.TaskRef)
	ev := Evidence{TaskRef: ref, Stage: StageLock, Lane: strings.TrimSpace(req.Lane)}

	if !IsTaskRef(ref) {
		return fail(ev, false, fmt.Errorf("shot: %q is not a task reference", req.TaskRef))
	}
	if ev.Lane == "" {
		return fail(ev, false, errors.New("shot: lane or role is required"))
	}
	if strings.TrimSpace(req.Root) == "" {
		return fail(ev, false, errors.New("shot: repository root is required"))
	}
	if err := r.wired(); err != nil {
		return fail(ev, false, err)
	}

	// Duplicate invocations must not double-claim or double-launch. The claim
	// itself is atomic downstream; this refuses the second process before it
	// can even ask, so the board never sees two competing claim attempts.
	unlock, err := lockTask(req.Root, ref)
	if err != nil {
		return fail(ev, false, err)
	}
	defer unlock()

	timeout := req.CallbackTimeout
	if timeout <= 0 {
		timeout = DefaultCallbackTimeout
	}

	ev.Stage = StageEligibility
	res, err := r.Eligible(ctx, ref)
	if err != nil {
		return fail(ev, false, err)
	}
	// One invocation may affect only the requested task: an answer about some
	// other card is a wiring fault, never a licence to proceed.
	if res.Ref != "" && !strings.EqualFold(strings.TrimSpace(res.Ref), ref) {
		return fail(ev, false, fmt.Errorf("shot: eligibility answered for %s, not %s", res.Ref, ref))
	}
	if res.State != eligibility.StateEligible {
		return fail(ev, false, fmt.Errorf("shot: %s is %s: %s",
			ref, res.State, strings.Join(res.Reasons, "; ")))
	}

	ev.Stage = StageDispatch
	facts, err := r.Dispatch(ctx, ref, req.Lane)
	if err != nil {
		// Dispatch is where side effects begin. A failure part-way through may
		// have left a claim, a worktree, or a tab behind for its compensator
		// and the recovery sweep, so this is never a clean retry.
		return fail(ev, true, err)
	}
	ev.Worktree, ev.BaseSHA, ev.LeaseGeneration = facts.Worktree, facts.BaseSHA, facts.LeaseGeneration
	if strings.TrimSpace(facts.Lane) != "" {
		ev.Lane = strings.TrimSpace(facts.Lane)
	}
	// Past this point a claim, a worktree, and a lease exist on disk and on the
	// board. Every later failure is recoverable state, not a clean retry.

	ev.Stage = StageIsolation
	if facts.LeaseGeneration <= 0 {
		return fail(ev, true, errors.New("shot: dispatch returned no lease generation"))
	}
	restore, resolved, branch, err := enterWorktree(facts.Worktree)
	if err != nil {
		return fail(ev, true, err)
	}
	defer restore()
	ev.Worktree, ev.Branch = resolved, branch
	if want := strings.TrimSpace(facts.Branch); want != "" && want != branch {
		return fail(ev, true, fmt.Errorf(
			"shot: dispatch reported branch %q but %s is actually on %q", want, resolved, branch))
	}

	ev.Stage = StageCallback
	cbCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cb, err := r.Await(cbCtx, ref, facts.LeaseGeneration)
	if err != nil {
		return fail(ev, true, err)
	}
	if err := validateCallback(cb, ref, facts.LeaseGeneration); err != nil {
		return fail(ev, true, err)
	}
	ev.CandidateSHA = cb.SHA

	ev.Stage = StageVerify
	receipt, err := r.Verify(ctx, resolved, verifier.VerificationRequest{
		TaskRef:           ref,
		LeaseGeneration:   strconv.FormatInt(facts.LeaseGeneration, 10),
		CandidateSHA:      cb.SHA,
		BaseSHA:           facts.BaseSHA,
		EnvironmentPolicy: verifier.EnvironmentPolicyHermetic,
	})
	if err != nil {
		return fail(ev, true, err)
	}
	if receipt == nil {
		return fail(ev, true, errors.New("shot: verification returned no receipt"))
	}
	ev.ReceiptDigest, ev.VerificationOutcome = receipt.Digest, string(receipt.Outcome)
	// Exact-SHA: a receipt for a different commit proves nothing about this one.
	if receipt.CandidateSHA != cb.SHA {
		return fail(ev, true, fmt.Errorf("shot: receipt covers %s, candidate is %s",
			receipt.CandidateSHA, cb.SHA))
	}
	if receipt.Outcome != verifier.OutcomePASS {
		return fail(ev, true, fmt.Errorf("shot: verification %s for %s", receipt.Outcome, cb.SHA))
	}

	ev.Stage = StageReview
	if err := r.Handoff(ctx, ev); err != nil {
		return fail(ev, true, err)
	}
	ev.OK, ev.Recoverable = true, true
	return ev, nil
}

func (r *TaskRun) wired() error {
	if r == nil {
		return errors.New("shot: nil task run")
	}
	var missing []string
	for name, seam := range map[string]bool{
		"eligibility": r.Eligible == nil,
		"dispatch":    r.Dispatch == nil,
		"callback":    r.Await == nil,
		"verify":      r.Verify == nil,
		"review":      r.Handoff == nil,
	} {
		if seam {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("shot: task run is not wired: %s", strings.Join(missing, ", "))
	}
	return nil
}

// validateCallback binds one report to THIS card and THIS lease. Without it a
// stale agent from a previous lease, or a callback about a neighbouring card,
// would be accepted as this shot's completion.
func validateCallback(cb mail.Callback, ref string, lease int64) error {
	if !strings.EqualFold(strings.TrimSpace(cb.Ref), ref) {
		return fmt.Errorf("shot: callback names %q, not %s", cb.Ref, ref)
	}
	if cb.LeaseGeneration != lease {
		return fmt.Errorf("shot: callback carries lease generation %d, dispatch holds %d (stale agent)",
			cb.LeaseGeneration, lease)
	}
	switch cb.Kind {
	case mail.CallbackComplete:
	case mail.CallbackBlocked:
		detail := strings.TrimSpace(cb.Detail)
		if detail == "" {
			detail = "(no detail)"
		}
		return fmt.Errorf("shot: %s reported BLOCKED: %s", ref, detail)
	default:
		return fmt.Errorf("shot: unknown callback kind %q", cb.Kind)
	}
	if !exactSHA.MatchString(strings.TrimSpace(cb.SHA)) {
		return fmt.Errorf("shot: completion callback carries %q, not an exact 40-character commit SHA", cb.SHA)
	}
	return nil
}

// enterWorktree puts the shot process INSIDE its assigned worktree and reads
// the branch that is actually checked out there. Both halves matter: the
// acceptance contract is that cwd equals the assigned worktree and the branch
// recorded is the real one, not the one dispatch intended to create.
func enterWorktree(dir string) (restore func(), resolved, branch string, err error) {
	if strings.TrimSpace(dir) == "" {
		return nil, "", "", errors.New("shot: dispatch returned no worktree")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, "", "", fmt.Errorf("shot: resolve worktree %s: %w", dir, err)
	}
	// macOS hands out /var/... paths that are really /private/var/...; without
	// EvalSymlinks the cwd comparison below fails on every temp worktree.
	if real, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = real
	}
	prev, err := os.Getwd()
	if err != nil {
		return nil, "", "", fmt.Errorf("shot: read current directory: %w", err)
	}
	if err := os.Chdir(abs); err != nil {
		return nil, "", "", fmt.Errorf("shot: enter worktree %s: %w", abs, err)
	}
	restore = func() { _ = os.Chdir(prev) }
	cwd, err := os.Getwd()
	if err != nil {
		restore()
		return nil, "", "", fmt.Errorf("shot: read worktree directory: %w", err)
	}
	if cwd != abs {
		restore()
		return nil, "", "", fmt.Errorf("shot: process cwd is %s, assigned worktree is %s", cwd, abs)
	}
	branch, err = currentBranch(abs)
	if err != nil {
		restore()
		return nil, "", "", err
	}
	return restore, abs, branch, nil
}

func currentBranch(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("shot: %s has no checked-out branch (detached HEAD or not a worktree): %w", dir, err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("shot: %s reported an empty branch name", dir)
	}
	return name, nil
}

// lockTask is the single-flight guard that makes a duplicate `herd shot <ref>`
// refuse instead of racing a second claim and launch for the same card.
// os.Mkdir is atomic on every filesystem we support, so nothing heavier earns
// its keep.
//
// pkg/lock.DirLock is deliberately NOT reused: it returns success without
// touching the filesystem when HERD_SHARED_LOCK_HELD is set in the
// environment, and an environment variable must never be able to switch off a
// claim-safety gate.
func lockTask(root, ref string) (func(), error) {
	dir := filepath.Join(root, ".herd", "locks", "shot-"+strings.ToLower(ref)+".lock.d")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, fmt.Errorf("shot: create lock directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("shot: acquire %s lock: %w", ref, err)
		}
		if !breakDeadLock(dir) {
			return nil, fmt.Errorf("shot: another herd shot already holds %s (%s)", ref, dir)
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return nil, fmt.Errorf("shot: another herd shot already holds %s (%s)", ref, dir)
		}
	}
	holder := filepath.Join(dir, "holder")
	if err := os.WriteFile(holder, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("shot: record %s lock holder: %w", ref, err)
	}
	return func() { _ = os.RemoveAll(dir) }, nil
}

// breakDeadLock removes a lock whose holder process is gone, so a crashed shot
// does not wedge its card until an operator deletes the directory by hand. A
// lock with a live or unreadable holder is left alone.
func breakDeadLock(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "holder"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	if syscall.Kill(pid, 0) == nil {
		return false
	}
	return os.RemoveAll(dir) == nil
}

func fail(ev Evidence, recoverable bool, err error) (Evidence, error) {
	ev.OK = false
	ev.Recoverable = recoverable
	ev.Error = err.Error()
	return ev, err
}
