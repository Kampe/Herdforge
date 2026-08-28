package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/kick"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/pulse"
	"github.com/Kampe/Herdforge/pkg/quotasup"
	"github.com/Kampe/Herdforge/pkg/review"
	"github.com/Kampe/Herdforge/pkg/reviewroot"
	"github.com/Kampe/Herdforge/pkg/standing"
	"github.com/Kampe/Herdforge/pkg/usage"
	"github.com/Kampe/Herdforge/pkg/winddown"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// runPulse is the FAC-73 coordinator heartbeat. Default is observe (read-only);
// --act applies bounded renewals and idempotent callback consumption; --spawn
// (with --act) may plan dispatch only when capacity is known and healthy.
func runPulse() {
	os.Exit(runPulseCommand(os.Args[2:], os.Stdout, os.Stderr))
}

func runPulseCommand(args []string, out, errOut *os.File) int {
	fs := flag.NewFlagSet("pulse", flag.ContinueOnError)
	fs.SetOutput(errOut)
	act := fs.Bool("act", false, "Apply bounded mutations (renew leases, consume callbacks, reconcile)")
	spawn := fs.Bool("spawn", false, "With --act, allow bounded dispatch when capacity is known")
	asJSON := fs.Bool("json", false, "Emit the beat snapshot as JSON")
	quiet := fs.Bool("quiet", false, "Suppress section noise (verdict still prints)")
	reason := fs.String("reason", "", "Reason recorded on the beat for replay/debugging")
	// Retained for compatibility with older claim-oriented invocations; the
	// claim sweep lives on daemon/forge via eng.RunPulse, not this heartbeat.
	_ = fs.String("role", "worker", "Deprecated for coordinator pulse; ignored")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	opts := pulse.Options{
		Act:    *act,
		Spawn:  *spawn,
		Reason: strings.TrimSpace(*reason),
		Now:    time.Now().UTC(),
	}
	if err := opts.Validate(); err != nil {
		fmt.Fprintln(errOut, err.Error())
		return 2
	}

	// FAC-93 / stop-cli AC: pulse is the canonical fleet-admission gate.
	// Missing, corrupt, or active wind-down must fail closed before any beat
	// work (observe or --act), matching every other claiming command.
	ctx := context.Background()
	if err := requireFleetAdmission(ctx); err != nil {
		fmt.Fprintf(errOut, "pulse: %v\n", err)
		return 1
	}

	obs, actor := gatherPulseObservation(ctx, opts.Act)
	snap, err := pulse.Beat(ctx, obs, opts, actor)
	if err != nil {
		// Apply may still return a partial snapshot with ExitCode set.
		if snap.BeatSequence != 0 {
			writePulseOutput(out, errOut, snap, *asJSON, *quiet)
			if snap.ExitCode != 0 {
				return snap.ExitCode
			}
		}
		fmt.Fprintf(errOut, "pulse: %v\n", err)
		return 1
	}
	writePulseOutput(out, errOut, snap, *asJSON, *quiet)
	return snap.ExitCode
}

func writePulseOutput(out, errOut *os.File, snap pulse.Snapshot, asJSON, quiet bool) {
	_ = quiet
	if asJSON {
		raw, err := pulse.FormatJSON(snap)
		if err != nil {
			fmt.Fprintf(errOut, "pulse: encode JSON: %v\n", err)
			return
		}
		fmt.Fprintln(out, string(raw))
		return
	}
	fmt.Fprint(out, pulse.FormatHuman(snap))
}

// gatherPulseObservation reads each source once. Errors become Known=false;
// they never become zero work or free capacity. Observe mode must not write
// callback-consumer state; --act uses the durable consumer for Drain+Ack.
func gatherPulseObservation(ctx context.Context, act bool) (pulse.Observation, pulse.Actor) {
	var obs pulse.Observation
	actor := &livePulseActor{}

	// Provider / queue pressure (one scoped ListTasks). Pulse only needs the
	// claimable queue for bounded dispatch; the old all-status hydration made
	// beat latency grow with total board size.
	providerObs, doneRefs := readPulseProvider(ctx)
	obs.Provider = providerObs
	obs.Broker = readPulseBroker()
	actor.dispatchRef = providerObs.NextTaskRef
	dispatchTaskID := providerObs.NextTaskID
	actor.dispatch = func(dispatchCtx context.Context, target, reason string) error {
		if strings.TrimSpace(providerObs.NextTaskRef) == "" {
			return errors.New("pulse: no claimable task ref available for bounded dispatch")
		}
		cfg, err := config.LoadConfig(".herd/herd.yaml")
		if err != nil {
			return fmt.Errorf("pulse: bounded dispatch lane identity: load config: %w", err)
		}
		registry, err := canonicalLaneRegistry(cfg)
		if err != nil {
			return fmt.Errorf("pulse: bounded dispatch lane identity: %w", err)
		}
		laneName, err := resolvePulseDispatchLane(registry, target)
		if err != nil {
			return fmt.Errorf("pulse: bounded dispatch lane identity: %w", err)
		}
		_, _, err = dispatchTicketDecision(dispatchCtx, dispatchRequest{
			TicketRef:    providerObs.NextTaskRef,
			TaskID:       dispatchTaskID,
			LaneName:     laneName,
			LaneExplicit: true,
		}, io.Discard)
		if err != nil {
			return fmt.Errorf("pulse: bounded dispatch %s: %w", providerObs.NextTaskRef, err)
		}
		return nil
	}

	// Herdr fleet (one AgentList) + reap evidence enrichment (FAC-218).
	// Evidence gathering fills CommittedWork, TicketDone, SafeRef,
	// AwaitingVerdict, TabGeneration, and TabRevision so the FAC-221 reap
	// planner fires on every idle-with-evidence state, not just ticket-done.
	obs.Herdr = readPulseHerdr(ctx, doneRefs)

	// Leases (one ActiveClaims when store present).
	leases, leaseActor, leaseErr := readPulseLeases(ctx)
	if leaseErr != nil {
		// Lease store unreadable is not free capacity; renewals simply have
		// nothing safe to plan. Surface via empty active set, not invent.
		obs.Leases = nil
	} else {
		obs.Leases = leases
		actor.leases = leaseActor
	}

	// Callbacks: observe peeks the inbox; act drains the durable consumer.
	cbs, cbActor, _ := readPulseCallbacks(ctx, act)
	obs.Callbacks = cbs
	actor.callbacks = cbActor

	// Review posture — best-effort; unknown blocks dispatch.
	obs.Review = readPulseReview()

	// Quota (one usage snapshot).
	obs.Quota = readPulseQuota()
	// FAC-674: worktree accumulation was invisible until someone ran a sweep, so
	// it reached 403 registrations before anyone looked. Surfacing it here makes
	// the leak visible on every beat instead of on remembering.
	obs.Worktrees = readPulseWorktrees()

	// Wind-down (one Read).
	obs.WindDown = readPulseWindDown(ctx)

	// Durable event reconcile: pending is detected only when a control mailbox
	// exists with unacked coordinator mail that is not a callback body.
	obs.NeedsReconcile = pulseNeedsReconcile()
	actor.reconcile = func(context.Context) error {
		// Bounded: control CoordinatorLoop needs an order source; without one
		// this is a no-op success so observe/act do not invent outbox rows.
		return nil
	}

	return obs, actor
}

