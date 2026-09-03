package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/eligibility"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/shot"
	"github.com/Kampe/Herdforge/pkg/verifier"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// FAC-89: `herd shot <task-ref>` — the bounded ONE-TASK lane.
//
// pkg/shot.TaskRun owns the order and the gates; this file owns the production
// wiring of each seam and the exit contract. Exit codes: 0 the task reached
// review admission, 1 any stage failed, 2 a usage error.

// shotCallbackPoll is how often the callback wait re-reads the coordinator
// inbox. The wait itself is bounded by --timeout.
const shotCallbackPoll = 2 * time.Second

// runShotTask drives one board task from eligibility to review admission.
func runShotTask(ref string, args []string) {
	fs := flag.NewFlagSet("shot", flag.ExitOnError)
	lane := fs.String("lane", "worker", "Lane name or role to dispatch into")
	timeoutSec := fs.Int("timeout", 900, "Seconds to wait for the completion callback")
	risk := fs.String("risk", "", "Risk tier for this card (R0-R3) when the board carries no risk label")
	asJSON := fs.Bool("json", false, "Emit the evidence packet as JSON instead of prose")
	report := fs.String("report", "", "Post a builder callback instead of running a shot (complete|blocked)")
	sha := fs.String("sha", "", "Exact candidate commit SHA for --report complete; closed-worker supersession requires `herd receipt issue --role recovery --candidate-supersession <ref> <worktree>`")
	lease := fs.Int64("lease", 0, "Lease generation the shot reported at dispatch (required with --report)")
	detail := fs.String("detail", "", "Detail for --report blocked")
	fs.Parse(args)

	laneExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "lane" {
			laneExplicit = true
		}
	})

	root, err := worktree.ResolveCanonicalRoot(context.Background(), ".", firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ""))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd shot: resolve canonical repository root: %v\n", err)
		os.Exit(1)
	}

	if *report != "" {
		if err := postShotCallback(root, ref, *report, *sha, *detail, *lease); err != nil {
			fmt.Fprintf(os.Stderr, "herd shot: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("herd shot: %s callback posted for %s\n", *report, ref)
		return
	}
	if *timeoutSec <= 0 {
		fmt.Fprintln(os.Stderr, "herd shot: --timeout must be a positive integer")
		os.Exit(2)
	}

	// --json must own stdout so the evidence packet stays parseable.
	announce := io.Writer(os.Stdout)
	if *asJSON {
		announce = os.Stderr
	}

	// Resolve the config path ONCE, against the repository root. The shot
	// enters its task worktree before verifying, and a relative ".herd/herd.yaml"
	// would then resolve inside that worktree instead of the root.
	cfgPath := shotConfigPath(root)

	run := &shot.TaskRun{
		Eligible: func(ctx context.Context, ref string) (eligibility.Result, error) {
			return shotEligibility(ctx, cfgPath, ref, *lane, *risk)
		},
		Dispatch: func(ctx context.Context, ref, laneName string) (shot.DispatchFacts, error) {
			return shotDispatch(ctx, ref, laneName, laneExplicit, announce)
		},
		Await: func(ctx context.Context, ref string, leaseGen int64) (mail.Callback, error) {
			fmt.Fprintf(announce, "herd shot: waiting for `herd shot %s --report complete --sha <sha> --lease %d`\n", ref, leaseGen)
			return awaitShotCallback(ctx, root, ref, leaseGen)
		},
		Verify: func(ctx context.Context, dir string, req verifier.VerificationRequest) (*verifier.Receipt, error) {
			return shotVerify(ctx, cfgPath, root, dir, req)
		},
		Handoff: func(ctx context.Context, ev shot.Evidence) error {
			return shotHandoff(root, ev)
		},
	}

	ev, runErr := run.Run(context.Background(), shot.TaskRequest{
		TaskRef:         ref,
		Lane:            *lane,
		Root:            root,
		CallbackTimeout: time.Duration(*timeoutSec) * time.Second,
	})

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ev)
	} else {
		printShotEvidence(os.Stdout, ev)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "herd shot: %v\n", runErr)
		os.Exit(1)
	}
}

