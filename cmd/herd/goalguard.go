package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/Kampe/Herdforge/pkg/progress"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/goalguard"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/lock"
	"github.com/Kampe/Herdforge/pkg/security"
)

func runGoalGuard() error {
	fs := flag.NewFlagSet("goal-guard", flag.ContinueOnError)
	state := fs.String("state", goalguard.DefaultPath(), "durable goal state path")
	set := fs.Bool("set", false, "create or replace a standing goal")
	check := fs.Bool("check", false, "evaluate evidence JSON from stdin")
	stopHook := fs.Bool("stop-hook", false, "Claude Stop hook mode: silent when no goal, block stop while goal is active")
	clear := fs.Bool("clear", false, "retire the durable goal with grantor or completion evidence")
	lane := fs.String("lane", "", "standing lane identity")
	task := fs.String("task", "", "task identity")
	owner := fs.String("owner", "", "goal owner")
	generation := fs.Int64("generation", 0, "lease generation")
	max := fs.Int("max", 0, "maximum continuations (0 = unbounded: run until the goal is met)")
	expires := fs.String("expires", "", "RFC3339 expiry")
	grantor := fs.String("grantor", "", "authority envelope grantor")
	packet := fs.String("packet", "", "exact standing packet path")
	autonomy := fs.String("autonomy", "", "bounded standing autonomy")
	mutations := fs.String("mutations", "", "standing mutation limits")
	forbidden := fs.String("forbidden", "", "comma-separated forbidden actions")
	stopConditions := fs.String("stop-conditions", "", "comma-separated stop conditions")
	receipt := fs.String("receipt", "", "task-bound completion receipt proving native Done readback")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	modeCount := 0
	for _, mode := range []bool{*set, *check, *clear, *stopHook} {
		if mode {
			modeCount++
		}
	}
	if modeCount != 1 {
		return errors.New("exactly one of --set, --check, --stop-hook, or --clear is required")
	}
	s, err := goalguard.Open(*state)
	if err != nil {
		return err
	}
	if *clear {
		return clearGoal(s, *grantor, *generation, *receipt)
	}
	if *set {
		now := time.Now().UTC()
		g := goalguard.Goal{SchemaVersion: goalguard.SchemaVersion, Lane: *lane, Task: *task, Owner: *owner, Generation: *generation, MaxContinuations: *max, CreatedAt: now, UpdatedAt: now}
		if strings.TrimSpace(*grantor) != "" || strings.TrimSpace(*packet) != "" {
			g.Authority = &goalguard.AuthorityEnvelope{Grantor: *grantor, PacketPath: *packet, BoundedAutonomy: *autonomy, MutationLimits: *mutations, ForbiddenActions: splitGoalCSV(*forbidden), StopConditions: splitGoalCSV(*stopConditions)}
		}
		if strings.TrimSpace(*expires) != "" {
			expiry, parseErr := time.Parse(time.RFC3339Nano, *expires)
			if parseErr != nil {
				return fmt.Errorf("parse expiry: %w", parseErr)
			}
			g.ExpiresAt = &expiry
		}
		if err := s.Set(g); err != nil {
			return err
		}
		return writeGoalJSON(os.Stdout, g)
	}
	if *stopHook {
		return runGoalGuardStopHook(s, nil)
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var evidence goalguard.Evidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return fmt.Errorf("decode evidence: %w", err)
	}
	if strings.TrimSpace(evidence.Lane) == "" {
		// stdin is not Evidence JSON — it is a Claude Stop hook payload.
		// --check wired as a Stop hook must behave like --stop-hook, not
		// spam "incomplete evidence" on every session end.
		return runGoalGuardStopHook(s, raw)
	}
	decision, err := s.Evaluate(evidence)
	if errors.Is(err, goalguard.ErrMissing) {
		// No durable goal means there is nothing to guard. Stop hooks run
		// --check on every session end; absence is a quiet no-decision, not
		// an error to spam.
		return writeGoalJSON(os.Stdout, goalguard.Decision{Reason: "no_goal"})
	}
	if err != nil {
		return err
	}
	return writeGoalJSON(os.Stdout, decision)
}

