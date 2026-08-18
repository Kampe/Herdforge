package main

// FAC-184: the compiled action seams behind `herd drain --act`.
//
// Every action the coordinator beat can take is a typed adapter over an
// already-compiled authority: the board provider resolves a task ref, git
// reads back the exact candidate head, the router issues the launch decision
// (which carries the different-family reviewer policy), herdr proves a durable
// launch identity, and pkg/harvest's serialized Integration owns admission,
// merge, and board-complete. Nothing here shells out to a deleted script, a
// shell interpreter, or a sibling repository, and no adapter merges a branch
// or closes a card by itself.
//
// Every external authority is an injected seam so `herd drain --selftest` can
// exercise the same adapters hermetically with fake providers and a fake
// process API.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/review"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/standing"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

// drainLaunchProof is what a process API must return before a review launch is
// recorded: the live agent the packet reached, and the decision digest of the
// durable launch receipt that process API proved for it. An empty or
// mismatched digest is an absent receipt and fails closed.
type drainLaunchProof struct {
	Agent   string
	Receipt string
}

// drainLauncher is the process-API seam. The live implementation resolves the
// standing reviewer through herdr, which itself refuses to resolve an agent
// whose durable launch identity does not match the routed decision.
type drainLauncher interface {
	LaunchReviewer(ctx context.Context, req launch.Request, packet string) (drainLaunchProof, error)
}

// drainAdapters carries every authority one bounded `--act` beat may use.
// A nil field is missing authority, never a permissive default.
type drainAdapters struct {
	root       string
	project    string
	repository string
	cap        int
	lane       *config.LaneDef
	supervisor string
	tasks      provider.TaskProvider
	ledger     *reviewledger.Ledger
	launcher   drainLauncher

	// head reads back the exact current head of a candidate branch.
	head func(ctx context.Context, branch string) (string, error)
	// patchID computes an independent patch identity for a candidate SHA.
	patchID func(ctx context.Context, sha string) (string, error)
	// authorModel resolves the builder's model from its own durable launch
	// receipt — provenance the board's free-text labels cannot supply.
	authorModel func(taskRef string) (string, error)
	// route issues the reviewer launch decision.
	route func(*config.LaneDef, *provider.Task) (*router.LaunchDecision, error)
	// run executes the serialized harvest integration for one candidate.
	run func(ctx context.Context, sha string, adm harvest.AdmissionContext, dry bool) (*harvest.IntegrationResult, error)
}

func (a *drainAdapters) hooks() drainActionHooks {
	return drainActionHooks{
		launchReview: a.launchReview,
		dryRun:       func(ctx context.Context, e drainActionEvidence) error { return a.integrate(ctx, e, true) },
		harvest:      func(ctx context.Context, e drainActionEvidence) error { return a.integrate(ctx, e, false) },
	}
}

// authority refuses before any side effect when a required compiled authority
// was never wired. Unknown authority is never treated as permission.
func (a *drainAdapters) authority() error {
	switch {
	case a == nil:
		return fmt.Errorf("no compiled drain action authority")
	case a.tasks == nil:
		return fmt.Errorf("missing board provider authority")
	case a.ledger == nil:
		return fmt.Errorf("missing review ledger authority")
	case strings.TrimSpace(a.project) == "":
		return fmt.Errorf("missing board project authority")
	case a.head == nil || a.patchID == nil:
		return fmt.Errorf("missing git candidate-identity authority")
	}
	return nil
}

func (a *drainAdapters) reviewAuthority() error {
	if err := a.authority(); err != nil {
		return err
	}
	switch {
	case a.lane == nil:
		return fmt.Errorf("missing reviewer lane authority")
	case a.route == nil || a.authorModel == nil:
		return fmt.Errorf("missing launch routing authority")
	case a.launcher == nil:
		return fmt.Errorf("missing process API authority")
	}
	return nil
}

