package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/goalguard"
)

func runGoalGuard() error {
	fs := flag.NewFlagSet("goal-guard", flag.ContinueOnError)
	state := fs.String("state", goalguard.DefaultPath(), "durable goal state path")
	set := fs.Bool("set", false, "create or replace a standing goal")
	check := fs.Bool("check", false, "evaluate evidence JSON from stdin")
	stopHook := fs.Bool("stop-hook", false, "Claude Stop hook mode: silent when no goal, block stop while goal is active")
	clear := fs.Bool("clear", false, "remove the durable goal")
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
		if err := os.Remove(*state); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear: %w", err)
		}
		fmt.Fprintln(os.Stdout, `{"cleared":true}`)
		return nil
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

	block := map[string]string{
		"decision": "block",
		"reason":   goalGuardContinueReason(g.Task, g.Lane, decision.Continuations),
	}
	return writeGoalJSON(os.Stdout, block)
}

// goalGuardPlateauAfter is how many continuations may pass before the guard
// stops saying "keep working" and starts describing a plateau. Three is
// deliberate: one continuation is normal, two can be a slow beat, and by the
// third an unchanged lane is looping rather than progressing.
const goalGuardPlateauAfter = 3

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
		return fmt.Sprintf(preamble+"goal %q on lane %q is not met (continuation %d). Keep working toward the goal; stop only when it is complete, then run `herd goal-guard --clear`.",
			task, lane, continuations)
	}
	return fmt.Sprintf(preamble+"goal %q on lane %q is not met (continuation %d). "+
		"You have continued %d times. If you produced NO new artifact since the last continuation, you are PLATEAUED, and repeating the same probe is not work: it spends quota to re-observe unchanged state. "+
		"Do this instead: (1) say ONCE what you are waiting on, with the counts that prove there is nothing claimable right now; (2) do NOT repeat that report on later continuations; (3) WAIT for a real transition -- a verdict callback, a freed pool slot, a dependency card closing, or new claimable work -- rather than re-running the probe that just returned unchanged. "+
		"Waiting on an event IS valid progress for a standing lane; a lane with genuinely nothing to claim is correctly idle, not failing. "+
		"If you DID produce an artifact since the last continuation, ignore all of the above and keep going. Stop only when the goal is complete, then run `herd goal-guard --clear`.",
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