func readPulseBroker() pulse.BrokerObservation {
	root, err := canonicalHerdRoot()
	if err != nil {
		return pulse.BrokerObservation{Known: true, Error: err.Error()}
	}
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		return pulse.BrokerObservation{Known: true, Error: err.Error()}
	}
	health := readBrokerHealth(root, cfg)
	return pulse.BrokerObservation{Known: true, Serving: health.Serving, Socket: health.Socket, Error: health.Detail}
}

// resolvePulseDispatchLane converts the live agent ID discovered by Herdr
// (forge-<lane>) into the configured lane name expected by dispatch. Keeping
// the namespaces explicit prevents dispatch from treating a live ID as a
// configured lane name and failing after selection.
func resolvePulseDispatchLane(registry lifecycle.CanonicalLaneRegistry, liveID string) (string, error) {
	lane, err := registry.ResolveLiveAgentID(liveID)
	if err != nil {
		return "", err
	}
	return lane.Name, nil
}

func readPulseProvider(ctx context.Context) (pulse.ProviderObservation, map[string]bool) {
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		return pulse.ProviderObservation{Known: false, Error: err.Error()}, nil
	}
	tp, err := loadTaskProvider(cfg)
	if err != nil {
		return pulse.ProviderObservation{Known: false, Error: err.Error()}, nil
	}
	project := strings.TrimSpace(cfg.TaskProvider.ProjectID)
	tasks, err := tp.ListTasks(ctx, project, provider.StatusToDo)
	if err != nil {
		return pulse.ProviderObservation{Known: false, Error: err.Error()}, nil
	}
	var claimable, inProgress int64
	doneRefs := make(map[string]bool)
	claimableTasks := make([]*provider.Task, 0)
	for _, t := range tasks {
		if t == nil {
			continue
		}
		switch t.Status {
		case provider.StatusToDo, "":
			claimable++
			claimableTasks = append(claimableTasks, t)
		case provider.StatusInProgress:
			inProgress++
		case provider.StatusDone:
			ref := strings.ToUpper(strings.TrimSpace(t.Ref))
			if ref != "" {
				doneRefs[ref] = true
			}
		}
	}
	obs := pulse.ProviderObservation{
		Known:      true,
		QueueDepth: int64(len(tasks)),
		Claimable:  claimable,
		InProgress: inProgress,
	}
	if next := selectPulseDispatchTask(claimableTasks); next != nil {
		obs.NextTaskRef = strings.TrimSpace(next.Ref)
		obs.NextTaskID = strings.TrimSpace(next.ID)
	}
	return obs, doneRefs
}

// selectPulseDispatchTask keeps pulse's one-dispatch bound deterministic and
// aligned with the fleet task-selection contract: priority descending, then
// ticket reference ascending. Nil and ref-less tasks cannot be dispatched.
func selectPulseDispatchTask(tasks []*provider.Task) *provider.Task {
	priority := func(p provider.Priority) int {
		switch p {
		case provider.PriorityUrgent:
			return 4
		case provider.PriorityHigh:
			return 3
		case provider.PriorityMedium:
			return 2
		case provider.PriorityLow:
			return 1
		default:
			return 0
		}
	}
	var best *provider.Task
	for _, task := range tasks {
		if task == nil || (task.Status != "" && task.Status != provider.StatusToDo) || strings.TrimSpace(task.Ref) == "" {
			continue
		}
		if best == nil || priority(task.Priority) > priority(best.Priority) ||
			(priority(task.Priority) == priority(best.Priority) && strings.TrimSpace(task.Ref) < strings.TrimSpace(best.Ref)) {
			best = task
		}
	}
	return best
}