func printShotEvidence(w io.Writer, ev shot.Evidence) {
	status := "FAILED"
	if ev.OK {
		status = "OK"
	}
	fmt.Fprintf(w, "herd shot %s: %s at stage %s\n", ev.TaskRef, status, ev.Stage)
	for _, field := range [][2]string{
		{"lane", ev.Lane}, {"worktree", ev.Worktree}, {"branch", ev.Branch},
		{"candidate", ev.CandidateSHA}, {"receipt", ev.ReceiptDigest},
		{"verification", ev.VerificationOutcome},
	} {
		if field[1] != "" {
			fmt.Fprintf(w, "  %-13s %s\n", field[0]+":", field[1])
		}
	}
	if ev.LeaseGeneration > 0 {
		fmt.Fprintf(w, "  %-13s %d\n", "lease:", ev.LeaseGeneration)
	}
	if !ev.OK {
		fmt.Fprintf(w, "  %-13s %t\n", "recoverable:", ev.Recoverable)
	}
}

// shotEligibility answers the board question for exactly one ref. It runs BOTH
// gates that already exist: the deps launch gate (dependency closure, TOCTOU,
// provenance) under the shot entrypoint, and the FAC-123 board evaluation
// (acceptance criteria, role label, duplicates, status, priority).
//
// RequireRiskHint is on: the bounded lane hands its candidate to the review
// ledger, and Admit refuses a candidate with no risk tier on record. Refusing
// before the claim beats discovering it after verification. --risk supplies the
// tier for a card whose board labels carry none.
func shotEligibility(ctx context.Context, cfgPath, ref, laneValue, risk string) (eligibility.Result, error) {
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		return eligibility.Result{}, fmt.Errorf("failed to load config: %w", err)
	}
	tp, err := loadTaskProvider(cfg)
	if err != nil {
		return eligibility.Result{}, fmt.Errorf("task provider: %w", err)
	}
	task, err := tp.GetTask(ctx, ref)
	if err != nil {
		return eligibility.Result{}, fmt.Errorf("read task %s: %w", ref, err)
	}
	if task == nil {
		return eligibility.Result{}, fmt.Errorf("task %s not found", ref)
	}

	desired, err := deps.ExtractProvenanceFromText(task.Description)
	if err != nil {
		return eligibility.Result{}, fmt.Errorf("provenance extract: %w", err)
	}
	store := deps.StoreFor(tp, cfg.TaskProvider.ProjectID)
	gate, err := deps.RequireTaskLaunch(ctx, store, deps.EntryShot, deps.Ref(ref), desired, "")
	if err != nil {
		return eligibility.Result{}, fmt.Errorf("dependency gate: %w", err)
	}

	facts := eligibility.Facts{RequireRiskHint: true}
	if len(gate.BlockedBy) > 0 {
		facts.Blockers = map[string][]string{ref: gate.BlockedBy}
		facts.OpenRefs = map[string]bool{}
		for _, b := range gate.BlockedBy {
			facts.OpenRefs[b] = true
		}
	}
	if strings.TrimSpace(risk) != "" {
		facts.RiskHints = map[string]string{ref: strings.TrimSpace(risk)}
	}

	role := laneValue
	if registry, rerr := canonicalLaneRegistry(cfg); rerr == nil {
		if canonical, lerr := registry.ResolveRole(laneValue); lerr == nil {
			role = canonical.Role
		} else if canonical, lerr := registry.ResolveLaneName(laneValue); lerr == nil {
			role = canonical.Role
		}
	}
	result := eligibility.EvaluateTask(task, facts, role)
	shotRiskTier = strings.TrimSpace(result.RiskHint)
	return result, nil
}