func clearGoal(s *goalguard.Store, grantor string, generation int64, receiptPath string) error {
	fence := lock.NewDirLock(s.Path() + ".lock.d")
	if err := fence.Acquire(context.Background(), 0, "goalguard clear"); err != nil {
		return fmt.Errorf("clear: acquire lock: %w", err)
	}
	defer fence.Release()
	g, err := s.Load()
	if errors.Is(err, goalguard.ErrMissing) {
		// An unqualified clear preserves the historical quiet no-op. Supplying
		// proof against an absent goal, however, is a replay or stale request.
		if strings.TrimSpace(grantor) == "" && strings.TrimSpace(receiptPath) == "" && generation == 0 {
			fmt.Fprintln(os.Stdout, `{"cleared":false,"reason":"no_goal"}`)
			return nil
		}
		return errors.New("goal-guard: clear refused: goal is missing; authorization is stale or replayed")
	}
	if err != nil {
		return fmt.Errorf("clear: %w", err)
	}
	if retired, checkErr := s.HasRetirement(); checkErr != nil {
		return checkErr
	} else if retired {
		return errors.New("goal-guard: clear refused: retirement already recorded (replay)")
	}

	grantor = strings.TrimSpace(grantor)
	receiptPath = strings.TrimSpace(receiptPath)
	if grantor != "" || generation != 0 {
		if grantor == "" || generation <= 0 {
			return errors.New("goal-guard: clear refused: grantor and positive current generation are required")
		}
		if g.Authority == nil {
			return fmt.Errorf("goal-guard: clear refused for owner %q: no recorded grantor; use coordinator retirement or a valid completion receipt", g.Owner)
		}
		if err := g.Authority.Validate(); err != nil {
			return fmt.Errorf("goal-guard: clear refused for owner %q: recorded authority invalid: %w", g.Owner, err)
		}
		if grantor != g.Authority.Grantor {
			return fmt.Errorf("goal-guard: clear refused for owner %q: grantor %q is not the recorded grantor %q", g.Owner, grantor, g.Authority.Grantor)
		}
		if generation != g.Generation {
			return fmt.Errorf("goal-guard: clear refused for owner %q: stale generation %d (goal generation %d)", g.Owner, generation, g.Generation)
		}
		if err := validateNativeGrantor(g, grantor, generation); err != nil {
			return err
		}
		return retireGoal(s, g, goalguard.Retirement{Lane: g.Lane, Task: g.Task, Owner: g.Owner, Generation: g.Generation, Grantor: grantor, RetiredAt: time.Now().UTC()})
	}
	if receiptPath == "" {
		return fmt.Errorf("goal-guard: clear refused for owner %q: agent-only clear is not supported; coordinator must provide --grantor/--generation or --receipt", g.Owner)
	}
	receipt, err := hsync.LoadReceipt(receiptPath)
	if err != nil {
		return fmt.Errorf("goal-guard: clear refused: load completion receipt: %w", err)
	}
	if err := validateGoalCompletionReceipt(g, receipt); err != nil {
		return err
	}
	return retireGoal(s, g, goalguard.Retirement{Lane: g.Lane, Task: g.Task, Owner: g.Owner, Generation: g.Generation, Receipt: receipt.Digest, RetiredAt: time.Now().UTC()})
}

func validateNativeGrantor(g goalguard.Goal, grantor string, generation int64) error {
	path := security.CanonicalLeaseDBPath(".")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("goal-guard: clear refused for owner %q: native claim authority is missing; coordinator must present the live claim", g.Owner)
		}
		return fmt.Errorf("goal-guard: clear refused: inspect native claim authority: %w", err)
	}
	if err := security.WireCanonicalClaimAuthority("."); err != nil {
		return fmt.Errorf("goal-guard: clear refused: open native claim authority: %w", err)
	}
	lookup, err := security.RequireClaimAuthority()
	if err != nil {
		return fmt.Errorf("goal-guard: clear refused: %w", err)
	}
	current, err := lookup.LookupActiveClaim(context.Background(), g.Task)
	if err != nil {
		return fmt.Errorf("goal-guard: clear refused for task %q: native claim is not active: %w", g.Task, err)
	}
	if current == nil || current.TaskRef != g.Task || current.OwnerID != grantor || current.Generation != generation {
		return fmt.Errorf("goal-guard: clear refused: native claim owner/generation mismatch (want owner=%q generation=%d)", grantor, generation)
	}
	caller, err := goalGuardCallerIdentity()
	if err != nil {
		return fmt.Errorf("goal-guard: clear refused: caller is not a verified native coordinator session: %w", err)
	}
	if caller != grantor {
		return fmt.Errorf("goal-guard: clear refused: grantor %q does not match verified native caller %q", grantor, caller)
	}
	return nil
}