// readPulseHerdr reads the live fleet and enriches each agent with reap
// evidence (FAC-218). The evidence fields — CommittedWork, TicketDone,
// SafeRef, AwaitingVerdict, TabGeneration, TabRevision — drive the FAC-221
// reap planner so it fires on every idle-with-evidence state, not just
// ticket-done. Without enrichment the planner never sees exit evidence and
// idle lanes sit resident indefinitely.
func readPulseHerdr(ctx context.Context, doneRefs map[string]bool) pulse.HerdrObservation {
	agents, err := herdr.AgentList()
	if err != nil {
		return pulse.HerdrObservation{Known: false, Error: err.Error()}
	}
	// AgentList is intentionally fleet-wide in Herdr. Pulse must scope that
	// inventory to this repository's configured workspace before deriving
	// capacity or reap evidence; otherwise a focused workspace from another
	// checkout (for example Chainseer) is treated as Herdforge's fleet.
	if workspace := pulseHerdrWorkspace(); workspace != "" {
		scoped := filterPulseAgentsWorkspace(agents, workspace)
		// FAC-649: a scope that matches NOTHING against a non-empty fleet is a
		// wrong scope, not an empty fleet.
		//
		// This filter reported a confident agents=0 whenever the configured
		// workspace did not exist. Measured live on the review host: running pulse
		// without HERD_CONFIG_PATH resolves .herd/herd.yaml, which registers the
		// COORDINATOR's workspace (wB) -- a workspace that host has never had. The
		// filter dropped all 7 live reviewers and pulse answered agents=0,
		// capacity=0, reviews_in_flight=0. Setting the correct config path turned
		// the same command into agents=7 on the same beat.
		//
		// The cost was not a wrong number. An orchestrator read that zero as "the
		// review host cannot spawn", stopped dispatching, closed finished panes
		// without replacing them, and reported a control-plane outage for an hour
		// while eight reviewers were running the whole time.
		//
		// An honestly empty fleet still reports zero: the guard only fires when
		// Herdr returned agents and this scope excluded every one of them.
		if len(scoped) == 0 && len(agents) > 0 {
			live := map[string]int{}
			for _, a := range agents {
				live[strings.TrimSpace(a.Workspace)]++
			}
			return pulse.HerdrObservation{
				Known: false,
				Error: fmt.Sprintf("configured workspace %q holds none of the %d live agents (live workspaces: %v); "+
					"this is a wrong scope, not an idle fleet -- check fleet.herdr_workspace for THIS host "+
					"(HERD_CONFIG_PATH may be selecting another host's profile)",
					workspace, len(agents), live),
			}
		}
		agents = scoped
	}
	ev := loadReapEvidence(ctx, agents, doneRefs)
	out := make([]pulse.AgentObservation, 0, len(agents))
	for _, a := range agents {
		paneBody := ""
		if a.PaneID != "" {
			if body, readErr := herdr.PaneRead(a.PaneID, 40); readErr == nil {
				paneBody = body
			}
		}
		processName := ""
		if a.PaneID != "" {
			if processes, processErr := herdr.PaneProcessInfo(a.PaneID); processErr == nil && len(processes) > 0 {
				processName = processes[0].Name
			}
		}
		explainTarget := a.Name
		if strings.TrimSpace(explainTarget) == "" {
			explainTarget = a.PaneID
		}
		explain, explainErr := herdr.ExplainAgent(explainTarget)
		agent := pulse.AgentObservation{
			Name: a.Name,
			Raw:  a.Status,
			// FAC-614: pane-AWARE classification. readPulseHerdr already pays
			// for the pane read above; classifying without it is what made a
			// paused goal indistinguishable from work in progress. An
			// independent review FAILed the first version of this change for
			// exactly this -- the policy existed and the beat never used it.
			Status: pulse.ClassifyStatusWithPane(a.Status, false, paneBody),
			// FAC-614: the harness's own transition counter. Resume escalation
			// measures PROGRESS against this, not elapsed time -- a lane can be
			// legitimately slow without being stuck.
			StateSeq:          safeStateSeq(a.StateChangeSeq),
			PaneID:            a.PaneID,
			PaneState:         a.Status,
			ForegroundProcess: processName,
			// FAC-418: carry the harness kind. Idle detection is not equally
			// trustworthy across harnesses, and the reap decision needs to know
			// which one it is looking at.
			Kind:          a.Kind,
			TabID:         a.TabID,
			Workspace:     a.Workspace,
			Worktree:      a.Cwd,
			TabGeneration: a.TabGeneration,
			TabRevision:   a.Revision,
		}
		if warning := herdr.DetectContextWarning(paneBody); warning != "" {
			agent.ContextWarning = warning
		}
		if explainErr != nil {
			agent.LastError = explainErr.Error()
			agent.PacketPending = true
		} else {
			agent.PaneState = explain.State
			agent.LastError = strings.TrimSpace(explain.Warning)
			if agent.LastError == "" {
				agent.LastError = strings.TrimSpace(explain.FallbackReason)
			}
			if a.Status == "done" {
				agent.ExitReason = "agent reported done"
			} else if explain.State == "blocked" || explain.VisibleBlocker {
				agent.ExitReason = "agent detector reports blocked"
			} else if explain.State == "unknown" {
				agent.ExitReason = "pane state unknown"
			}
			// If neither visible idle nor visible working is proven, the pane
			// may still hold an unconsumed composer or goal. Keep it resident.
			agent.PacketPending = packetPendingFromExplain(explain)
		}
		ref := taskRefFromAgentName(a.Name)
		agent = applyReapEvidence(agent, ref, ev)
		out = append(out, agent)
	}
	return pulse.HerdrObservation{Known: true, Agents: out}
}

func packetPendingFromExplain(explain herdr.AgentExplain) bool {
	return !explain.VisibleIdle && !explain.VisibleWorking
}

func pulseHerdrWorkspace() string {
	if cfg, err := config.LoadConfig(".herd/herd.yaml"); err == nil {
		if ws := strings.TrimSpace(cfg.Fleet.HerdrWorkspace); ws != "" {
			return ws
		}
		if entries, listErr := herdr.WorkspaceList(); listErr == nil {
			if root, rootErr := os.Getwd(); rootErr == nil {
				if id, ok := herdr.PickWorkspaceStrict(entries, filepath.Base(root)); ok {
					return id
				}
			}
		}
	}
	return strings.TrimSpace(os.Getenv("HERD_WORKSPACE"))
}

func filterPulseAgentsWorkspace(agents []herdr.AgentEntry, workspace string) []herdr.AgentEntry {
	filtered := make([]herdr.AgentEntry, 0, len(agents))
	for _, agent := range agents {
		if strings.TrimSpace(agent.Workspace) == workspace {
			filtered = append(filtered, agent)
		}
	}
	return filtered
}

func leaseDBPath() string {
	// HERD_LEASE_DB is a test/override hook only. Production claims write
	// deps.DefaultLaunchLeasePath() (.herd/launch-claims.db) via daemon/dispatch
	// OpenLeaseOwnership — pulse must renew against that same store.
	if p := strings.TrimSpace(os.Getenv("HERD_LEASE_DB")); p != "" {
		return p
	}
	return deps.DefaultLaunchLeasePath()
}