// shotDispatch is the atomic claim + isolated launch. It reuses the exact
// production path `herd dispatch` takes — same hold admission, same scope
// fence, same launch admission — so the bounded lane cannot be a weaker door.
func shotDispatch(ctx context.Context, ref, laneValue string, laneExplicit bool, announce io.Writer) (shot.DispatchFacts, error) {
	result, decision, err := dispatchTicketDecision(ctx, dispatchRequest{
		TicketRef:    ref,
		LaneName:     laneValue,
		LaneExplicit: laneExplicit,
	}, announce)
	if err != nil {
		return shot.DispatchFacts{}, err
	}
	if !result.Launched {
		return shot.DispatchFacts{}, fmt.Errorf("dispatch produced no live agent for %s; nothing will ever call back", ref)
	}
	facts := shot.DispatchFacts{
		Worktree:        result.Worktree,
		Branch:          result.Branch,
		BaseSHA:         result.BaseSHA,
		LeaseGeneration: result.LeaseGeneration,
		Lane:            result.Lane,
		Launched:        result.Launched,
	}
	if decision != nil {
		fmt.Fprintf(announce, "herd shot: %s launched on %s/%s (lease %d)\n",
			ref, decision.Provider, result.Model, result.LeaseGeneration)
	}
	shotBuilderFamily = builderFamilyFor(decision, result.Model)
	shotBuilderIdentity = result.AgentName
	return facts, nil
}

// These carry facts each stage resolves forward to the review handoff: the
// risk tier eligibility settled on, and the surface dispatch actually launched.
// A shot is one task in one process, so package-level is the whole state this
// needs.
// ponytail: process-scoped, single-task by construction; if `herd shot` ever
// runs two tasks in one process these move onto the run struct.
var (
	shotRiskTier        string
	shotBuilderFamily   string
	shotBuilderIdentity string
)

func builderFamilyFor(decision *router.LaunchDecision, model string) string {
	if decision == nil {
		return ""
	}
	if model == "" {
		model = decision.Model
	}
	return router.FamilyFor(decision.Provider, model)
}

// shotConfigPath anchors the herd config at the repository root while still
// honouring the operator's HERD_CONFIG_PATH profile override.
func shotConfigPath(root string) string {
	if path := os.Getenv("HERD_CONFIG_PATH"); path != "" {
		return path
	}
	return filepath.Join(root, config.DefaultConfigPath)
}

// shotMailbox is the coordinator inbox. It is the same durable mailbox the
// control plane already uses; mail routes by recipient and DrainCallbacks
// filters by subject, so agent callbacks and coordinator orders coexist in it.
func shotMailbox(root string) *mail.Mailbox {
	return mail.NewMailbox(mail.CallbackMailPath(root))
}

// awaitShotCallback blocks until a callback bound to THIS ref and lease lands,
// or the bounded wait expires. Callbacks for other cards or other leases are
// left in the inbox for their own owner.
func awaitShotCallback(ctx context.Context, root, ref string, lease int64) (mail.Callback, error) {
	mb := shotMailbox(root)
	for {
		callbacks, err := mb.DrainCallbacksContext(ctx)
		if err != nil {
			return mail.Callback{}, fmt.Errorf("read coordinator inbox: %w", err)
		}
		for _, cb := range callbacks {
			if strings.EqualFold(strings.TrimSpace(cb.Ref), ref) && cb.LeaseGeneration == lease {
				return cb, nil
			}
		}
		select {
		case <-ctx.Done():
			return mail.Callback{}, fmt.Errorf(
				"no callback for %s at lease %d before the shot timeout: %w", ref, lease, ctx.Err())
		case <-time.After(shotCallbackPoll):
		}
	}
}

