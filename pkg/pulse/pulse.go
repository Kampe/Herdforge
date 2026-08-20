// Package pulse implements one deterministic coordinator heartbeat (FAC-73).
//
// A beat reads every source once, classifies fleet and queue posture, plans
// only safe bounded actions, and (under --act) applies renewals and idempotent
// callback consumption. Observation is the default: mutations never run as a
// side effect of looking. Unknown critical state fails closed — no dispatch,
// non-zero action result — and provider/Herdr errors never become free capacity.
package pulse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AgentStatus is the coordinator classification for one observed unit of work
// or standing lane. Values are stable for JSON and human output.
type AgentStatus string

const (
	StatusHealthyIdle AgentStatus = "healthy_idle"
	StatusBusy        AgentStatus = "busy"
	StatusBlocked     AgentStatus = "blocked"
	StatusDone        AgentStatus = "done"
	StatusStale       AgentStatus = "stale"
	StatusUnknown     AgentStatus = "unknown"
)

// ActionKind names a bounded action the beat may plan or apply.
type ActionKind string

const (
	ActionReconcile       ActionKind = "reconcile"
	ActionRenewLease      ActionKind = "renew_lease"
	ActionConsumeCallback ActionKind = "consume_callback"
	ActionOpenReview      ActionKind = "open_review"
	ActionReapLane        ActionKind = "reap_lane"
	ActionDispatch        ActionKind = "dispatch"
	ActionWouldRun        ActionKind = "would_run"
)

// Mode is the beat posture. Observe never mutates; Act applies safe renewals
// and callback consumption; Act+Spawn may also plan dispatch when capacity is
// known and healthy.
type Mode string

const (
	ModeObserve  Mode = "observe"
	ModeAct      Mode = "act"
	ModeActSpawn Mode = "act+spawn"
)

// Kind order for deterministic action lists (stable across runs).
var actionKindOrder = map[ActionKind]int{
	ActionReconcile:       1,
	ActionRenewLease:      2,
	ActionConsumeCallback: 3,
	ActionOpenReview:      4,
	ActionReapLane:        5,
	ActionDispatch:        6,
	ActionWouldRun:        7,
}

// Options configure one beat.
type Options struct {
	// Act enables bounded mutations (lease renew, callback consume, reconcile).
	Act bool
	// Spawn enables dispatch planning when Act is also true. Spawn alone is invalid.
	Spawn bool
	// Reason is recorded on the snapshot for replay/debugging.
	Reason string
	// Now overrides the wall clock (tests / fake-clock).
	Now time.Time
	// RenewWithin renews leases whose remaining TTL is at or below this bound.
	// Zero defaults to half of each lease's original TTL, floored at 30s.
	RenewWithin time.Duration
	// BeatSequence is an optional durable sequence position for this beat.
	// When zero, Plan assigns 1. Callers that persist beats should pass the
	// next monotonic value so restarts keep a stable log order.
	BeatSequence uint64
}

// ModeOf returns the posture for the given flags. Spawn without Act is rejected
// by Validate, not silently downgraded.
func (o Options) ModeOf() Mode {
	if o.Act && o.Spawn {
		return ModeActSpawn
	}
	if o.Act {
		return ModeAct
	}
	return ModeObserve
}

// Validate checks flag combinations that must fail before any read.
func (o Options) Validate() error {
	if o.Spawn && !o.Act {
		return errors.New("pulse: --spawn requires --act (spawning is never observation)")
	}
	return nil
}

// ProviderObservation is one read of the task provider / board.
type ProviderObservation struct {
	Known       bool   `json:"known"`
	Error       string `json:"error,omitempty"`
	QueueDepth  int64  `json:"queue_depth"`
	Claimable   int64  `json:"claimable"`
	InProgress  int64  `json:"in_progress"`
	NextTaskRef string `json:"next_task_ref,omitempty"`
	ObservedSeq uint64 `json:"observed_seq,omitempty"`
}

// HerdrObservation is one read of the live fleet.
type HerdrObservation struct {
	Known  bool               `json:"known"`
	Error  string             `json:"error,omitempty"`
	Agents []AgentObservation `json:"agents,omitempty"`
}