func readPulseLeases(ctx context.Context) ([]pulse.LeaseObservation, *claim.ClaimManager, error) {
	path := leaseDBPath()
	if _, err := os.Stat(path); err != nil {
		// No store yet: known empty, not unknown.
		return nil, nil, nil
	}
	store, err := claim.NewSQLiteLeaseStore(path)
	if err != nil {
		return nil, nil, err
	}
	// ClaimManager.Renew does not require hold reader; Release does.
	mgr := claim.NewClaimManager(store)
	active, err := mgr.ActiveClaims(ctx)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	out := make([]pulse.LeaseObservation, 0, len(active))
	for _, l := range active {
		if l == nil {
			continue
		}
		out = append(out, pulse.LeaseObservation{
			Repo:       l.Repo,
			Provider:   l.Provider,
			Project:    l.Project,
			TaskRef:    l.TaskRef,
			OwnerID:    l.OwnerID,
			HoldLane:   l.HoldLane,
			Generation: l.Generation,
			Held:       l.Held,
			ExpiresAt:  l.ExpiresAt,
			ClaimedAt:  l.ClaimedAt,
			RenewedAt:  l.RenewedAt,
			Active:     l.Status == claim.StatusActive,
		})
	}
	return out, mgr, nil
}

func pulseMailPath() string {
	return mail.CallbackMailPath(".")
}

func readPulseCallbacks(ctx context.Context, act bool) ([]pulse.CallbackObservation, *mail.CallbackConsumer, error) {
	path := pulseMailPath()
	if _, err := os.Stat(path); err != nil {
		return nil, nil, nil
	}
	mb := mail.NewMailbox(path)
	if !act {
		// Observe: parse inbox without durable consumer mutation.
		cbs, err := mb.DrainCallbacksContext(ctx)
		if err != nil {
			return nil, nil, err
		}
		out := make([]pulse.CallbackObservation, 0, len(cbs))
		for i, cb := range cbs {
			out = append(out, pulse.CallbackObservation{
				EnvelopeID:      fmt.Sprintf("peek:%d:%s", i, cb.Ref),
				Sequence:        cb.Sequence,
				Ref:             cb.Ref,
				Kind:            string(cb.Kind),
				LeaseGeneration: cb.LeaseGeneration,
			})
		}
		return out, nil, nil
	}
	cons, err := mail.NewCallbackConsumer(mb, 0)
	if err != nil {
		return nil, nil, err
	}
	drained, err := cons.DrainContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]pulse.CallbackObservation, 0, len(drained))
	for _, d := range drained {
		out = append(out, pulse.CallbackObservation{
			EnvelopeID:      d.EnvelopeID,
			Sequence:        d.Sequence,
			Ref:             d.Callback.Ref,
			Kind:            string(d.Callback.Kind),
			LeaseGeneration: d.Callback.LeaseGeneration,
			Attempt:         d.Attempt,
		})
	}
	return out, cons, nil
}

func readPulseReview() pulse.ReviewObservation {
	// Full drain scan is owned by `herd drain`. Pulse only needs a known
	// posture bit; when the ledger is absent, review is known-empty rather
	// than free capacity for launches that require review headroom. RawVetoed
	// is the ledger's unfiltered vetoed-SHA set and must not be conflated with
	// drain's live unmerged-candidate NeedReview count.
	path := drainLedgerPath()
	if _, err := os.Stat(path); err != nil {
		// FAC-643: an absent ledger does not mean an empty INBOX. Verdicts land
		// in the inbox before anything admits them to a ledger, so returning
		// early here reported inbox_uningested=0 next to a directory holding 123
		// real verdict files -- the backlog FAC-624 added this field to surface.
		// Sweep anyway and report what is actually waiting.
		obs := pulse.ReviewObservation{Known: true, Pending: 0, RawVetoed: 0}
		if found, sweepErr := sweepUningestedArtifacts(reviewroot.Resolve(".").Root, path); sweepErr == nil {
			obs.InboxUningested = len(found)
		}
		return obs
	}
	// Ledger exists — verify it is readable. A corrupt ledger must surface
	// as Known=false so an operator can detect the problem, not silently
	// report Known=true with zero pending (finding 3).
	ledger := review.OpenLedger(path)
	ctx := context.Background()
	pending, err := ledger.Pending()
	if err != nil {
		return pulse.ReviewObservation{Known: false, Error: fmt.Sprintf("review ledger unreadable: %v", err)}
	}
	vetoed, err := ledger.Vetoed(ctx)
	if err != nil {
		return pulse.ReviewObservation{Known: false, Error: fmt.Sprintf("review ledger unreadable: %v", err)}
	}
	pendingRefs := make([]string, 0, len(pending))
	for _, row := range pending {
		if strings.TrimSpace(row.SHA) != "" {
			pendingRefs = append(pendingRefs, row.SHA)
		}
	}
	needReviewRefs := make([]string, 0, len(vetoed))
	for sha := range vetoed {
		if strings.TrimSpace(sha) != "" {
			needReviewRefs = append(needReviewRefs, sha)
		}
	}
	sort.Strings(pendingRefs)
	sort.Strings(needReviewRefs)
	cap := drainIntEnv("HERD_IN_REVIEW_CAP", 8)
	// FAC-624: surface the inbox backlog alongside the ledger posture. Pending
	// alone reported 0 with 88 verdicts waiting to be admitted.
	uningested := 0
	if found, sweepErr := sweepUningestedArtifacts(reviewroot.Resolve(".").Root, path); sweepErr == nil {
		uningested = len(found)
	}
	return pulse.ReviewObservation{
		Known: true, Pending: len(pendingRefs), PendingRefs: pendingRefs,
		InboxUningested: uningested,
		RawVetoed:       len(needReviewRefs), RawVetoedRefs: needReviewRefs,
		// FAC-650: Cap is carried so the block decision can compare it against
		// LIVE review concurrency. Saturated stays for compatibility but is no
		// longer what gates dispatch: pending+raw_vetoed counted a historical
		// vetoed set, so old FAIL/BLOCKED candidates saturated the fleet forever.
		Cap:       cap,
		Saturated: len(pendingRefs)+len(needReviewRefs) >= cap,
	}
}