// launchReview turns one ledger candidate into a reviewer launch. It consumes
// the exact candidate SHA, the recorded builder family, a routed decision that
// excludes that family, live cap headroom, and a durable launch receipt; the
// ledger launch record is written only after all of them hold.
func (a *drainAdapters) launchReview(ctx context.Context, e drainActionEvidence) error {
	if err := a.reviewAuthority(); err != nil {
		return err
	}
	sha, err := drainExactSHA(e.SHA)
	if err != nil {
		return fmt.Errorf("review launch: %w", err)
	}
	family := strings.ToLower(strings.TrimSpace(e.BuilderFamily))
	if !review.LedgerFamilyAllowlist[family] {
		return fmt.Errorf("review launch: unknown recorded builder family %q", e.BuilderFamily)
	}
	head, err := a.head(ctx, e.Branch)
	if err != nil {
		return fmt.Errorf("review launch: candidate head readback for %q: %w", e.Branch, err)
	}
	if strings.TrimSpace(head) != sha {
		return fmt.Errorf("review launch: stale candidate — %s head is %s, ledger recorded %s", e.Branch, strings.TrimSpace(head), sha)
	}
	task, err := a.boardTask(ctx, e.Branch)
	if err != nil {
		return fmt.Errorf("review launch: %w", err)
	}
	if err := a.capHeadroom(ctx); err != nil {
		return fmt.Errorf("review launch: %w", err)
	}
	model, err := a.authorModel(task.Ref)
	if err != nil {
		return fmt.Errorf("review launch: %w", err)
	}
	decision, err := a.route(a.lane, drainCandidateTask(task, family, model, sha))
	if err != nil {
		return fmt.Errorf("review launch route rejected: %w", err)
	}
	if decision == nil {
		return fmt.Errorf("review launch: router returned no decision")
	}
	if decision.CandidateSHA != sha {
		return fmt.Errorf("review launch: decision candidate %q is not the exact candidate %s", decision.CandidateSHA, sha)
	}
	if decision.Family == "" || strings.EqualFold(decision.Family, family) {
		return fmt.Errorf("review launch: reviewer family %q must differ from builder family %q", decision.Family, family)
	}
	req := taskLaunchRequest(decision, task.Ref, a.repository, a.lane.Name)
	proof, err := a.launcher.LaunchReviewer(ctx, req, drainReviewPacket(task.Ref, sha, a.lane.Worktree, a.supervisor))
	if err != nil {
		return fmt.Errorf("review launch: %w", err)
	}
	if strings.TrimSpace(proof.Agent) == "" || proof.Receipt != launch.DecisionDigest(decision) {
		return fmt.Errorf("review launch: no exact durable launch receipt for %s", task.Ref)
	}
	// The launch record is the provenance later admission is checked against:
	// task and lease here must match what the reviewer reports in its verdict.
	return a.ledger.Record(reviewledger.RecordOpts{
		SHA:            sha,
		Branch:         e.Branch,
		BuilderFamily:  family,
		ReviewerFamily: strings.ToLower(decision.Family),
		Reviewer:       proof.Agent,
		Provider:       decision.Provider,
		Model:          decision.Model,
		Gate:           "drain",
		Tier:           e.Tier,
		Task:           task.Ref,
		Lease:          strconv.FormatInt(decision.LeaseGeneration, 10),
	})
}

// integrate runs one candidate through the serialized pkg/harvest pipeline.
// Admission context is resolved from the launch record and an independently
// computed patch id — never from the verdict row Admit is about to check it
// against — and only this one SHA has any admission context at all, so a
// second candidate cannot ride along on the same beat.
func (a *drainAdapters) integrate(ctx context.Context, e drainActionEvidence, dry bool) error {
	if err := a.authority(); err != nil {
		return err
	}
	if a.run == nil {
		return fmt.Errorf("missing harvest integration authority")
	}
	sha, err := drainExactSHA(e.SHA)
	if err != nil {
		return fmt.Errorf("harvest: %w", err)
	}
	adm, err := a.admission(ctx, sha)
	if err != nil {
		return fmt.Errorf("harvest: %w", err)
	}
	res, err := a.run(ctx, sha, adm, dry)
	if err != nil {
		return fmt.Errorf("harvest integration: %w", err)
	}
	if res == nil {
		return fmt.Errorf("harvest integration returned no result for %s", sha)
	}
	gated := false
	for _, g := range res.ReviewGatedSHAs {
		if g.SHA != sha {
			continue
		}
		gated = true
		if !g.Eligible {
			return fmt.Errorf("harvest: review admission refused for %s: %s", sha, drainReason(g.Reason, g.Err))
		}
	}
	if !gated {
		return fmt.Errorf("harvest: integration never gated exact candidate %s", sha)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("harvest integration errors: %s", strings.Join(res.Errors, "; "))
	}
	if dry {
		return nil
	}
	for _, m := range res.MergedSHAs {
		if m.SHA == sha && m.Err == "" && (m.Pushed || m.AlreadyMerged) {
			return nil
		}
	}
	return fmt.Errorf("harvest: exact candidate %s was not merged", sha)
}

