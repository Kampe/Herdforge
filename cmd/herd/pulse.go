package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
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

	// Provider / queue pressure (one ListTasks). Also captures done task
	// refs for reap evidence — a lane whose ticket is done is reap-eligible.
	providerObs, doneRefs := readPulseProvider(ctx)
	obs.Provider = providerObs
	actor.dispatchRef = providerObs.NextTaskRef
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
	tasks, err := tp.ListTasks(ctx, project, "")
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
		agents = filterPulseAgentsWorkspace(agents, workspace)
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
			Name:              a.Name,
			Raw:               a.Status,
			Status:            pulse.ClassifyStatus(a.Status, false),
			PaneID:            a.PaneID,
			PaneState:         a.Status,
			ForegroundProcess: processName,
			TabID:             a.TabID,
			Workspace:         a.Workspace,
			TabGeneration:     a.TabGeneration,
			TabRevision:       a.Revision,
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
	// than free capacity for launches that require review headroom.
	path := drainLedgerPath()
	if _, err := os.Stat(path); err != nil {
		return pulse.ReviewObservation{Known: true, Pending: 0, NeedReview: 0}
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
	return pulse.ReviewObservation{
		Known: true, Pending: len(pendingRefs), PendingRefs: pendingRefs,
		NeedReview: len(needReviewRefs), NeedReviewRefs: needReviewRefs,
	}
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

// hasCommittedWork reports whether the worktree at cwd has at least one
// commit ahead of origin/main whose subject is not an anchor or wip marker.
// Git errors return false (fail closed — no evidence of committed work).
func hasCommittedWork(cwd string) bool {
	out, err := exec.Command("git", "-C", cwd, "log", "--format=%s", "origin/main..HEAD").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		subj := strings.TrimSpace(line)
		if subj == "" || strings.HasPrefix(subj, "chore: anchor") || strings.HasPrefix(subj, "wip:") {
			continue
		}
		return true
	}
	return false
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
	target := standing.AgentName(supervisor.Name)
	packet := pulseReviewPacket(lane)
	if _, err := herdr.AgentPrompt(target, packet, false); err != nil {
		return fmt.Errorf("pulse: notify review supervisor %s: %w", target, err)
	}
	_ = ctx
	return nil
}

func pulseReviewPacket(lane pulse.AgentObservation) string {
	return fmt.Sprintf("PULSE REVIEW HANDOFF\nLane: %s\nTab: %s\nWorkspace: %s\n\nInspect this finished lane's exact worktree and candidate receipt. Admit and review only an exact candidate SHA with valid verification evidence. You own reviewer dispatch, retries, verdict ingest, and reviewer-pane cleanup; send only a merge-ready PASS handoff to the coordinator. Do not ask the coordinator to perform review work.", lane.Name, lane.TabID, lane.Workspace)
}