// readPulseWorktrees classifies the worktree population using the SAME logic
// worktree-reap uses, so pulse and the reaper can never disagree about what is
// reclaimable (FAC-674). A second classifier would be a second definition of one
// decision, which is the defect pkg/invariant exists to catch.
func readPulseWorktrees() pulse.WorktreeObservation {
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	entries, err := listWorktreeEntries(root)
	if err != nil {
		return pulse.WorktreeObservation{Known: false, Error: err.Error()}
	}
	out := pulse.WorktreeObservation{Known: true, Total: len(entries)}
	for _, e := range entries {
		switch {
		case e.IsMain:
			// The repository's own checkout is not a task surface.
		case e.Detached:
			out.Detached++
		case e.Locked:
			out.Locked++
		case e.Branch == "":
			out.Unknown++
		case e.Dirty:
			out.Dirty++
		case isResidentHome(e.Branch, e.Path):
			out.ResidentHome++
		default:
			switch ahead := commitsAhead(root, "origin/main", e.Branch); {
			case ahead < 0:
				out.Unknown++
			case ahead == 0:
				out.Landed++
			default:
				out.Unmerged++
			}
		}
	}
	return out.Summarize()
}

func readPulseQuota() pulse.QuotaObservation {
	snap, err := usage.FetchSnapshot()
	if err != nil {
		return pulse.QuotaObservation{Known: false, Error: err.Error()}
	}
	eng := usage.NewQuotaEngine()
	computed := eng.ComputeAll(snap)
	// Quota is a routing constraint, not an AND over every provider. One
	// exhausted surface must not block a healthy sibling (for example Codex at
	// its weekly limit while Grok remains available). Dispatch is exhausted
	// only when every native provider surface is exhausted or unavailable.
	exhausted, atRisk := true, false
	knownSurface := false
	for _, provider := range []string{"codex", "grok", "claude", "agy"} {
		st, ok := computed[provider]
		if !ok {
			continue
		}
		knownSurface = true
		if st.Available {
			exhausted = false
		}
		switch quotasup.Classify(&st, quotasup.DefaultWarnRunwayMinutes) {
		case quotasup.AtRisk:
			atRisk = true
		}
	}
	if !knownSurface {
		exhausted = false
	}
	return pulse.QuotaObservation{Known: true, Exhausted: exhausted, AtRisk: atRisk}
}

func readPulseWindDown(ctx context.Context) pulse.WindDownObservation {
	path := winddownStatePath()
	a, err := winddown.New(path, nil)
	if err != nil {
		return pulse.WindDownObservation{Known: false, Error: err.Error()}
	}
	st, err := a.Read(ctx)
	if err != nil {
		if errors.Is(err, winddown.ErrStateMissing) {
			// Missing file: not in wind-down (known off).
			return pulse.WindDownObservation{Known: true, Enabled: false}
		}
		return pulse.WindDownObservation{Known: false, Error: err.Error()}
	}
	return pulse.WindDownObservation{
		Known:      true,
		Enabled:    st.Enabled,
		Generation: st.Generation,
	}
}

func pulseNeedsReconcile() bool {
	// Detect unacked non-callback coordinator mail as "durable events pending".
	// Callbacks are handled by the callback consumer path.
	path := pulseMailPath()
	if _, err := os.Stat(path); err != nil {
		return false
	}
	mb := mail.NewMailbox(path)
	envs, err := mb.ReadInbox(mail.CoordinatorInbox)
	if err != nil {
		return false
	}
	for _, e := range envs {
		if e == nil {
			continue
		}
		subj := strings.ToLower(e.Subject)
		if strings.HasPrefix(subj, "complete:") || strings.HasPrefix(subj, "blocked:") {
			continue
		}
		if e.Subject != "" {
			return true
		}
	}
	return false
}

// livePulseActor applies renewals and callback acks against real stores.
type livePulseActor struct {
	leases      *claim.ClaimManager
	callbacks   *mail.CallbackConsumer
	reconcile   func(context.Context) error
	dispatch    func(context.Context, string, string) error
	dispatchRef string
}

func (a *livePulseActor) Reconcile(ctx context.Context) error {
	if a.reconcile == nil {
		return nil
	}
	return a.reconcile(ctx)
}

func (a *livePulseActor) RenewLease(ctx context.Context, l pulse.LeaseObservation) error {
	if a.leases == nil {
		return errors.New("pulse: lease store not available")
	}
	if !l.Active || l.Generation <= 0 {
		return fmt.Errorf("pulse: refuse renew of non-current generation %d", l.Generation)
	}
	key := claim.LeaseKey{
		Repo:     l.Repo,
		Provider: l.Provider,
		Project:  l.Project,
		TaskRef:  l.TaskRef,
	}
	_, err := a.leases.Renew(ctx, key, l.OwnerID, l.Generation)
	return err
}

func (a *livePulseActor) ConsumeCallback(ctx context.Context, cb pulse.CallbackObservation) error {
	if a.callbacks == nil {
		return errors.New("pulse: callback consumer not available")
	}
	// Ack is idempotent for already-acked envelope IDs.
	return a.callbacks.AckContext(ctx, cb.EnvelopeID)
}

func (a *livePulseActor) Dispatch(ctx context.Context, target, reason string) error {
	if a.dispatch == nil {
		return errors.New("pulse: bounded dispatch adapter unavailable")
	}
	if strings.TrimSpace(a.dispatchRef) == "" {
		return errors.New("pulse: no claimable task ref available for bounded dispatch")
	}
	return a.dispatch(ctx, target, reason)
}

func (a *livePulseActor) ReapLane(ctx context.Context, lane pulse.AgentObservation) error {
	if strings.TrimSpace(lane.TabID) == "" {
		return fmt.Errorf("pulse: reap requires tab_id; lane %q has none", lane.Name)
	}
	if strings.TrimSpace(lane.Workspace) == "" {
		return fmt.Errorf("pulse: reap requires workspace; lane %q tab %s has none", lane.Name, lane.TabID)
	}
	if lane.TabGeneration == 0 {
		return fmt.Errorf("pulse: reap requires tab generation evidence; lane %q tab %s has none (FAC-180 compare-and-close fence)", lane.Name, lane.TabID)
	}
	nonce, err := reapNonce(lane.TabID)
	if err != nil {
		return fmt.Errorf("pulse: reap nonce: %w", err)
	}
	var paneIDs []string
	if strings.TrimSpace(lane.PaneID) != "" {
		paneIDs = []string{lane.PaneID}
	}
	req := herdr.CompareAndCloseRequest{
		WorkspaceID:   lane.Workspace,
		TabID:         lane.TabID,
		TabGeneration: lane.TabGeneration,
		TabRevision:   lane.TabRevision,
		PaneIDs:       paneIDs,
		Nonce:         nonce,
	}
	receipt, err := herdr.CompareAndCloseTab(req)
	if err != nil {
		return fmt.Errorf("pulse: reap close %s: %w", lane.TabID, err)
	}
	switch receipt.Outcome {
	case herdr.OutcomeClosed, herdr.OutcomeReplayed, herdr.OutcomeAlreadyClosed:
		if !receipt.ResultingAbsence {
			return fmt.Errorf("pulse: reap close %s: outcome %s without resulting absence", lane.TabID, receipt.Outcome)
		}
		return nil
	default:
		return fmt.Errorf("pulse: reap close %s: %s", lane.TabID, receipt.Outcome)
	}
}