// admission binds the merge context Admit validates. Task and lease come from
// the durable launch record written at review launch; the patch id is computed
// from git. Admit then requires the reviewer's own verdict row to agree.
func (a *drainAdapters) admission(ctx context.Context, sha string) (harvest.AdmissionContext, error) {
	rows, err := a.ledger.AllRows()
	if err != nil {
		return harvest.AdmissionContext{}, fmt.Errorf("read review ledger: %w", err)
	}
	want := a.ledger.NormalizeSHA(sha)
	var record *reviewledger.LedgerRow
	for i := range rows {
		if rows[i].Event == string(reviewledger.EventRecord) && a.ledger.NormalizeSHA(rows[i].SHA) == want {
			record = &rows[i]
		}
	}
	if record == nil {
		return harvest.AdmissionContext{}, fmt.Errorf("no durable review launch record for exact candidate %s", sha)
	}
	if strings.TrimSpace(record.Task) == "" || strings.TrimSpace(record.Lease) == "" {
		return harvest.AdmissionContext{}, fmt.Errorf("review launch record for %s carries no task/lease provenance", sha)
	}
	family := strings.ToLower(strings.TrimSpace(record.BuilderFamily))
	if !review.LedgerFamilyAllowlist[family] {
		return harvest.AdmissionContext{}, fmt.Errorf("review launch record for %s has unknown builder family %q", sha, record.BuilderFamily)
	}
	patch, err := a.patchID(ctx, sha)
	if err != nil {
		return harvest.AdmissionContext{}, fmt.Errorf("patch identity for %s: %w", sha, err)
	}
	if strings.TrimSpace(patch) == "" {
		return harvest.AdmissionContext{}, fmt.Errorf("empty patch identity for %s", sha)
	}
	return harvest.AdmissionContext{
		Task:           record.Task,
		Lease:          record.Lease,
		PatchURL:       strings.TrimSpace(patch),
		AuthorFamily:   family,
		AuthorIdentity: record.BuilderIdentity,
	}, nil
}

// boardTask resolves the branch to exactly one live board task. A branch is
// not a task ref: a branch whose ticket token matches no live card is refused
// here rather than silently reported as "no tasks" by a launcher.
func (a *drainAdapters) boardTask(ctx context.Context, branch string) (*provider.Task, error) {
	ref := drainCandidateRef(branch)
	if ref == "" {
		return nil, fmt.Errorf("branch %q carries no task ref", branch)
	}
	var match *provider.Task
	for _, status := range []string{"in-progress", "in-review"} {
		tasks, err := a.tasks.ListTasks(ctx, a.project, status)
		if err != nil {
			return nil, fmt.Errorf("board lookup for %s: %w", ref, err)
		}
		for _, t := range tasks {
			if t == nil || !strings.EqualFold(hsync.NormalizeRef(t.Ref), ref) {
				continue
			}
			if match != nil && match.ID != t.ID {
				return nil, fmt.Errorf("ambiguous live board tasks for %s", ref)
			}
			match = t
		}
	}
	if match == nil {
		return nil, fmt.Errorf("no live board task for %s (branch %q)", ref, branch)
	}
	return match, nil
}

// capHeadroom re-reads the live board immediately before a launch. The report's
// count is one beat old; the cap is only honest against current state.
func (a *drainAdapters) capHeadroom(ctx context.Context) error {
	if a.cap <= 0 {
		return fmt.Errorf("review cap authority unknown")
	}
	tasks, err := a.tasks.ListTasks(ctx, a.project, "in-review")
	if err != nil {
		return fmt.Errorf("live review cap posture unknown: %w", err)
	}
	if len(tasks) >= a.cap {
		return fmt.Errorf("live review cap drift: %d in-review at cap %d", len(tasks), a.cap)
	}
	return nil
}

func drainReason(reason, errText string) string {
	if strings.TrimSpace(reason) != "" {
		return reason
	}
	if strings.TrimSpace(errText) != "" {
		return errText
	}
	return "unspecified"
}

// drainExactSHA refuses anything that is not a full-length object id. A short
// or empty pin cannot identify the candidate a verdict was bound to.
func drainExactSHA(sha string) (string, error) {
	trimmed := strings.TrimSpace(sha)
	if len(trimmed) != 40 || strings.TrimLeft(trimmed, "0123456789abcdefABCDEF") != "" {
		return "", fmt.Errorf("an exact candidate SHA is required, got %q", sha)
	}
	return strings.ToLower(trimmed), nil
}

var drainRefToken = regexp.MustCompile(`(?i)([a-z]{2,})[-/]([0-9]+)`)

// drainCandidateRef extracts the ticket token a branch claims. It is only a
// candidate: boardTask decides whether it is a real task.
func drainCandidateRef(branch string) string {
	m := drainRefToken.FindStringSubmatch(strings.TrimSpace(branch))
	if m == nil {
		return ""
	}
	return hsync.NormalizeRef(strings.ToUpper(m[1]) + "-" + m[2])
}