// postShotCallback is the builder half of the loop: it posts the completion (or
// blocked) report the waiting shot is bound to.
func postShotCallback(root, ref, kind, sha, detail string, lease int64) error {
	if lease <= 0 {
		return fmt.Errorf("--lease is required; use the lease generation the shot printed at dispatch")
	}
	cb := mail.Callback{Ref: ref, LeaseGeneration: lease, Detail: strings.TrimSpace(detail)}
	switch kind {
	case string(mail.CallbackComplete):
		cb.Kind = mail.CallbackComplete
		cb.SHA = strings.TrimSpace(sha)
		if cb.SHA == "" {
			return fmt.Errorf("--report complete requires --sha <exact 40-character commit SHA>")
		}
		if err := recordShotLifecycleLease(root, ref, lease, cb.SHA); err != nil {
			return err
		}
	case string(mail.CallbackBlocked):
		cb.Kind = mail.CallbackBlocked
		if cb.Detail == "" {
			return fmt.Errorf("--report blocked requires --detail explaining what is blocking")
		}
	default:
		return fmt.Errorf("--report must be %q or %q, got %q",
			mail.CallbackComplete, mail.CallbackBlocked, kind)
	}
	if _, err := shotMailbox(root).PostCallback("shot:"+strings.ToLower(ref), cb); err != nil {
		return fmt.Errorf("post callback: %w", err)
	}
	if cb.Kind == mail.CallbackBlocked {
		kind := router.HelpKindForReason(cb.Detail)
		route, routeErr := router.DefaultHelpRoute(kind)
		if routeErr != nil {
			return fmt.Errorf("route blocked help request: %w", routeErr)
		}
		req := mail.HelpRequest{
			Lane:            "shot:" + strings.ToLower(ref),
			TaskRef:         ref,
			Reason:          cb.Detail,
			Capability:      route.Capability,
			SuggestedHelper: route.Target,
			SuggestedFamily: route.Family,
		}
		if _, helpErr := shotMailbox(root).PostHelpRequest(req.Lane, req); helpErr != nil {
			return fmt.Errorf("post blocked help request: %w", helpErr)
		}
	}
	return nil
}

// recordShotLifecycleLease makes the builder callback's lease evidence visible
// to the canonical completion/review path. Dispatch normally creates this
// state before launch; the callback is also the recovery boundary for older
// worktrees that have a valid signed task receipt but no lifecycle row yet.
// Same-generation retries are idempotent; stale or conflicting evidence is
// rejected rather than overwriting the durable record.
func recordShotLifecycleLease(root, ref string, lease int64, sha string) error {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(ref) == "" {
		return fmt.Errorf("shot: lifecycle evidence requires repository root and task ref")
	}
	if lease <= 0 {
		return fmt.Errorf("shot: lifecycle evidence requires a positive lease generation")
	}
	if !validShotSHA(strings.TrimSpace(sha)) {
		return fmt.Errorf("shot: lifecycle evidence requires an exact candidate SHA")
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o700); err != nil {
		return fmt.Errorf("shot: create lifecycle state directory: %w", err)
	}
	machine, err := lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		return fmt.Errorf("shot: open lifecycle machine: %w", err)
	}
	defer machine.Close()
	current, err := machine.EventStore().CurrentState(ref)
	if err != nil {
		return fmt.Errorf("shot: read lifecycle state: %w", err)
	}
	if current != nil {
		if current.LeaseGeneration != lease {
			if current.LeaseGeneration < lease {
				return runShotGenerationReconcile(context.Background(), root, ref, lease, sha, machine, current)
			}
			return shotLeaseGenerationConflict(current.LeaseGeneration, lease)
		}
		if current.CandidateSHA != "" && current.CandidateSHA != sha {
			// An eligible row can be left behind when a worker is retried
			// without a fresh dispatch event. Preserve the lease fence while
			// moving through the canonical recovery state to bind the callback
			// to the candidate that actually completed. Once work has advanced
			// beyond eligibility, a same-generation SHA change is still a
			// conflict and must fail closed.
			if current.State == lifecycle.StateRecovering {
				return runShotCandidateSupersession(context.Background(), root, ref, lease, sha, machine, current)
			}
			if current.State != lifecycle.StateEligible {
				return fmt.Errorf("shot: lifecycle candidate %s conflicts with reported %s", current.CandidateSHA, sha)
			}
			if _, err := machine.Transition(lifecycle.TransitionRequest{
				TaskRef:         ref,
				Repo:            "herdforge",
				To:              lifecycle.StateRecovering,
				Actor:           "worker",
				IdempotencyKey:  fmt.Sprintf("shot:%s:lease:%d:recovery-candidate:%s", strings.ToLower(ref), lease, sha),
				LeaseGeneration: lease,
				Branch:          "herd/" + strings.ToLower(ref),
				CandidateSHA:    sha,
			}); err != nil {
				return fmt.Errorf("shot: update lifecycle candidate: %w", err)
			}
			return nil
		}
		return nil
	}
	if _, err := machine.Transition(lifecycle.TransitionRequest{
		TaskRef:         ref,
		Repo:            "herdforge",
		To:              lifecycle.StateEligible,
		Actor:           "worker",
		IdempotencyKey:  fmt.Sprintf("shot:%s:lease:%d:candidate:%s", strings.ToLower(ref), lease, sha),
		LeaseGeneration: lease,
		Branch:          "herd/" + strings.ToLower(ref),
		CandidateSHA:    sha,
	}); err != nil {
		return fmt.Errorf("shot: record lifecycle lease: %w", err)
	}
	return nil
}