// reapNonce generates a durable idempotency nonce for a pulse reap close.
func reapNonce(tabID string) (string, error) {
	b := make([]byte, 8)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	return "pulse-reap-" + tabID + "-" + hex.EncodeToString(b), nil
}

// taskRefFromAgentName extracts the ticket ref from a task-lane agent name.
// Task lanes are named "task-<lowercased-ref>" (e.g. "task-fac-218" ->
// "FAC-218"). Returns "" for standing lanes that don't carry a task ref.
func taskRefFromAgentName(name string) string {
	name = strings.TrimSpace(name)
	lower := strings.ToLower(name)
	if !strings.HasPrefix(lower, "task-") {
		return ""
	}
	return strings.ToUpper(strings.TrimPrefix(lower, "task-"))
}

// reapEvidence holds pre-loaded evidence for enriching pulse agent
// observations. Each map is nil-safe: a nil map means "no evidence
// available" and the corresponding field is left unset (fail closed —
// the lane is not reaped without proof).
//
// ledgerCorrupt is the fail-closed KEEP signal: when the review ledger
// file exists but cannot be read (corrupted JSONL, disk error), we cannot
// know whether a FAIL/BLOCKED verdict is pending. Any agent with committed
// work is treated as AwaitingVerdict so the reap planner does not destroy
// a live lane whose verdict it cannot read. This closes the asymmetry:
// nil exit-evidence maps correctly cause fail-closed (no reap), but nil
// vetoedSHAs alone caused fail-open (lost KEEP signal → reap).
type reapEvidence struct {
	doneRefs      map[string]bool   // uppercased task refs with done status
	safeRefs      map[string]string // uppercased task ref -> safe ref name
	vetoedSHAs    map[string]bool   // SHAs with FAIL/BLOCKED verdict
	headSHAs      map[string]string // agent name -> HEAD SHA
	committed     map[string]bool   // agent name -> has committed work
	ledgerCorrupt bool              // ledger exists but unreadable — verdict may be pending
}

// applyReapEvidence fills in CommittedWork, TicketDone, SafeRef, and
// AwaitingVerdict on one agent observation from pre-loaded evidence.
// When ledgerCorrupt is set, any agent with committed work is marked
// AwaitingVerdict — a verdict may be pending that cannot be read, so
// the reap planner must KEEP the lane. This is a pure function — no
// I/O — so it is directly unit-testable.
func applyReapEvidence(agent pulse.AgentObservation, ref string, ev reapEvidence) pulse.AgentObservation {
	if ref != "" {
		if ev.doneRefs != nil && ev.doneRefs[ref] {
			agent.TicketDone = true
		}
		if sr, ok := ev.safeRefs[ref]; ok && strings.TrimSpace(sr) != "" {
			agent.SafeRef = sr
		}
	}
	if ev.committed != nil && ev.committed[agent.Name] {
		agent.CommittedWork = true
	}
	if ev.ledgerCorrupt && agent.CommittedWork {
		agent.AwaitingVerdict = true
	}
	if ev.vetoedSHAs != nil && ev.headSHAs != nil {
		if sha, ok := ev.headSHAs[agent.Name]; ok && sha != "" && ev.vetoedSHAs[sha] {
			agent.AwaitingVerdict = true
		}
	}
	return agent
}

// loadReapEvidence gathers evidence from worktree, board, and review sources.
// It runs git in each agent's Cwd to check for committed work, safe refs, and
// HEAD SHAs, and reads the review ledger for vetoed SHAs. Any source error
// leaves the corresponding map nil (fail closed) rather than inventing
// evidence. A ledger read error sets ledgerCorrupt so applyReapEvidence can
// set AwaitingVerdict on committed lanes — a verdict may be pending that
// cannot be read, and the lane must not be reaped.
func loadReapEvidence(ctx context.Context, entries []herdr.AgentEntry, doneRefs map[string]bool) reapEvidence {
	ev := reapEvidence{doneRefs: doneRefs}

	ledgerPath := drainLedgerPath()
	if _, err := os.Stat(ledgerPath); err == nil {
		ledger := review.OpenLedger(ledgerPath)
		vetoed, err := ledger.Vetoed(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pulse: review ledger unreadable at %s: %v — treating all committed lanes as awaiting verdict (fail-closed KEEP)\n", ledgerPath, err)
			ev.ledgerCorrupt = true
		} else {
			ev.vetoedSHAs = vetoed
		}
	}

	ev.safeRefs = make(map[string]string)
	ev.headSHAs = make(map[string]string)
	ev.committed = make(map[string]bool)

	for _, a := range entries {
		cwd := strings.TrimSpace(a.Cwd)
		if cwd == "" {
			continue
		}
		ref := taskRefFromAgentName(a.Name)
		if ref != "" {
			safeRefName := worktree.SafeRefFor(ref)
			if sha, err := gitRevParse(cwd, safeRefName); err == nil && sha != "" {
				ev.safeRefs[ref] = safeRefName
			}
		}
		if sha, err := gitRevParse(cwd, "HEAD"); err == nil && sha != "" {
			ev.headSHAs[a.Name] = sha
		}
		if hasCommittedWork(cwd) {
			ev.committed[a.Name] = true
		}
	}
	return ev
}