// drainCandidateTask carries compiled provenance into the routing authority.
// Board-supplied provenance labels are deliberately dropped: the ledger and
// the builder's own launch receipt are the authority, not board free text.
func drainCandidateTask(t *provider.Task, family, model, sha string) *provider.Task {
	candidate := *t
	candidate.Labels = []string{"author-family:" + family, "author-model:" + model, "candidate-sha:" + sha}
	return &candidate
}

func drainReviewPacket(ref, sha, worktree, supervisor string) string {
	return fmt.Sprintf(`REVIEW %s candidate %s — verdict ONLY, edit nothing. End with the verdict line.
REPORT_TARGET: %s (mandatory; never coordinator)
REPORT_CONTRACT: deliver the signed verdict artifact to the review supervisor. The supervisor owns retries, author feedback, exact-SHA ledger ingest, and reviewer-tab cleanup. The coordinator receives only an exact PASS plus merge-ready handoff.
cd %s
1. git diff origin/main..%s --stat  (review ONLY these changed files)
2. %s   (targeted tests for the changed packages, not the whole repo)
Your FINAL line MUST be exactly one of:
REVIEW VERDICT %s: APPROVED
REVIEW VERDICT %s: REJECTED - <numbered fixes>
Do not read the whole codebase. Do not run the full suite. Change nothing.


 Do not run a retry loop yourself. On FAIL, include numbered findings and stop;
 the supervisor re-dispatches the fresh SHA. On PASS, stop after retaining the
 artifact and notifying the supervisor. The coordinator performs post-merge
 generation-fenced cleanup; preserve standing lanes and lanes with unconsumed
 review/goal evidence.`,
		ref, sha, supervisor, worktree, sha, scopedTestCommand(worktree), ref, ref)
}

// ---- live seams ------------------------------------------------------------

func drainGitHead(root string) func(context.Context, string) (string, error) {
	return func(ctx context.Context, branch string) (string, error) {
		if strings.TrimSpace(branch) == "" {
			return "", fmt.Errorf("no candidate branch recorded")
		}
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "-q", branch+"^{commit}")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git rev-parse %s: %w", branch, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
}

func drainPatchID(root string) func(context.Context, string) (string, error) {
	engine := review.NewReviewEngine(root)
	return func(ctx context.Context, sha string) (string, error) { return engine.ComputePatchID(ctx, sha) }
}

// drainAuthorModel reads the builder's own accepted launch receipt. A review
// launch that cannot name the model it is reviewing has no provenance.
func drainAuthorModel(taskRef string) (string, error) {
	path := strings.TrimSpace(os.Getenv("HERD_LAUNCH_RECEIPTS"))
	if path == "" {
		path = ".herd/launch-receipts.jsonl"
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("no builder launch receipts for %s: %w", taskRef, err)
	}
	defer f.Close()
	builder := map[string]bool{launch.WorkerRole: true, launch.ForgeSmithRole: true, launch.RecoveryRole: true}
	model := ""
	dec := json.NewDecoder(f)
	for {
		var r launch.Receipt
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("read builder launch receipts: %w", err)
		}
		if r.Accepted && r.TaskRef == taskRef && builder[strings.ToLower(r.Role)] && strings.TrimSpace(r.Model) != "" {
			model = r.Model
		}
	}
	if model == "" {
		return "", fmt.Errorf("no accepted builder launch receipt for %s; author model is unprovable", taskRef)
	}
	return model, nil
}

// liveDrainLauncher delivers to the standing reviewer only. Resolving it
// through herdr is itself the receipt proof: ResolveAgentTabWithDecision
// refuses an agent whose durable launch identity does not match the decision.
type liveDrainLauncher struct{ lane string }

func (l liveDrainLauncher) LaunchReviewer(_ context.Context, req launch.Request, packet string) (drainLaunchProof, error) {
	if !herdr.IsAvailable() {
		return drainLaunchProof{}, fmt.Errorf("herdr CLI not found")
	}
	name := fmt.Sprintf("forge-%s", l.lane)
	if _, err := herdr.ResolveAgentTabWithDecision(name, req); err != nil {
		return drainLaunchProof{}, fmt.Errorf("standing reviewer %s has no proven durable launch identity: %w", name, err)
	}
	if _, err := herdr.AgentPrompt(name, packet, false); err != nil {
		return drainLaunchProof{}, fmt.Errorf("deliver review packet to %s: %w", name, err)
	}
	return drainLaunchProof{Agent: name, Receipt: launch.DecisionDigest(req.Decision)}, nil
}