// AgentObservation is one standing or one-off lane.
type AgentObservation struct {
	Name              string      `json:"name"`
	Status            AgentStatus `json:"status"`
	Raw               string      `json:"raw_status,omitempty"`
	PaneID            string      `json:"pane_id,omitempty"`
	PaneState         string      `json:"pane_state,omitempty"`
	ForegroundProcess string      `json:"foreground_process,omitempty"`
	ExitReason        string      `json:"exit_reason,omitempty"`
	LastError         string      `json:"last_error,omitempty"`
	ContextWarning    string      `json:"context_warning,omitempty"`
	// Stale is set when the source marks the lane past its progress bound.
	Stale bool `json:"stale,omitempty"`
	// FAC-221: reap evidence. Filled by the caller from worktree, board,
	// review, and safe-ref sources. Plan uses these to decide reap vs keep.
	// TabID identifies the lane for the reap close target.
	TabID string `json:"tab_id,omitempty"`
	// Workspace is the herdr workspace the tab lives in (reap close target).
	Workspace string `json:"workspace,omitempty"`
	// CommittedWork is true when the lane's worktree has committed (non-empty)
	// work. An idle agent with uncommitted work is never reaped.
	CommittedWork bool `json:"committed_work,omitempty"`
	// TicketDone is true when the board status for the lane's task is done.
	TicketDone bool `json:"ticket_done,omitempty"`
	// SafeRef is the safe/fac-<ref> pin protecting the lane's tip. A non-empty
	// SafeRef means the branch is out for review and the agent has nothing to
	// do until a verdict returns.
	SafeRef string `json:"safe_ref,omitempty"`
	// AwaitingVerdict is true when a review verdict has been returned that the
	// agent must act on (e.g. "changes requested"). This is the KEEP signal:
	// the lane is idle but it has specific pending work only it can do.
	AwaitingVerdict bool `json:"awaiting_verdict,omitempty"`
	// PacketPending is the fail-closed signal that Herdr has not exposed a
	// consumed goal/packet for this pane. Reapers must keep such a lane.
	PacketPending bool `json:"packet_pending,omitempty"`
	// TabGeneration is the herdr tab lifecycle generation at observation time.
	// The reap close path requires this for FAC-180 compare-and-close fencing.
	// Zero means unknown; ReapLane will fail closed rather than close unfenced.
	TabGeneration uint64 `json:"tab_generation,omitempty"`
	// TabRevision is the herdr tab revision counter at observation time.
	TabRevision uint64 `json:"tab_revision,omitempty"`
}