// gitRevParse runs `git rev-parse --verify --quiet <ref>` in cwd and returns
// the trimmed SHA. An empty string with nil error means the ref does not exist.
func gitRevParse(cwd, ref string) (string, error) {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--verify", "--quiet", ref).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// hasCommittedWork reports whether the worktree at cwd holds work that is
// genuinely not on origin/main -- not merely absent from its ancestry.
//
// FAC-576: this used `git log origin/main..HEAD`, which is ancestry-only. A
// rebase-merge rewrites the SHA, so landed work is never an ancestor and the
// lane looked unlanded forever. Pulse re-emitted a candidate that had already
// been reviewed and merged as an equivalent patch, and `herd candidate` was
// meanwhile reporting it already-on-main -- the same question answered two ways.
//
// Patch-equivalence now comes from harvest.UnlandedSubjects, the single
// definition. Git errors fail closed: no evidence of unlanded work.
func hasCommittedWork(cwd string) bool {
	subjects, err := harvest.UnlandedSubjects(context.Background(), cwd, "origin/main")
	if err != nil {
		return false
	}
	return len(harvest.SubstantiveSubjects(subjects)) > 0
}

func (a *livePulseActor) OpenReview(ctx context.Context, lane pulse.AgentObservation) error {
	if strings.TrimSpace(lane.TabID) == "" {
		return fmt.Errorf("pulse: open_review requires tab_id; lane %q has none", lane.Name)
	}
	// Pulse observes a lane but intentionally does not invent a candidate SHA.
	// Hand the exact tab identity to the standing review supervisor, which owns
	// candidate discovery, receipt admission, reviewer dispatch, and retries.
	cfg, err := config.LoadConfig(filepath.Join(".herd", "herd.yaml"))
	if err != nil {
		return fmt.Errorf("pulse: load config for review supervisor: %w", err)
	}
	supervisor := findReviewSupervisorLane(cfg)
	if supervisor == nil {
		return errors.New("pulse: no standing review supervisor configured")
	}
	if !herdr.IsAvailable() {
		return errors.New("pulse: herdr CLI not found")
	}
	// FAC-547: resolve to the agent that actually exists. A fleet whose
	// supervisor predates repo-qualified naming holds the legacy unqualified
	// name, and targeting the qualified form sent every review handoff to a
	// name no agent held.
	target := standing.AgentNameForRepository(supervisor.Name, repositoryIdentityForLaunch(cfg))
	// FAC-597: a census that CANNOT be read and a census that does not hold this
	// lane are different facts. A list error is unknown, so keep the qualified
	// name and let delivery report its own failure. A successful list that lacks
	// the supervisor is MISSING: addressing it anyway spends a review slot on a
	// reviewer told to report to an agent nobody can name.
	if live, listErr := herdrStandingAgents(); listErr == nil {
		name, isLive := standing.LiveAgentName(live, supervisor.Name, repositoryIdentityForLaunch(cfg))
		if !isLive {
			return fmt.Errorf("pulse: review supervisor lane %q has no live agent (looked for %q across %d live standing agent(s)); raise or rebind it before opening a review",
				supervisor.Name, name, len(live))
		}
		target = name
	}
	// Resolve exact identity here, from git, rather than asking the receiver to
	// re-derive it from a tab id.
	commits, resolveErr := harvest.UnlandedCommits(context.Background(), lane.Worktree, "origin/main")
	substantive := harvest.SubstantiveCommits(commits)
	packet := pulseReviewPacket(lane, substantive, resolveErr)

	// FAC-567: queue DURABLY first, then nudge the pane.
	//
	// Sequential pane delivery let a later handoff supersede an earlier one, so
	// two real queues (29 API commits and 2 DeFi) could not both be delivered.
	// Consumption is not retention: text that reached a pane can still be
	// displaced by the next thing written there. The durable entry is the
	// delivery; the pane send is a nudge on top of a record that already exists.
	subject := fmt.Sprintf("PULSE REVIEW HANDOFF %s (%d candidate(s))", lane.Name, len(substantive))
	id := handoffEnvelopeID(lane.Name, substantive)
	queued, qErr := enqueueReviewHandoff(context.Background(),
		canonicalHandoffMailbox(), "pulse", target, subject, packet, id)
	if qErr != nil {
		// Refuse rather than deliver something that can be lost silently.
		return fmt.Errorf("pulse: %w", qErr)
	}
	if !queued {
		// This exact handoff is already pending. Re-nudging is safe and helps a
		// starved supervisor, but it must not be reported as new work.
		fmt.Fprintf(os.Stderr, "pulse: handoff for %s already pending (%s); re-nudging only\n", lane.Name, id)
	}

	// Nudge is best-effort BECAUSE the durable record exists. A failed nudge is
	// reported, never fatal: failing here would discard a handoff that is
	// already safely queued.
	if _, err := herdr.Send(target, packet, true, 30*time.Second); err != nil {
		fmt.Fprintf(os.Stderr,
			"pulse: durably queued handoff for %s but pane nudge failed (%v); supervisor drains its inbox at next idle\n",
			target, err)
	}
	_ = ctx
	return nil
}

// herdrStandingAgents lists live agents in the standing shape used for
// identity resolution. A listing failure is not fatal to the caller: it falls
// back to the qualified name, which is the pre-FAC-547 behaviour.
func herdrStandingAgents() ([]standing.Agent, error) {
	raw, err := herdr.AgentList()
	if err != nil {
		return nil, err
	}
	out := make([]standing.Agent, 0, len(raw))
	for _, a := range raw {
		out = append(out, standing.Agent{
			Name: a.Name, Status: a.Status, PaneID: a.PaneID,
			TabID: a.TabID, Workspace: a.Workspace, Cwd: a.Cwd,
			Model: a.Model, LaunchModel: a.LaunchModel,
		})
	}
	return out, nil
}

// pulseReviewPacket builds the handoff body.
//
// FAC-566: this used to carry ONLY lane, tab and workspace. A receiver could not
// confirm the handoff was still valid, and a lane holding several unlanded
// commits collapsed into one ambiguous assertion -- one observed lane had 29
// unlanded commits against 8 already patch-equivalent, which a tab-level handoff
// cannot express. A stale resident transcript in the target pane then made the
// wrong candidate look plausible.
//
// The exact unlanded commits are named. When none can be resolved the packet
// SAYS SO rather than implying a candidate exists: an unverifiable handoff must
// not read like a verified one.
func pulseReviewPacket(lane pulse.AgentObservation, commits []harvest.UnlandedCommit, resolveErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PULSE REVIEW HANDOFF\nLane: %s\nTab: %s\nWorkspace: %s\nWorktree: %s\n",
		lane.Name, lane.TabID, lane.Workspace, lane.Worktree)

	switch {
	case resolveErr != nil:
		fmt.Fprintf(&b, "\nCANDIDATES: UNRESOLVED — %v\n", resolveErr)
		b.WriteString("Do not treat this as a candidate assertion. Resolve identity in the worktree before admitting anything.\n")
	case len(commits) == 0:
		b.WriteString("\nCANDIDATES: NONE unlanded (every commit is already on origin/main by patch identity).\n")
		b.WriteString("This handoff carries no reviewable work; do not open a review for it.\n")
	default:
		fmt.Fprintf(&b, "\nCANDIDATES: %d commit(s) genuinely not on origin/main (patch-equivalent commits excluded):\n", len(commits))
		for _, c := range commits {
			fmt.Fprintf(&b, "  %s  %s\n", c.SHA, c.Subject)
		}
		if len(commits) > 1 {
			b.WriteString("\nEach needs its own receipt. Do NOT treat the branch tip as one harvestable candidate.\n")
		}
	}

	b.WriteString("\nAdmit and review only an exact candidate SHA from the list above, with a valid " +
		"cross-family verdict. Ignore any stale transcript in the target pane: the list above is " +
		"computed fresh from git, the pane is not.\n")
	// Responsibility assignment, preserved verbatim in intent from the original
	// packet. Rewriting the body dropped it once, and the existing test caught
	// that: the supervisor owns the review lifecycle end to end, and the
	// coordinator is not a fallback for any part of it.
	b.WriteString("\nYou own candidate discovery, receipt admission, reviewer dispatch, retries, " +
		"verdict ingest, and reviewer-pane cleanup. Do not ask the coordinator to perform any " +
		"of them.\n")
	// FAC-569: a live supervisor consumed two sequential nudges and reported
	// "The latest handoff supersedes the DeFi request", processing only the
	// newer one and leaving BOTH durable records unread. Retention alone did
	// not help because the supervisor treated the pane as the queue. The packet
	// must state that it is not the queue.
	b.WriteString("\nTHIS NUDGE DOES NOT SUPERSEDE ANY EARLIER HANDOFF. Handoffs are queued " +
		"durably and each is independent work; a newer one never cancels an older one. " +
		"Your DURABLE INBOX is the authority, not this pane:\n" +
		"  herd handoffs list                 # your pending handoffs; resolves your own name\n" +
		"  herd handoffs done <id>            # only after that handoff reaches a disposition\n" +
		"Do NOT pass a role id as --recipient. The commands resolve your exact agent name from\n" +
		"your pane; a role id names an inbox that does not exist and returns an empty list,\n" +
		"which is indistinguishable from having no work.\n" +
		"Drain the inbox before acting on pane text. An unread entry always means unfinished " +
		"work, so never mark one done to clear the list.\n")
	return b.String()
}