// drainVerifier adapts the compiled test gate to the harvest contract.
type drainVerifier struct{ v *verifier.Verifier }

func (d drainVerifier) Execute(ctx context.Context, dir string) (*harvest.VerifyResult, error) {
	r, err := d.v.Execute(ctx, dir)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("verifier returned no result for %s", dir)
	}
	return &harvest.VerifyResult{Passed: r.Passed, Output: r.Output}, nil
}

// drainDispatcher closes a card only through the compiled board-done authority,
// which requires merge evidence on origin/main and verifies by read-back.
type drainDispatcher struct {
	tasks   provider.TaskProvider
	root    string
	project string
}

// BoardComplete closes a card through the same authority `herd board-done` uses.
//
// This branch was written against the old positional BoardDone(ctx, tp, root,
// project, ref, evidenceSHA, force). FAC-132 replaced that with a DoneRequest
// whose closing authority is a task-bound completion RECEIPT (or an explicit,
// attributable override) -- a bare evidence SHA is no longer sufficient to move
// a card. buildDoneRequest is the shared loader that both CLI call sites use, so
// the drain path cannot close a card by a weaker rule than the CLI.
//
// evidenceSHA is retained in the signature because callers still record it, but
// it is deliberately NOT passed as closing authority: doing so would reintroduce
// exactly the evidence class FAC-132 removed.
func (d drainDispatcher) BoardComplete(ctx context.Context, ref, evidenceSHA string) error {
	_ = evidenceSHA
	req, closeAuthority, err := buildDoneRequest(d.root, d.project, ref, "", nil)
	if err != nil {
		closeAuthority()
		return err
	}
	res, err := hsync.BoardDone(ctx, d.tasks, req)
	closeAuthority()
	if err != nil {
		return err
	}
	releaseScopeClaimQuietly(res.Ref)
	return nil
}

// liveDrainIntegration runs the serialized pipeline for exactly one candidate:
// only this SHA has admission context, so every other unmerged tip fails the
// review gate closed on the same beat.
func (a *drainAdapters) liveDrainIntegration(ctx context.Context, sha string, adm harvest.AdmissionContext, dry bool) (*harvest.IntegrationResult, error) {
	in := harvest.NewIntegration(
		harvest.NewHarvester(a.root),
		drainVerifier{v: verifier.NewVerifier("")},
		drainDispatcher{tasks: a.tasks, root: a.root, project: a.project},
		a.ledger,
		a.root,
		harvest.WithDryRun(dry),
		harvest.WithAdmissionSource(harvest.MapAdmissionSource{sha: adm}),
	)
	return in.Run(ctx)
}

// newDrainAdapters wires the live authorities. Any missing piece is reported,
// and the caller keeps the fail-closed default hooks.
func newDrainAdapters(root, ledgerPath string, cfg *config.Config, tp provider.TaskProvider, reviewCap int) (*drainAdapters, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no compiled config authority")
	}
	if tp == nil {
		return nil, fmt.Errorf("no board provider authority")
	}
	lane := findReviewSupervisorLane(cfg)
	if lane == nil {
		lane = findLaneForRole(cfg, launch.AssayerRole)
	}
	if lane == nil {
		return nil, fmt.Errorf("no reviewer lane configured (roles: reviewer, assayer)")
	}
	supervisorLane := findReviewSupervisorLane(cfg)
	if supervisorLane == nil {
		return nil, fmt.Errorf("no standing review supervisor lane configured (roles: review-supervisor, reviewer, harvest)")
	}
	if strings.TrimSpace(lane.Worktree) == "" {
		return nil, fmt.Errorf("reviewer lane %q has no isolated worktree", lane.Name)
	}
	ledger, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		return nil, fmt.Errorf("review ledger: %w", err)
	}
	project := strings.TrimSpace(cfg.TaskProvider.ProjectID)
	if project == "" {
		project = review.ResolveKaneoProject(root)
	}
	a := &drainAdapters{
		root:        root,
		project:     project,
		repository:  repositoryIdentityForLaunch(cfg),
		cap:         reviewCap,
		lane:        lane,
		supervisor:  standing.AgentName(supervisorLane.Name),
		tasks:       tp,
		ledger:      ledger,
		launcher:    liveDrainLauncher{lane: lane.Name},
		head:        drainGitHead(root),
		patchID:     drainPatchID(root),
		authorModel: drainAuthorModel,
		route: func(lane *config.LaneDef, task *provider.Task) (*router.LaunchDecision, error) {
			return laneLaunchDecision(context.Background(), lane, task)
		},
	}
	a.run = a.liveDrainIntegration
	if err := a.reviewAuthority(); err != nil {
		return nil, err
	}
	return a, nil
}