var goalGuardCallerIdentity = resolveGoalGuardCallerIdentity

func resolveGoalGuardCallerIdentity() (string, error) {
	pane := strings.TrimSpace(os.Getenv("HERDR_PANE_ID"))
	if pane == "" {
		return "", errors.New("HERDR_PANE_ID is missing")
	}
	agents, err := herdr.AgentList()
	if err != nil {
		return "", fmt.Errorf("herdr agent list: %w", err)
	}
	var match *herdr.AgentEntry
	for i := range agents {
		if strings.TrimSpace(agents[i].PaneID) == pane {
			if match != nil || strings.TrimSpace(agents[i].Name) == "" || !herdr.RealModelSessionID(agents[i].Session.Value) {
				return "", errors.New("pane has no unique real model session")
			}
			match = &agents[i]
		}
	}
	if match == nil {
		return "", fmt.Errorf("no live agent is bound to pane %q", pane)
	}
	return match.Name, nil
}

func validateGoalCompletionReceipt(g goalguard.Goal, receipt *hsync.CompletionReceipt) error {
	if receipt == nil || receipt.Digest == "" || receipt.Digest != receipt.ComputeDigest() {
		return errors.New("goal-guard: clear refused: completion receipt is missing or has an invalid digest")
	}
	if !strings.EqualFold(hsync.NormalizeRef(receipt.TaskRef), hsync.NormalizeRef(g.Task)) {
		return fmt.Errorf("goal-guard: clear refused: receipt task %q does not match goal task %q", receipt.TaskRef, g.Task)
	}
	if receipt.Verdict != "PASS" || receipt.IntegrationResult != hsync.IntegrationMerged {
		return errors.New("goal-guard: clear refused: completion receipt is not a merged PASS")
	}
	log, err := hsync.ReadDoneLog(".")
	if err != nil {
		return fmt.Errorf("goal-guard: clear refused: native Done readback unavailable: %w", err)
	}
	for _, record := range log {
		if strings.EqualFold(hsync.NormalizeRef(record.Ref), hsync.NormalizeRef(g.Task)) && record.ReceiptDigest == receipt.Digest && strings.EqualFold(record.ProviderReadback, "done") {
			return nil
		}
	}
	return fmt.Errorf("goal-guard: clear refused: receipt %s has no native Done readback", receipt.Digest)
}

func retireGoal(s *goalguard.Store, g goalguard.Goal, retirement goalguard.Retirement) error {
	if err := s.RecordRetirement(retirement); err != nil {
		return err
	}
	if err := s.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
		// The retirement marker remains evidence if this final unlink fails.
		return fmt.Errorf("clear: remove goal after retirement: %w", err)
	}
	fmt.Fprintln(os.Stdout, `{"cleared":true}`)
	return nil
}