// LeaseObservation is one durable claim lease row.
type LeaseObservation struct {
	Repo     string `json:"repo"`
	Provider string `json:"provider"`
	Project  string `json:"project"`
	TaskRef  string `json:"task_ref"`
	OwnerID  string `json:"owner_id"`
	// HoldLane is the canonical lane identity protected by this lease's hold.
	// It lets dispatch planning exclude held capacity before spending the beat.
	HoldLane   string    `json:"hold_lane,omitempty"`
	Generation int64     `json:"generation"`
	Held       bool      `json:"held,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	ClaimedAt  time.Time `json:"claimed_at,omitempty"`
	RenewedAt  time.Time `json:"renewed_at,omitempty"`
	// Active is true only for the current live generation.
	Active bool `json:"active"`
}

// CallbackObservation is one unacked coordinator-inbox callback.
type CallbackObservation struct {
	EnvelopeID      string `json:"envelope_id"`
	Sequence        int64  `json:"sequence"`
	Ref             string `json:"ref"`
	Kind            string `json:"kind"`
	LeaseGeneration int64  `json:"lease_generation,omitempty"`
	Attempt         int    `json:"attempt,omitempty"`
}

// ReviewObservation is one read of review-pile pressure. RawVetoed is the
// unfiltered, unexpired SHA set from the review ledger; it is intentionally
// distinct from drain's NeedReview, which is the live unmerged-candidate set.
type ReviewObservation struct {
	Known         bool     `json:"known"`
	Error         string   `json:"error,omitempty"`
	Pending       int      `json:"pending"`
	PendingRefs   []string `json:"pending_refs,omitempty"`
	RawVetoed     int      `json:"raw_vetoed"`
	RawVetoedRefs []string `json:"raw_vetoed_refs,omitempty"`
	Saturated     bool     `json:"saturated,omitempty"`
}

// QuotaObservation is one read of capacity/quota posture.
type QuotaObservation struct {
	Known     bool   `json:"known"`
	Error     string `json:"error,omitempty"`
	Exhausted bool   `json:"exhausted,omitempty"`
	AtRisk    bool   `json:"at_risk,omitempty"`
}

// WindDownObservation is one read of the fleet wind-down gate.
type WindDownObservation struct {
	Known      bool   `json:"known"`
	Error      string `json:"error,omitempty"`
	Enabled    bool   `json:"enabled"`
	Generation uint64 `json:"generation,omitempty"`
}

// BrokerObservation is the coordinator's receipt-gated provider control-plane
// health. A missing broker is not free capacity: dispatch cannot safely launch
// a lane until this probe is serving.
type BrokerObservation struct {
	Known   bool   `json:"known"`
	Serving bool   `json:"serving"`
	Socket  string `json:"socket,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Observation is the full one-shot source snapshot for a beat. Every field is
// filled by a single read of its source; Plan never re-reads.
type Observation struct {
	Provider  ProviderObservation   `json:"provider"`
	Herdr     HerdrObservation      `json:"herdr"`
	Leases    []LeaseObservation    `json:"leases,omitempty"`
	Callbacks []CallbackObservation `json:"callbacks,omitempty"`
	Review    ReviewObservation     `json:"review"`
	Quota     QuotaObservation      `json:"quota"`
	WindDown  WindDownObservation   `json:"wind_down"`
	Broker    BrokerObservation     `json:"broker"`
	// NeedsReconcile is true when durable lifecycle/control events are pending.
	NeedsReconcile bool `json:"needs_reconcile,omitempty"`
}

// Action is one planned (and optionally applied) beat step with a stable
// sequence position for replay and debugging.
type Action struct {
	Sequence int        `json:"sequence"`
	Kind     ActionKind `json:"kind"`
	Target   string     `json:"target"`
	Reason   string     `json:"reason"`
	// Safe means Apply may execute this under --act (renew/consume/reconcile).
	// Dispatch is Safe only under act+spawn with known healthy capacity.
	Safe bool `json:"safe"`
	// Applied is set after a successful Apply for this action.
	Applied bool `json:"applied,omitempty"`
	// ApplyError records a non-fatal apply failure for this action.
	ApplyError string `json:"apply_error,omitempty"`
	// WouldRun is the exact command or description withheld in observe mode.
	WouldRun string `json:"would_run,omitempty"`
}

// Counts are the human and JSON shared tallies. They must be identical across
// both renderers for the same Snapshot.
type Counts struct {
	Agents          int `json:"agents"`
	HealthyIdle     int `json:"healthy_idle"`
	Busy            int `json:"busy"`
	Blocked         int `json:"blocked"`
	Done            int `json:"done"`
	Stale           int `json:"stale"`
	Unknown         int `json:"unknown"`
	Actions         int `json:"actions"`
	RenewLeases     int `json:"renew_leases"`
	ConsumeCallback int `json:"consume_callbacks"`
	Dispatch        int `json:"dispatch"`
	OpenReview      int `json:"open_review"`
	ReapLanes       int `json:"reap_lanes"`
	WouldRun        int `json:"would_run"`
	Reconcile       int `json:"reconcile"`
	Applied         int `json:"applied"`
}

// Snapshot is the complete beat product: sources, ordered actions, counts, and
// the exit posture. JSON encoding of Counts must match FormatHuman counts.
type Snapshot struct {
	Mode                Mode               `json:"mode"`
	Reason              string             `json:"reason,omitempty"`
	ObservedAt          time.Time          `json:"observed_at"`
	BeatSequence        uint64             `json:"beat_sequence"`
	Observation         Observation        `json:"observation"`
	Agents              []AgentObservation `json:"agents"`
	Actions             []Action           `json:"actions"`
	Counts              Counts             `json:"counts"`
	UnknownCritical     bool               `json:"unknown_critical"`
	UnknownReasons      []string           `json:"unknown_reasons,omitempty"`
	DispatchBlocked     bool               `json:"dispatch_blocked"`
	DispatchBlockReason string             `json:"dispatch_block_reason,omitempty"`
	// ExitCode is 0 when the beat is healthy; non-zero when unknown critical
	// state is present or an applied action failed hard.
	ExitCode int `json:"exit_code"`
}

// Actor applies the safe half of a planned beat. Implementations must fence
// lease renewals by generation and treat callback acks as idempotent.
type Actor interface {
	Reconcile(ctx context.Context) error
	RenewLease(ctx context.Context, lease LeaseObservation) error
	ConsumeCallback(ctx context.Context, cb CallbackObservation) error
	// OpenReview sends a finished lane's committed work to adversarial review.
	// FAC-226: this is the event trigger that was missing — every beat detects
	// finished lanes (idle/done + committed work + not already out for review)
	// and plans this action so the coordinator cannot miss a lane that needs
	// review. The implementation must resolve the lane's worktree, verify the
	// rebase-onto-origin/main non-empty diff, and hand off to the review
	// supervisor. A nil or error means no review was opened.
	OpenReview(ctx context.Context, lane AgentObservation) error
	// ReapLane closes an idle lane's tab. FAC-221: the reap target is the
	// lane in the observation; the implementation must use generation-fenced
	// close (TabCloseCAS) and fail closed when fencing evidence is incomplete.
	ReapLane(ctx context.Context, lane AgentObservation) error
	// Dispatch is optional; nil or error means no launch happened.
	Dispatch(ctx context.Context, target, reason string) error
}

// ErrUnknownCritical is returned by Apply when the snapshot forbids mutation
// of dispatch capacity (renew/consume may still be attempted if Safe).
var ErrUnknownCritical = errors.New("pulse: unknown critical state")

// ErrDispatchBlocked is returned when Apply is asked to dispatch while blocked.
var ErrDispatchBlocked = errors.New("pulse: dispatch blocked")

// ClassifyStatus maps a raw agent status string into the coordinator taxonomy.
func ClassifyStatus(raw string, stale bool) AgentStatus {
	if stale {
		return StatusStale
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "working", "starting", "busy":
		return StatusBusy
	case "blocked":
		return StatusBlocked
	case "done":
		return StatusDone
	case "idle":
		return StatusHealthyIdle
	case "", "unknown":
		return StatusUnknown
	default:
		return StatusUnknown
	}
}

// Plan builds a deterministic ordered action list from a single Observation.
// Same inputs always produce the same ordered Actions and Counts.
func Plan(obs Observation, opts Options) (Snapshot, error) {
	if err := opts.Validate(); err != nil {
		return Snapshot{}, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	seq := opts.BeatSequence
	if seq == 0 {
		seq = 1
	}

	snap := Snapshot{
		Mode:         opts.ModeOf(),
		Reason:       opts.Reason,
		ObservedAt:   now,
		BeatSequence: seq,
		Observation:  obs,
	}

	// Classify agents once from the Herdr observation.
	agents := make([]AgentObservation, 0, len(obs.Herdr.Agents))
	for _, a := range obs.Herdr.Agents {
		name := strings.TrimSpace(a.Name)
		raw := a.Raw
		if raw == "" {
			raw = string(a.Status)
		}
		st := a.Status
		if st == "" {
			st = ClassifyStatus(raw, a.Stale)
		} else if a.Stale {
			st = StatusStale
		}
		agents = append(agents, AgentObservation{
			Name:              name,
			Status:            st,
			Raw:               raw,
			PaneID:            a.PaneID,
			PaneState:         a.PaneState,
			ForegroundProcess: a.ForegroundProcess,
			ExitReason:        a.ExitReason,
			LastError:         a.LastError,
			ContextWarning:    a.ContextWarning,
			Stale:             a.Stale || st == StatusStale,
			TabID:             a.TabID,
			Workspace:         a.Workspace,
			CommittedWork:     a.CommittedWork,
			TicketDone:        a.TicketDone,
			SafeRef:           a.SafeRef,
			AwaitingVerdict:   a.AwaitingVerdict,
			PacketPending:     a.PacketPending,
			TabGeneration:     a.TabGeneration,
			TabRevision:       a.TabRevision,
		})
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].Name < agents[j].Name
	})
	snap.Agents = agents

	// Critical unknown: never treat errors as zero work or free capacity.
	var unknownReasons []string
	if !obs.Provider.Known || strings.TrimSpace(obs.Provider.Error) != "" {
		unknownReasons = append(unknownReasons, "provider: "+unknownDetail(obs.Provider.Known, obs.Provider.Error))
	}
	if !obs.Herdr.Known || strings.TrimSpace(obs.Herdr.Error) != "" {
		unknownReasons = append(unknownReasons, "herdr: "+unknownDetail(obs.Herdr.Known, obs.Herdr.Error))
	}
	if !obs.Quota.Known || strings.TrimSpace(obs.Quota.Error) != "" {
		unknownReasons = append(unknownReasons, "quota: "+unknownDetail(obs.Quota.Known, obs.Quota.Error))
	}
	if !obs.WindDown.Known || strings.TrimSpace(obs.WindDown.Error) != "" {
		unknownReasons = append(unknownReasons, "wind_down: "+unknownDetail(obs.WindDown.Known, obs.WindDown.Error))
	}
	brokerObserved := obs.Broker.Known || strings.TrimSpace(obs.Broker.Error) != "" || obs.Broker.Serving || strings.TrimSpace(obs.Broker.Socket) != ""
	if brokerObserved && (!obs.Broker.Known || strings.TrimSpace(obs.Broker.Error) != "") {
		unknownReasons = append(unknownReasons, "broker: "+unknownDetail(obs.Broker.Known, obs.Broker.Error))
	}
	// Review unknown is pressure uncertainty — block dispatch but do not alone
	// force a broken beat unless it is the only critical path for launches.
	reviewUnknown := !obs.Review.Known || strings.TrimSpace(obs.Review.Error) != ""

	snap.UnknownCritical = len(unknownReasons) > 0
	snap.UnknownReasons = unknownReasons

	// Dispatch gating (capacity + posture).
	blockReason := ""
	switch {
	case snap.UnknownCritical:
		blockReason = "unknown critical source: " + strings.Join(unknownReasons, "; ")
	case reviewUnknown:
		blockReason = "review posture unknown: " + unknownDetail(obs.Review.Known, obs.Review.Error)
	case obs.WindDown.Enabled:
		blockReason = "wind-down enabled"
	case brokerObserved && !obs.Broker.Serving:
		blockReason = "coordinator broker unavailable"
	case obs.Quota.Exhausted:
		blockReason = "quota exhausted"
	case obs.Review.Saturated:
		blockReason = "review saturated"
	case obs.Provider.Claimable <= 0:
		blockReason = "no claimable work"
	}
	snap.DispatchBlocked = blockReason != ""
	snap.DispatchBlockReason = blockReason

	var actions []Action

	// Reconcile durable events first when pending.
	if obs.NeedsReconcile {
		a := Action{
			Kind:   ActionReconcile,
			Target: "durable-events",
			Reason: "pending durable lifecycle/control events require reconciliation",
			Safe:   true,
		}
		if !opts.Act {
			a.Kind = ActionWouldRun
			a.WouldRun = "reconcile durable events"
			a.Reason = "would reconcile durable events (--act)"
			a.Safe = false
		}
		actions = append(actions, a)
	}

	// Renew only current (active) lease generations that need extension.
	leases := append([]LeaseObservation(nil), obs.Leases...)
	sort.Slice(leases, func(i, j int) bool {
		if leases[i].TaskRef != leases[j].TaskRef {
			return leases[i].TaskRef < leases[j].TaskRef
		}
		return leases[i].Generation < leases[j].Generation
	})
	for _, l := range leases {
		if !l.Active || l.Held || l.Generation <= 0 {
			continue
		}
		if l.ExpiresAt.IsZero() {
			continue
		}
		if !l.ExpiresAt.After(now) {
			// Already expired — never renew a non-current generation window.
			continue
		}
		if !needsRenew(l, now, opts.RenewWithin) {
			continue
		}
		target := leaseTarget(l)
		a := Action{
			Kind:   ActionRenewLease,
			Target: target,
			Reason: fmt.Sprintf("renew active lease generation %d for %s before expiry", l.Generation, l.TaskRef),
			Safe:   true,
		}
		if !opts.Act {
			a.Kind = ActionWouldRun
			a.WouldRun = "renew_lease " + target
			a.Reason = "would renew lease generation " + fmt.Sprintf("%d", l.Generation) + " (--act)"
			a.Safe = false
		}
		actions = append(actions, a)
	}

	// Consume callbacks idempotently (plan every unacked observation once).
	cbs := append([]CallbackObservation(nil), obs.Callbacks...)
	sort.Slice(cbs, func(i, j int) bool {
		if cbs[i].Sequence != cbs[j].Sequence {
			return cbs[i].Sequence < cbs[j].Sequence
		}
		return cbs[i].EnvelopeID < cbs[j].EnvelopeID
	})
	for _, cb := range cbs {
		target := cb.EnvelopeID
		if target == "" {
			target = fmt.Sprintf("seq:%d:%s", cb.Sequence, cb.Ref)
		}
		a := Action{
			Kind:   ActionConsumeCallback,
			Target: target,
			Reason: fmt.Sprintf("consume callback %s for %s (seq %d)", cb.Kind, cb.Ref, cb.Sequence),
			Safe:   true,
		}
		if !opts.Act {
			a.Kind = ActionWouldRun
			a.WouldRun = "consume_callback " + target
			a.Reason = "would consume callback (--act)"
			a.Safe = false
		}
		actions = append(actions, a)
	}

	// FAC-226: Detect finished lanes and open review. A lane is FINISHED when
	// its agent is idle/done AND it has committed work (non-empty diff against
	// origin/main) AND it is not already out for review (no SafeRef) AND its
	// ticket is not done (work not yet landed) AND it is not awaiting a verdict
	// it must act on. At that moment the lane should be sent to adversarial
	// review and then closed. This is the event trigger that was missing: every
	// beat plans open_review for finished lanes so the coordinator cannot miss
	// them. A lane that is already out for review (SafeRef set) or whose ticket
	// is done (work landed) is reaped instead — see the reap block below.
	for _, a := range agents {
		if a.Name == "" {
			continue
		}
		if a.Status != StatusHealthyIdle && a.Status != StatusDone {
			continue
		}
		if a.AwaitingVerdict {
			continue
		}
		if a.PacketPending {
			continue
		}
		if !a.CommittedWork {
			continue
		}
		if strings.TrimSpace(a.SafeRef) != "" {
			continue
		}
		if a.TicketDone {
			continue
		}
		target := a.Name
		if a.TabID != "" {
			target = a.TabID
		}
		reason := "open review: idle/done lane with committed work not yet out for review"
		act := Action{
			Kind:   ActionOpenReview,
			Target: target,
			Reason: reason,
			Safe:   true,
		}
		if !opts.Act {
			act.Kind = ActionWouldRun
			act.WouldRun = "open_review " + target
			act.Reason = "would open review for finished lane (--act): " + reason
			act.Safe = false
		}
		actions = append(actions, act)
	}

	// FAC-221: Reap idle lanes. A lane exists only while it is doing
	// something. An idle or done lane with a done ticket or a safe-ref pinning
	// its branch out for review is reap-eligible — close it and respawn on
	// demand. A lane awaiting a verdict it must act on is KEPT: it is idle but
	// has specific pending work only it can do. A lane that is FINISHED
	// (CommittedWork but no SafeRef and not TicketDone) gets OpenReview above,
	// not ReapLane — it must be sent to review before it is closed. This is
	// code, not coordinator discipline: every beat plans reap actions for
	// eligible lanes so they cannot be left resident by forgetfulness.
	for _, a := range agents {
		if a.Name == "" {
			continue
		}
		if a.Status != StatusHealthyIdle && a.Status != StatusDone {
			continue
		}
		if a.AwaitingVerdict {
			continue
		}
		if a.PacketPending {
			continue
		}
		// Only reap lanes that are already out for review or whose ticket is
		// done. Lanes with committed work but no SafeRef and no TicketDone are
		// FINISHED — they get OpenReview above, not ReapLane.
		hasReapEvidence := a.TicketDone || strings.TrimSpace(a.SafeRef) != ""
		if !hasReapEvidence {
			continue
		}
		target := a.Name
		if a.TabID != "" {
			target = a.TabID
		}
		var reasonParts []string
		switch {
		case a.TicketDone:
			reasonParts = append(reasonParts, "ticket done")
		case strings.TrimSpace(a.SafeRef) != "":
			reasonParts = append(reasonParts, "branch out for review at "+a.SafeRef)
		}
		reason := "reap idle lane: " + strings.Join(reasonParts, "; ")
		act := Action{
			Kind:   ActionReapLane,
			Target: target,
			Reason: reason,
			Safe:   true,
		}
		if !opts.Act {
			act.Kind = ActionWouldRun
			act.WouldRun = "reap_lane " + target
			act.Reason = "would reap idle lane (--act): " + strings.Join(reasonParts, "; ")
			act.Safe = false
		}
		actions = append(actions, act)
	}

	// Dispatch: only when act+spawn and not blocked. Observe/act print would-run.
	if opts.Act && opts.Spawn && !snap.DispatchBlocked {
		// Prefer a healthy idle lane as target; else generic queue. A held lease
		// names the canonical lane it protects, so exclude it before selecting
		// the one bounded dispatch target for this beat.
		heldLanes := make(map[string]bool)
		for _, lease := range obs.Leases {
			if lease.Held && strings.TrimSpace(lease.HoldLane) != "" {
				heldLanes[strings.TrimSpace(lease.HoldLane)] = true
			}
		}
		target := "queue"
		hasIdleLane := false
		for _, a := range agents {
			if a.Status != StatusHealthyIdle || a.Name == "" {
				continue
			}
			hasIdleLane = true
			if !heldLanes[a.Name] {
				target = a.Name
				break
			}
		}
		if target != "queue" && hasIdleLane {
			actions = append(actions, Action{
				Kind:   ActionDispatch,
				Target: target,
				Reason: "safe bounded dispatch: capacity known, no critical unknown",
				Safe:   true,
			})
		} else {
			reason := "dispatch withheld: no eligible target; all healthy idle lanes are held"
			if !hasIdleLane {
				reason = "dispatch withheld: no eligible target; no healthy idle lanes"
			}
			actions = append(actions, Action{
				Kind:     ActionWouldRun,
				Target:   "dispatch",
				Reason:   reason,
				WouldRun: "dispatch withheld",
				Safe:     false,
			})
		}
	} else if opts.Spawn || (opts.Act && opts.Spawn) {
		// unreachable spawn-without-act already rejected
	} else {
		// Record withheld dispatch plan when there would be work under spawn.
		if obs.Provider.Claimable > 0 {
			hint := "--act --spawn"
			reason := "would dispatch"
			if snap.DispatchBlocked {
				reason = "dispatch blocked: " + blockReason
			}
			actions = append(actions, Action{
				Kind:     ActionWouldRun,
				Target:   "dispatch",
				Reason:   reason + " (" + hint + ")",
				WouldRun: "dispatch bounded work",
				Safe:     false,
			})
		}
	}

	// Deterministic order: kind rank, then target, then reason.
	sort.SliceStable(actions, func(i, j int) bool {
		oi, oj := actionKindOrder[actions[i].Kind], actionKindOrder[actions[j].Kind]
		if oi != oj {
			return oi < oj
		}
		if actions[i].Target != actions[j].Target {
			return actions[i].Target < actions[j].Target
		}
		return actions[i].Reason < actions[j].Reason
	})
	for i := range actions {
		actions[i].Sequence = i + 1
	}
	snap.Actions = actions
	snap.Counts = CountActions(agents, actions)

	// Exit posture: unknown critical → non-zero; no dispatch when critical.
	if snap.UnknownCritical {
		snap.ExitCode = 1
	}
	return snap, nil
}