func shotLeaseGenerationConflict(held, reported int64) error {
	return fmt.Errorf("shot: lifecycle lease generation %d conflicts with reported %d", held, reported)
}

func validShotSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// shotVerify runs the configured verification command against the exact
// candidate and persists its receipt. An unconfigured command is a missing
// capability, not a reason to skip verification.
func shotVerify(ctx context.Context, cfgPath, root, dir string, req verifier.VerificationRequest) (*verifier.Receipt, error) {
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	command := strings.TrimSpace(cfg.Verification.TestCommand)
	if command == "" {
		return nil, fmt.Errorf("no verification.test_command configured; a shot cannot verify %s", req.CandidateSHA)
	}
	store, err := verifier.NewFileReceiptStore(filepath.Join(root, ".herd", "receipts"))
	if err != nil {
		return nil, fmt.Errorf("receipt store: %w", err)
	}
	return verifier.NewVerifier(command).VerifyAndPersist(ctx, dir, req, store)
}

// shotHandoff admits the verified candidate to review by appending its launch
// record to the review ledger. It records the exact SHA, the lease, the
// verification receipt digest, and the builder family — the fields
// (*Ledger).Admit later requires of a reviewer's verdict. It never merges and
// never moves the card to Done.
func shotHandoff(root string, ev shot.Evidence) error {
	if shotBuilderFamily == "" {
		return fmt.Errorf("builder family unknown; a review record without provable authorship cannot be admitted")
	}
	tier := shotRiskTier
	if tier == "" {
		return fmt.Errorf("no risk tier for %s; review admission refuses a candidate with no tier on record", ev.TaskRef)
	}
	ledgerPath := firstEnv("HERD_REVIEW_LEDGER", "HERD_SHOT_REVIEW_LEDGER",
		filepath.Join(stateDir(), "review-ledger.jsonl"))
	ledger, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		return fmt.Errorf("review ledger: %w", err)
	}
	return ledger.Record(reviewledger.RecordOpts{
		SHA:             ev.CandidateSHA,
		Branch:          ev.Branch,
		BuilderFamily:   shotBuilderFamily,
		BuilderIdentity: shotBuilderIdentity,
		Artifact:        ev.ReceiptDigest,
		Gate:            "independent",
		Tier:            tier,
		Task:            ev.TaskRef,
		Lease:           strconv.FormatInt(ev.LeaseGeneration, 10),
	})
}