// runGoalGuardStopHook adapts the guard to Claude Code's Stop hook contract:
// no durable goal means nothing to guard (silent exit 0, stop allowed), and an
// active goal blocks the stop via {"decision":"block"} so the agent keeps
// working until the goal is met. When the payload carries stop_hook_active the
// previous block in this turn was already delivered, so return success —
// re-blocking loops until the harness force-overrides. The nudge repeats on
// the agent's next natural stop instead.
func runGoalGuardStopHook(s *goalguard.Store, payload []byte) error {
	if payload == nil {
		payload, _ = io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	}
	var hook struct {
		StopHookActive bool `json:"stop_hook_active"`
	}
	_ = json.Unmarshal(payload, &hook)
	if hook.StopHookActive {
		return nil
	}

	// FAC-532: a Stop hook that RETURNS AN ERROR terminates the agent. Every
	// error path below used to do exactly that, so a guard whose whole purpose
	// is keeping lanes working was instead killing them on any unreadable
	// state. Nothing here may return a non-nil error: an undeterminable guard
	// reports on stderr and allows the stop, which is recoverable, rather than
	// failing the hook, which is not.
	g, err := s.Load()
	if err != nil {
		if !errors.Is(err, goalguard.ErrMissing) {
			fmt.Fprintf(os.Stderr, "goal-guard: cannot read goal, allowing stop: %v\n", err)
		}
		return nil
	}
	leaseHeld, err := goalGuardLeaseHeld(g)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goal-guard: cannot read lease, allowing stop: %v\n", err)
		return nil
	}
	evidence := goalguard.Evidence{Lane: g.Lane, Task: g.Task, Owner: g.Owner, Generation: g.Generation, LeaseHeld: leaseHeld, Now: time.Now().UTC()}
	// FAC-581 correction (independent review finding 5): the event-wait branch
	// needs the production observation, not only test-constructed evidence.
	// The HEAD commit of the worktree the hook runs in (cwd is the lane's
	// worktree) is the artifact a builder lane actually produces; the durable
	// baseline on the goal makes an UNCHANGED HEAD an event wait instead of a
	// silently spent continuation. Best-effort: a non-git cwd reports no
	// observation and leaves the pre-existing behavior unchanged.
	if head := goalGuardObserveWorktreeHEAD(); head != "" {
		evidence.ProgressClass = progress.ClassBuild
		evidence.Artifact = head
	}
	decision, err := s.Evaluate(evidence)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goal-guard: cannot evaluate goal, allowing stop: %v\n", err)
		return nil
	}
	if !decision.Continue {
		return writeGoalJSON(os.Stdout, decision)
	}

	// A goal recorded before authority envelopes existed (FAC-525) is still an
	// operator-granted goal — it predates the field, it is not unauthorized.
	// Treat it as legacy-granted and keep the lane working, warning so the
	// backlog of envelope-less goals stays visible. Refusing here would strand
	// every lane whose goal was set before FAC-525 landed.
	switch {
	case g.Authority == nil:
		fmt.Fprintf(os.Stderr, "goal-guard: goal %q on lane %q predates authority envelopes; continuing on legacy grant (re-set the goal to record one)\n", g.Task, g.Lane)
	default:
		if err := g.Authority.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "goal-guard: authority envelope invalid, allowing stop: %v\n", err)
			return nil
		}
	}

	blockReason := goalGuardContinueReason(g.Task, g.Lane, decision.Continuations)
	if decision.Reason == "event_wait" {
		blockReason = goalGuardEventWaitReason(g.Task, g.Lane, decision.Continuations)
	}
	block := map[string]string{
		"decision": "block",
		"reason":   blockReason,
	}
	return writeGoalJSON(os.Stdout, block)
}

// goalGuardObserveWorktreeHEAD observes the lane's actual production artifact:
// the HEAD commit of the worktree the Stop hook runs in. The hook's cwd is the
// lane worktree, so this is the exact artifact the lane produced since its last
// stop. Best-effort by design: git is always present, but a non-git cwd (or a
// git failure) reports no observation and the guard behaves as before.
func goalGuardObserveWorktreeHEAD() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// goalGuardEventWaitReason is the block instruction for a lane that produced no
// new artifact since the last continuation. FAC-652 landed "block means hold,
// quietly"; FAC-581 stops the wait from SPENDING budget. The lane is held, not
// killed, and is told to wait for a real transition instead of re-probing the
// unchanged state that just returned.
func goalGuardEventWaitReason(task, lane string, continuations int) string {
	const preamble = "AUTOMATED STOP-HOOK OUTPUT — NOT AN ASSIGNMENT. goal-guard: "
	return fmt.Sprintf(preamble+"goal %q on lane %q produced no new artifact since the last continuation (event wait; continuation %d NOT spent). "+
		"Waiting is valid progress for a standing lane: do NOT re-run the probe that just returned unchanged. "+
		"Wait for a real transition -- a verdict callback, a freed pool slot, a dependency card closing, or new claimable work -- or produce a new artifact and continue.",
		task, lane, continuations)
}

// goalGuardPlateauAfter is the shared plateau threshold, taken from the package
// that owns what progress MEANS rather than redefined here (FAC-665). A second
// copy of one rule is how the two drift.
const goalGuardPlateauAfter = progress.PlateauAfter