func unknownDetail(known bool, err string) string {
	if strings.TrimSpace(err) != "" {
		return err
	}
	if !known {
		return "not observed"
	}
	return "unknown"
}

func leaseTarget(l LeaseObservation) string {
	return fmt.Sprintf("%s|%s|%s|%s|g%d", l.Repo, l.Provider, l.Project, l.TaskRef, l.Generation)
}

func needsRenew(l LeaseObservation, now time.Time, within time.Duration) bool {
	remaining := l.ExpiresAt.Sub(now)
	if remaining <= 0 {
		return false
	}
	if within > 0 {
		return remaining <= within
	}
	// Default: renew when less than half the original TTL remains, min 30s window.
	start := l.RenewedAt
	if start.IsZero() {
		start = l.ClaimedAt
	}
	ttl := l.ExpiresAt.Sub(start)
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	threshold := ttl / 2
	if threshold < 30*time.Second {
		threshold = 30 * time.Second
	}
	return remaining <= threshold
}

// CountActions tallies agent statuses and action kinds. Shared by JSON and human.
func CountActions(agents []AgentObservation, actions []Action) Counts {
	c := Counts{Agents: len(agents), Actions: len(actions)}
	for _, a := range agents {
		switch a.Status {
		case StatusHealthyIdle:
			c.HealthyIdle++
		case StatusBusy:
			c.Busy++
		case StatusBlocked:
			c.Blocked++
		case StatusDone:
			c.Done++
		case StatusStale:
			c.Stale++
		default:
			c.Unknown++
		}
	}
	for _, a := range actions {
		switch a.Kind {
		case ActionRenewLease:
			c.RenewLeases++
		case ActionConsumeCallback:
			c.ConsumeCallback++
		case ActionDispatch:
			c.Dispatch++
		case ActionOpenReview:
			c.OpenReview++
		case ActionReapLane:
			c.ReapLanes++
		case ActionWouldRun:
			c.WouldRun++
		case ActionReconcile:
			c.Reconcile++
		}
		if a.Applied {
			c.Applied++
		}
	}
	return c
}