// ResumeGoal sends the harness resume verb to a lane whose goal loop stopped.
//
// FAC-614: an orchestrator sat at "Goal paused (/goal resume)" while herdr
// reported agent_status=working -- the process was alive, the goal loop was
// not. Every census called it healthy until an operator read the pane.
//
// The verb, not a prompt. A paused lane cannot consume plain text: kick already
// established this (FAC-696) and reported "FAIL unconsumed prompt" correctly
// and uselessly before it learned to send the verb instead.
//
// Re-checks the pane immediately before sending rather than trusting the
// classification that planned this action. A beat can be seconds old, the lane
// may have resumed on its own, and sending a resume verb into a lane that is
// working is the one harm this path can do.
func (a *livePulseActor) ResumeGoal(ctx context.Context, lane pulse.AgentObservation) error {
	paneID := strings.TrimSpace(lane.PaneID)
	if paneID == "" {
		return fmt.Errorf("pulse: resume requires pane_id; lane %q has none", lane.Name)
	}

	out, err := exec.CommandContext(ctx, "herdr", "pane", "read", paneID, "--source", "recent-unwrapped").Output()
	if err != nil {
		// Unknown is not paused. Refusing here costs one beat; guessing sends
		// the verb into a healthy lane.
		return fmt.Errorf("pulse: resume refused for lane %q pane %s: pane unreadable, and an unreadable pane is UNKNOWN not paused: %w",
			lane.Name, paneID, err)
	}
	if !kick.ContainsPausedGoalMarker(string(out)) {
		return fmt.Errorf("pulse: resume refused for lane %q pane %s: pane no longer shows a paused goal; it resumed on its own between the beat and now",
			lane.Name, paneID)
	}

	if err := exec.CommandContext(ctx, "herdr", "pane", "send-text", paneID, kick.GoalResumeVerb()).Run(); err != nil {
		return fmt.Errorf("pulse: resume send-text lane %q pane %s: %w", lane.Name, paneID, err)
	}
	if err := exec.CommandContext(ctx, "herdr", "pane", "send-keys", paneID, "enter").Run(); err != nil {
		return fmt.Errorf("pulse: resume submit lane %q pane %s: %w", lane.Name, paneID, err)
	}
	return nil
}

// safeStateSeq narrows herdr's unsigned transition counter without an
// overflowing conversion.
//
// FAC-614 wrote int64(a.StateChangeSeq) directly and the security gate refused
// it as an unreviewed HIGH G115 finding -- correctly: a uint64 above MaxInt64
// wraps to a NEGATIVE sequence, and resume escalation compares sequences to
// decide whether a lane made progress. A negative seq would read as "went
// backwards", which is neither progress nor stasis and would make the
// escalation counter behave unpredictably on exactly the lane it is meant to
// protect.
//
// Saturating is the right narrowing here rather than erroring: the counter is
// monotonic evidence of progress, so clamping at the maximum preserves the only
// property the comparison depends on.
func safeStateSeq(seq uint64) int64 {
	if seq > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(seq)
}