// goalGuardContinueReason is the instruction a blocked lane actually reads.
//
// FAC-652: this said "Keep working toward the goal; stop only when it is
// complete" on EVERY continuation, with no concept of having nothing to do. A
// standing lane whose queue is momentarily empty was therefore told to keep
// working, forever, and the only behaviours available to it were to spin or to
// die. Both were observed on the live fleet: perf-cost-guard emitted
// near-identical reports every one to two minutes, and herd-smith reached
// continuation 42 doing twenty-minute waits for a review cap to move -- burning
// a provider at 8% remaining to produce no artifact at all.
//
// Waiting is not failure. A standing lane is a loop, and a loop with no work
// available should be parked on an event, not asked to re-probe unchanged state.
// So past a plateau threshold the instruction changes shape: report the plateau
// ONCE with the counts that prove it, then wait for a real transition. The lane
// still may not stop -- the goal is still unmet and the guard still blocks --
// but "block" now means "hold, quietly" instead of "keep trying things".
//
// The events named here are the ones that actually exist: FAC-651 made an
// admitted verdict post a completion callback, and pool slots free on reap.
func goalGuardContinueReason(task, lane string, continuations int) string {
	const preamble = "AUTOMATED STOP-HOOK OUTPUT — NOT AN ASSIGNMENT. goal-guard: "
	if continuations < goalGuardPlateauAfter {
		return fmt.Sprintf(preamble+"goal %q on lane %q is not met (continuation %d). Keep working toward the goal; stop only when it is complete and the coordinator retires it.",
			task, lane, continuations)
	}
	return fmt.Sprintf(preamble+"goal %q on lane %q is not met (continuation %d). "+
		"You have continued %d times. If you produced NO new artifact since the last continuation, you are PLATEAUED, and repeating the same probe is not work: it spends quota to re-observe unchanged state. "+
		"Do this instead: (1) say ONCE what you are waiting on, with the counts that prove there is nothing claimable right now; (2) do NOT repeat that report on later continuations; (3) WAIT for a real transition -- a verdict callback, a freed pool slot, a dependency card closing, or new claimable work -- rather than re-running the probe that just returned unchanged. "+
		"Waiting on an event IS valid progress for a standing lane; a lane with genuinely nothing to claim is correctly idle, not failing. "+
		"If you DID produce an artifact since the last continuation, ignore all of the above and keep going. Stop only when the goal is complete and the coordinator retires it.",
		task, lane, continuations, continuations)
}

func splitGoalCSV(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// goalGuardLeaseHeld reads the same durable launch lease store used by the
// coordinator and pulse. A missing store has no live lease, while an existing
// store that cannot be read is an error so the Stop hook never invents live
// authority from an unavailable claim database.
// goalGuardLeaseHeld reports whether this lane still owns its launch lease.
//
// FAC-626: a MISSING lease store used to return false, which Evaluate reads as
// !LeaseHeld and converts into reason="lease_lost", continue=false. A standing
// lane whose worktree has no .herd/launch-claims.db was therefore told its lease
// had been LOST on every single stop, so the review-harvest supervisor completed
// one beat and halted, forever. Measured on the live lane: no lease db exists,
// goal Stop.LeaseLost is false, and the hook still returned lease_lost.
//
// Absence of a lease store is UNKNOWN, not loss. The distinction is the whole
// safety property: a store that EXISTS and does not list this lane is a genuine
// loss and must still stop it. Only the unprovable case now continues.
func goalGuardLeaseHeld(g goalguard.Goal) (bool, error) {
	path := leaseDBPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		// No store: nothing can be proven either way. Treat as held so an active
		// goal is not killed by a file that was never created.
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("goal-guard: inspect lease store: %w", err)
	}
	store, err := claim.NewSQLiteLeaseStore(path)
	if err != nil {
		return false, fmt.Errorf("goal-guard: open lease store: %w", err)
	}
	defer store.Close()
	leases, err := store.ActiveClaims(context.Background(), time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("goal-guard: read lease store: %w", err)
	}
	for _, lease := range leases {
		if lease != nil && lease.TaskRef == g.Task && lease.Generation == g.Generation && lease.HoldLane == g.Lane {
			return true, nil
		}
	}
	return false, nil
}

func writeGoalJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