// Apply executes Safe actions under an Act/ActSpawn snapshot. Observe mode is
// a no-op. Renewals pass the planned generation (never invent a newer one).
// Callback consumption is idempotent at the Actor boundary. Dispatch is
// refused when UnknownCritical or DispatchBlocked.
func Apply(ctx context.Context, snap Snapshot, actor Actor) (Snapshot, error) {
	if snap.Mode == ModeObserve {
		return snap, nil
	}
	if actor == nil {
		return snap, errors.New("pulse: actor is required for --act")
	}
	if err := ctx.Err(); err != nil {
		return snap, err
	}

	// Index original observations for apply payloads.
	leaseByTarget := make(map[string]LeaseObservation, len(snap.Observation.Leases))
	for _, l := range snap.Observation.Leases {
		leaseByTarget[leaseTarget(l)] = l
	}
	cbByTarget := make(map[string]CallbackObservation, len(snap.Observation.Callbacks))
	for _, cb := range snap.Observation.Callbacks {
		target := cb.EnvelopeID
		if target == "" {
			target = fmt.Sprintf("seq:%d:%s", cb.Sequence, cb.Ref)
		}
		cbByTarget[target] = cb
	}
	// FAC-221: index agents by both name and tab_id so Apply can resolve a
	// reap target back to the full observation with evidence fields.
	laneByTarget := make(map[string]AgentObservation, len(snap.Agents)*2)
	for _, a := range snap.Agents {
		if a.Name != "" {
			laneByTarget[a.Name] = a
		}
		if a.TabID != "" {
			laneByTarget[a.TabID] = a
		}
	}

	out := snap
	out.Actions = append([]Action(nil), snap.Actions...)
	hardErr := false

	for i := range out.Actions {
		a := &out.Actions[i]
		if !a.Safe || a.Kind == ActionWouldRun {
			continue
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var err error
		switch a.Kind {
		case ActionReconcile:
			err = actor.Reconcile(ctx)
		case ActionRenewLease:
			l, ok := leaseByTarget[a.Target]
			if !ok {
				err = fmt.Errorf("lease target %q not in observation", a.Target)
			} else if !l.Active {
				err = fmt.Errorf("refuse renew of non-active lease generation %d", l.Generation)
			} else {
				err = actor.RenewLease(ctx, l)
			}
		case ActionConsumeCallback:
			cb, ok := cbByTarget[a.Target]
			if !ok {
				// Idempotent: already gone is success.
				a.Applied = true
				continue
			}
			err = actor.ConsumeCallback(ctx, cb)
		case ActionDispatch:
			if out.UnknownCritical {
				err = fmt.Errorf("%w: %s", ErrUnknownCritical, strings.Join(out.UnknownReasons, "; "))
			} else if out.DispatchBlocked {
				err = fmt.Errorf("%w: %s", ErrDispatchBlocked, out.DispatchBlockReason)
			} else {
				err = actor.Dispatch(ctx, a.Target, a.Reason)
			}
		case ActionReapLane:
			lane, ok := laneByTarget[a.Target]
			if !ok {
				err = fmt.Errorf("reap target %q not in observation", a.Target)
			} else {
				err = actor.ReapLane(ctx, lane)
			}
		case ActionOpenReview:
			lane, ok := laneByTarget[a.Target]
			if !ok {
				err = fmt.Errorf("open_review target %q not in observation", a.Target)
			} else {
				err = actor.OpenReview(ctx, lane)
			}
		default:
			continue
		}
		if err != nil {
			a.ApplyError = err.Error()
			// Missing callback after race is not hard; generation fence errors,
			// dispatch, reap, reconcile, and open-review are.
			if a.Kind == ActionDispatch || a.Kind == ActionRenewLease || a.Kind == ActionReconcile || a.Kind == ActionReapLane || a.Kind == ActionOpenReview {
				hardErr = true
			}
			continue
		}
		a.Applied = true
	}

	out.Counts = CountActions(out.Agents, out.Actions)
	if out.UnknownCritical || hardErr {
		out.ExitCode = 1
	} else {
		out.ExitCode = 0
	}
	if hardErr {
		return out, fmt.Errorf("pulse: one or more act steps failed")
	}
	return out, nil
}

// FormatHuman renders the beat for operators. Counts match Snapshot.Counts.
func FormatHuman(snap Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== herd-pulse: verdict (mode: %s) ===\n", snap.Mode)
	if snap.Reason != "" {
		fmt.Fprintf(&b, "reason: %s\n", snap.Reason)
	}
	fmt.Fprintf(&b, "beat_sequence: %d observed_at: %s\n", snap.BeatSequence, snap.ObservedAt.UTC().Format(time.RFC3339Nano))
	c := snap.Counts
	fmt.Fprintf(&b, "counts: agents=%d healthy_idle=%d busy=%d blocked=%d done=%d stale=%d unknown=%d actions=%d renew_leases=%d consume_callbacks=%d dispatch=%d open_review=%d reap_lanes=%d would_run=%d reconcile=%d applied=%d\n",
		c.Agents, c.HealthyIdle, c.Busy, c.Blocked, c.Done, c.Stale, c.Unknown,
		c.Actions, c.RenewLeases, c.ConsumeCallback, c.Dispatch, c.OpenReview, c.ReapLanes, c.WouldRun, c.Reconcile, c.Applied)
	if snap.UnknownCritical {
		fmt.Fprintf(&b, "unknown_critical: true\n")
		for _, r := range snap.UnknownReasons {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}
	if snap.DispatchBlocked {
		fmt.Fprintf(&b, "dispatch_blocked: %s\n", snap.DispatchBlockReason)
	}
	if brokerObserved := snap.Observation.Broker.Known || snap.Observation.Broker.Error != "" || snap.Observation.Broker.Serving || snap.Observation.Broker.Socket != ""; brokerObserved {
		if snap.Observation.Broker.Serving {
			fmt.Fprintf(&b, "broker: serving (%s)\n", snap.Observation.Broker.Socket)
		} else {
			fmt.Fprintf(&b, "broker: UNAVAILABLE (%s)\n", snap.Observation.Broker.Error)
		}
	}
	fmt.Fprintf(&b, "agents:\n")
	if len(snap.Agents) == 0 {
		fmt.Fprintf(&b, "  (none)\n")
	} else {
		for _, a := range snap.Agents {
			fmt.Fprintf(&b, "  %-20s %s\n", a.Name, a.Status)
		}
	}
	fmt.Fprintf(&b, "actions:\n")
	if len(snap.Actions) == 0 {
		fmt.Fprintf(&b, "  (none)\n")
	} else {
		for _, a := range snap.Actions {
			line := fmt.Sprintf("  %3d %-18s %-24s %s", a.Sequence, a.Kind, a.Target, a.Reason)
			if a.WouldRun != "" {
				line += " would-run=" + a.WouldRun
			}
			if a.Applied {
				line += " [applied]"
			}
			if a.ApplyError != "" {
				line += " [error: " + a.ApplyError + "]"
			}
			fmt.Fprintln(&b, line)
		}
	}
	if snap.ExitCode != 0 {
		fmt.Fprintf(&b, "herd-pulse: BEAT ACTION REQUIRED (exit %d)\n", snap.ExitCode)
	} else {
		fmt.Fprintf(&b, "=== herd-pulse: complete (%d actions) ===\n", len(snap.Actions))
	}
	return b.String()
}

// FormatJSON encodes the snapshot. Counts in JSON equal FormatHuman counts.
func FormatJSON(snap Snapshot) ([]byte, error) {
	// Recompute counts so JSON cannot drift from the human renderer contract.
	snap.Counts = CountActions(snap.Agents, snap.Actions)
	return json.MarshalIndent(snap, "", "  ")
}

// Beat is Plan followed by optional Apply. It is the single entry for CLI and
// tests that want the full observe-or-act path over an already-read Observation.
func Beat(ctx context.Context, obs Observation, opts Options, actor Actor) (Snapshot, error) {
	snap, err := Plan(obs, opts)
	if err != nil {
		return Snapshot{}, err
	}
	if !opts.Act {
		return snap, nil
	}
	return Apply(ctx, snap, actor)
}
