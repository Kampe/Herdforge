package dispatch

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/envplan"
	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

var ErrRecoveredLifecycleRefused = errors.New("dispatch: recovered lifecycle generation rebind refused")

// RecoveredLifecycleRequest is the exact coordinator-owned identity available
// after a recovered packet/context pair has been durably published, and before
// any launcher receives it.
type RecoveredLifecycleRequest struct {
	Binding             envplan.Binding
	EnvironmentPlanID   string
	ProviderType        string
	ProjectID           string
	TaskID              string
	TaskRef             string
	Repository          string
	LifecycleRepository string
	LeaseID             string
	LeaseGeneration     int64
	Branch              string
	WorktreePath        string
	WorktreeHead        string
	BaseSHA             string
	AnchorRef           string
	TaskPacket          string
	NewReceipt          TaskContext
}

type RecoveredLifecycleAuthority interface {
	RebindRecovered(context.Context, RecoveredLifecycleRequest) (*lifecycle.GenerationRebindResult, error)
}

// LifecycleGenerationRebinder consumes only durable, append-only authorities.
// It never edits a receipt, callback, review row, candidate ref, or runstate.
type LifecycleGenerationRebinder struct {
	RepoRoot         string
	LifecyclePath    string
	ReviewLedgerPath string
	CallbackPath     string
	EnvironmentPlans *envplan.Store
	Now              func() time.Time
}

func NewLifecycleGenerationRebinder(root, lifecyclePath, reviewLedgerPath, callbackPath string, plans *envplan.Store) *LifecycleGenerationRebinder {
	return &LifecycleGenerationRebinder{
		RepoRoot: root, LifecyclePath: lifecyclePath, ReviewLedgerPath: reviewLedgerPath,
		CallbackPath: callbackPath, EnvironmentPlans: plans, Now: time.Now,
	}
}

type recoveredLifecycleEvidence struct {
	Version               int                 `json:"version"`
	ProviderType          string              `json:"provider_type"`
	ProjectID             string              `json:"project_id"`
	TaskID                string              `json:"task_id"`
	TaskRef               string              `json:"task_ref"`
	Repository            string              `json:"repository"`
	LifecycleRepository   string              `json:"lifecycle_repository"`
	ProviderRevision      string              `json:"provider_revision"`
	GraphRevision         string              `json:"graph_revision"`
	RunID                 string              `json:"run_id"`
	EnvironmentPlanID     string              `json:"environment_plan_id"`
	EnvironmentPlanDigest string              `json:"environment_plan_digest"`
	RecoveryFromRevision  int64               `json:"recovery_from_revision"`
	RunRevision           int64               `json:"run_revision"`
	PriorLifecycle        lifecycle.TaskState `json:"prior_lifecycle"`
	PriorReceiptDigest    string              `json:"prior_receipt_digest"`
	NewReceiptDigest      string              `json:"new_receipt_digest"`
	PriorLeaseID          string              `json:"prior_lease_id"`
	NewLeaseID            string              `json:"new_lease_id"`
	PriorBaseSHA          string              `json:"prior_base_sha"`
	NewBaseSHA            string              `json:"new_base_sha"`
	Branch                string              `json:"branch"`
	AnchorRef             string              `json:"anchor_ref"`
	WorktreeHead          string              `json:"worktree_head"`
	FailedVerdictDigest   string              `json:"failed_verdict_digest"`
	CallbackDigest        string              `json:"callback_digest"`
}

func (r *LifecycleGenerationRebinder) RebindRecovered(ctx context.Context, req RecoveredLifecycleRequest) (*lifecycle.GenerationRebindResult, error) {
	if err := r.validateConfiguration(); err != nil {
		return nil, err
	}
	if err := validateRecoveredLifecycleRequest(req); err != nil {
		return nil, err
	}
	now := r.Now().UTC()
	plan, err := r.EnvironmentPlans.Load(ctx, req.EnvironmentPlanID)
	if err != nil {
		return nil, refuseRecovered("environment plan readback", err)
	}
	if plan.ID != req.EnvironmentPlanID || plan.Binding != req.Binding || !plan.Binding.Recovered() ||
		!now.Before(plan.ExpiresAt) {
		return nil, refuseRecovered("environment plan identity", errors.New("recovered provider/task/graph/run binding or expiry changed"))
	}
	for _, request := range plan.Requests {
		if err := r.EnvironmentPlans.Authorize(ctx, plan.ID, req.Binding, request.Capability, now); err != nil {
			return nil, refuseRecovered("environment plan authorization", err)
		}
	}

	verifier, err := LoadVerifier(r.RepoRoot)
	if err != nil {
		return nil, refuseRecovered("receipt verifier", err)
	}
	if err := verifier.Verify(req.NewReceipt); err != nil {
		return nil, refuseRecovered("new receipt signature", err)
	}
	if err := exactNewReceiptIdentity(req); err != nil {
		return nil, err
	}
	if !now.Before(req.NewReceipt.ExpiresAt) {
		return nil, refuseRecovered("new receipt expiry", fmt.Errorf("receipt expired at %s", req.NewReceipt.ExpiresAt.UTC().Format(time.RFC3339Nano)))
	}

	canonical, err := LoadCanonicalReceiptSession(r.RepoRoot, req.ProviderType, req.ProjectID, req.TaskRef, req.NewReceipt.SessionID)
	if err != nil || !canonical.EqualsIssued(req.NewReceipt) {
		return nil, refuseRecovered("canonical new receipt readback", err)
	}
	if err := verifier.Verify(canonical); err != nil {
		return nil, refuseRecovered("canonical new receipt signature", err)
	}
	worktreeReceipt, err := ReadTaskContext(req.WorktreePath)
	if err != nil || !worktreeReceipt.EqualsIssued(req.NewReceipt) {
		return nil, refuseRecovered("worktree receipt readback", err)
	}
	if err := verifier.Verify(worktreeReceipt); err != nil {
		return nil, refuseRecovered("worktree receipt signature", err)
	}
	if err := validateRecoveredPacket(req); err != nil {
		return nil, err
	}
	if err := r.validateWorktree(ctx, req); err != nil {
		return nil, err
	}

	if _, err := os.Stat(r.LifecyclePath); err != nil {
		return nil, refuseRecovered("lifecycle state", err)
	}
	machine, err := openLifecycleMachine(r.LifecyclePath)
	if err != nil {
		return nil, refuseRecovered("open lifecycle", err)
	}
	defer machine.Close()
	currentState, err := machine.EventStore().CurrentState(req.TaskRef)
	if err != nil || currentState == nil {
		return nil, refuseRecovered("prior lifecycle read", err)
	}
	priorState := *currentState
	switch currentState.LeaseGeneration {
	case req.LeaseGeneration - 1:
		// Fresh CAS uses the exact generation-(N-1) snapshot.
	case req.LeaseGeneration:
		// An identical retry may observe the already materialized state. Rebuild
		// the immutable pre-transaction snapshot; RebindGeneration then proves
		// the exact two event keys and evidence before returning a replay.
		if currentState.Seq < 3 {
			return nil, refuseRecovered("prior lifecycle replay", fmt.Errorf("state=%+v", currentState))
		}
		priorState.Seq -= 2
		priorState.LeaseGeneration--
		events, eventsErr := machine.EventStore().Events(req.TaskRef)
		if eventsErr != nil {
			return nil, refuseRecovered("prior lifecycle replay history", eventsErr)
		}
		found := false
		for _, event := range events {
			if event.Seq == priorState.Seq && event.ToState == priorState.State &&
				event.LeaseGeneration == priorState.LeaseGeneration && event.Branch == priorState.Branch &&
				event.CandidateSHA == priorState.CandidateSHA {
				priorState.UpdatedAt = event.CreatedAt
				found = true
				break
			}
		}
		if !found {
			return nil, refuseRecovered("prior lifecycle replay history", errors.New("immutable prior event is absent or conflicts"))
		}
	default:
		return nil, refuseRecovered("prior lifecycle generation", fmt.Errorf("held=%d requested=%d", currentState.LeaseGeneration, req.LeaseGeneration))
	}
	if priorState.State != lifecycle.StateEligible || priorState.Seq < 1 || priorState.LeaseGeneration != req.LeaseGeneration-1 ||
		priorState.Repo != req.LifecycleRepository || priorState.Branch != req.Branch || !exactCommit(priorState.CandidateSHA) {
		return nil, refuseRecovered("prior lifecycle identity", fmt.Errorf("state=%+v", priorState))
	}

	priorReceipt, err := r.loadPriorReceipt(verifier, req, priorState)
	if err != nil {
		return nil, err
	}
	if err := r.validateCandidateReachability(ctx, priorReceipt, priorState); err != nil {
		return nil, err
	}
	verdictDigest, err := r.validateFailedVerdict(priorState, req)
	if err != nil {
		return nil, err
	}
	callbackDigest, err := r.validateCallbackHistory(priorState, req)
	if err != nil {
		return nil, err
	}

	evidence := recoveredLifecycleEvidence{
		Version: 1, ProviderType: req.ProviderType, ProjectID: req.ProjectID,
		TaskID: req.TaskID, TaskRef: req.TaskRef, Repository: req.Repository, LifecycleRepository: req.LifecycleRepository,
		ProviderRevision: req.Binding.ProviderRevision, GraphRevision: req.Binding.GraphRevision,
		RunID: req.Binding.RunID, EnvironmentPlanID: req.EnvironmentPlanID, EnvironmentPlanDigest: jsonDigest(plan),
		RecoveryFromRevision: req.Binding.RecoveryFromRevision,
		RunRevision:          req.Binding.RunRevision, PriorLifecycle: priorState,
		PriorReceiptDigest: receiptDigest(priorReceipt), NewReceiptDigest: receiptDigest(req.NewReceipt),
		PriorLeaseID: priorReceipt.LeaseID, NewLeaseID: req.LeaseID,
		PriorBaseSHA: priorReceipt.BaseSHA, NewBaseSHA: req.BaseSHA,
		Branch: req.Branch, AnchorRef: req.AnchorRef, WorktreeHead: req.WorktreeHead,
		FailedVerdictDigest: verdictDigest, CallbackDigest: callbackDigest,
	}
	payload, err := json.Marshal(evidence)
	if err != nil {
		return nil, refuseRecovered("marshal evidence", err)
	}
	digest := sha256.Sum256(payload)
	evidenceDigest := "sha256:" + hex.EncodeToString(digest[:])
	key := fmt.Sprintf("dispatch-recovery:%s:lease:%d:%d", strings.ToLower(req.TaskRef), priorState.LeaseGeneration, req.LeaseGeneration)
	result, err := machine.RebindGeneration(lifecycle.GenerationRebindRequest{
		Expected: priorState, LeaseGeneration: req.LeaseGeneration,
		ProviderRevision: req.Binding.ProviderRevision, Actor: "coordinator-dispatch",
		IdempotencyKey: key, EvidenceDigest: evidenceDigest, Payload: string(payload),
	})
	if err != nil {
		return nil, refuseRecovered("atomic lifecycle CAS", err)
	}
	if result.State.LeaseGeneration != req.LeaseGeneration || result.State.State != priorState.State ||
		result.State.Branch != priorState.Branch || result.State.CandidateSHA != priorState.CandidateSHA {
		return nil, refuseRecovered("lifecycle readback", fmt.Errorf("state=%+v", result.State))
	}
	return &result, nil
}

func openLifecycleMachine(path string) (*lifecycle.Machine, error) {
	var lastBusy error
	for attempt := 0; attempt < 12; attempt++ {
		machine, err := lifecycle.NewMachine(path)
		if err == nil {
			return machine, nil
		}
		message := strings.ToLower(err.Error())
		if !strings.Contains(message, "database is locked") && !strings.Contains(message, "sqlite_busy") {
			return nil, err
		}
		lastBusy = err
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	return nil, fmt.Errorf("concurrent lifecycle open did not settle: %w", lastBusy)
}

func (r *LifecycleGenerationRebinder) validateConfiguration() error {
	if r == nil || strings.TrimSpace(r.RepoRoot) == "" || strings.TrimSpace(r.LifecyclePath) == "" ||
		strings.TrimSpace(r.ReviewLedgerPath) == "" || strings.TrimSpace(r.CallbackPath) == "" || r.EnvironmentPlans == nil || r.Now == nil {
		return refuseRecovered("configuration", errors.New("root, lifecycle, review, callback, environment-plan, and clock authorities are required"))
	}
	return nil
}

func validateRecoveredLifecycleRequest(req RecoveredLifecycleRequest) error {
	if !req.Binding.Recovered() || req.Binding.RunID != "dispatch:"+req.TaskID ||
		req.Binding.TaskRef != req.TaskRef || req.Binding.TaskID != req.TaskID ||
		req.Binding.Provider != req.ProviderType || strings.TrimSpace(req.Binding.ProviderRevision) == "" ||
		strings.TrimSpace(req.Binding.GraphRevision) == "" {
		return refuseRecovered("recovered run binding", errors.New("exact explicit recovered task/provider/graph/run identity is required"))
	}
	for _, field := range []string{req.EnvironmentPlanID, req.ProviderType, req.ProjectID, req.TaskID, req.TaskRef, req.Repository, req.LifecycleRepository, req.LeaseID,
		req.Branch, req.WorktreePath, req.WorktreeHead, req.BaseSHA, req.AnchorRef, req.TaskPacket} {
		if strings.TrimSpace(field) == "" {
			return refuseRecovered("request identity", errors.New("all project/task/repository/lease/branch/worktree/base/packet fields are required"))
		}
	}
	if req.LeaseGeneration < 2 || !exactCommit(req.WorktreeHead) || !exactCommit(req.BaseSHA) {
		return refuseRecovered("request lease or commit identity", errors.New("positive recovered generation and exact commits are required"))
	}
	return nil
}

func exactNewReceiptIdentity(req RecoveredLifecycleRequest) error {
	tc := req.NewReceipt
	if tc.ProviderType != req.ProviderType || tc.ProjectID != req.ProjectID || tc.TaskID != req.TaskID ||
		tc.TaskRef != req.TaskRef || tc.Repository != req.Repository || tc.LeaseID != req.LeaseID ||
		tc.LeaseGeneration != req.LeaseGeneration || tc.LeaseTaskRef != req.TaskRef ||
		tc.Branch != req.Branch || tc.BaseSHA != req.BaseSHA || tc.AnchorRef != req.AnchorRef || tc.Role != RoleWorker ||
		tc.CandidateSHA != "" || tc.HerdrWorkspace != "" || tc.AgentSessionID != "" {
		return refuseRecovered("new receipt identity", errors.New("signed receipt does not exactly match dispatch project/task/repository/lease/branch/base/anchor"))
	}
	return nil
}

func validateRecoveredPacket(req RecoveredLifecycleRequest) error {
	path := filepath.Join(req.WorktreePath, TaskPacketFile)
	body, err := os.ReadFile(path)
	if err != nil {
		return refuseRecovered("task packet readback", err)
	}
	if string(body) != req.TaskPacket {
		return refuseRecovered("task packet readback", errors.New("packet differs from coordinator-issued bytes"))
	}
	wants := []string{
		fmt.Sprintf("branch %s", req.Branch),
		fmt.Sprintf("provider=%s project=%s", req.ProviderType, req.ProjectID),
		fmt.Sprintf("task_ref: %s; task_id: %s; lease_generation: %d", req.TaskRef, req.TaskID, req.LeaseGeneration),
		fmt.Sprintf("herd shot %s --report complete --sha <sha> --lease %d", req.TaskRef, req.LeaseGeneration),
	}
	for _, want := range wants {
		if !strings.Contains(req.TaskPacket, want) {
			return refuseRecovered("task packet identity", fmt.Errorf("missing exact binding %q", want))
		}
	}
	return nil
}

func (r *LifecycleGenerationRebinder) validateWorktree(ctx context.Context, req RecoveredLifecycleRequest) error {
	path, err := filepath.Abs(req.WorktreePath)
	if err != nil {
		return refuseRecovered("worktree path", err)
	}
	run := func(dir string, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	top, err := gitroot.Toplevel(ctx, path)
	if err != nil || !sameExistingPath(top, path) {
		return refuseRecovered("worktree root", err)
	}
	branch, err := run(path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != req.Branch {
		return refuseRecovered("worktree branch", err)
	}
	head, err := run(path, "rev-parse", "HEAD")
	if err != nil || head != req.WorktreeHead {
		return refuseRecovered("worktree head", err)
	}
	if contains, err := gitroot.CommitIsAncestor(ctx, path, req.BaseSHA, req.WorktreeHead); err != nil || !contains {
		return refuseRecovered("worktree base ancestry", err)
	}
	statusOutput, statusErr := exec.CommandContext(ctx, "git", "-C", path, "status", "--porcelain=v1", "--untracked-files=all").CombinedOutput()
	status := strings.TrimRight(string(statusOutput), "\r\n")
	err = statusErr
	if err != nil {
		return refuseRecovered("worktree cleanliness", fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(string(statusOutput))))
	}
	for _, line := range strings.Split(status, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 4 {
			return refuseRecovered("worktree cleanliness", fmt.Errorf("malformed porcelain status %q", line))
		}
		name := strings.TrimSpace(line[3:])
		if name != TaskPacketFile && name != TaskContextFile {
			return refuseRecovered("worktree cleanliness", fmt.Errorf("unexpected dirty path %s", name))
		}
	}
	listed, err := run(r.RepoRoot, "worktree", "list", "--porcelain")
	if err != nil || !registeredWorktreeMatches(listed, path, req.WorktreeHead, req.Branch) {
		return refuseRecovered("worktree registration", err)
	}
	anchor, err := run(r.RepoRoot, "rev-parse", req.AnchorRef+"^{commit}")
	if err != nil || anchor != req.BaseSHA {
		return refuseRecovered("worktree anchor/base", err)
	}
	return nil
}

func registeredWorktreeMatches(listed, path, head, branch string) bool {
	blocks := strings.Split(strings.TrimSpace(listed), "\n\n")
	for _, block := range blocks {
		fields := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(line, " ")
			if ok {
				fields[key] = value
			}
		}
		if sameExistingPath(fields["worktree"], path) && fields["HEAD"] == head && fields["branch"] == "refs/heads/"+branch {
			return true
		}
	}
	return false
}

func sameExistingPath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func (r *LifecycleGenerationRebinder) loadPriorReceipt(verifier *Verifier, req RecoveredLifecycleRequest, state lifecycle.TaskState) (TaskContext, error) {
	dir := filepath.Join(r.RepoRoot, CanonicalTaskContextDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return TaskContext{}, refuseRecovered("prior receipt inventory", err)
	}
	prefix := strings.ToLower(req.TaskRef) + "-"
	var matches []TaskContext
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		tc, err := readCanonicalFile(filepath.Join(dir, entry.Name()), req.TaskRef)
		if err != nil {
			return TaskContext{}, refuseRecovered("prior receipt inventory", err)
		}
		if err := verifier.Verify(tc); err != nil {
			return TaskContext{}, refuseRecovered("prior receipt signature", err)
		}
		if tc.LeaseGeneration == state.LeaseGeneration && tc.ProviderType == req.ProviderType && tc.ProjectID == req.ProjectID &&
			tc.TaskID == req.TaskID && tc.TaskRef == req.TaskRef && tc.Repository == req.Repository && tc.Role == RoleWorker &&
			tc.LeaseTaskRef == req.TaskRef && tc.Branch == req.Branch && tc.AnchorRef == req.AnchorRef {
			matches = append(matches, tc)
		}
	}
	if len(matches) != 1 {
		return TaskContext{}, refuseRecovered("prior receipt identity", fmt.Errorf("found %d exact generation-%d receipts", len(matches), state.LeaseGeneration))
	}
	prior := matches[0]
	if prior.LeaseID == req.LeaseID || prior.SessionID == req.NewReceipt.SessionID || prior.LeaseGeneration+1 != req.LeaseGeneration ||
		prior.ProviderWorkspace != req.NewReceipt.ProviderWorkspace || prior.ProviderProfile != req.NewReceipt.ProviderProfile ||
		prior.AllowedOps == nil || strings.Join(prior.AllowedOps, "\x00") != strings.Join(req.NewReceipt.AllowedOps, "\x00") {
		return TaskContext{}, refuseRecovered("prior/new receipt transition", errors.New("receipt generation, lease, session, provider profile, or operation identity conflicts"))
	}
	return prior, nil
}

func (r *LifecycleGenerationRebinder) validateCandidateReachability(ctx context.Context, prior TaskContext, state lifecycle.TaskState) error {
	run := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", r.RepoRoot}, args...)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if !exactCommit(prior.BaseSHA) {
		return refuseRecovered("prior receipt base", errors.New("prior base is not an exact commit"))
	}
	if _, err := run("cat-file", "-e", state.CandidateSHA+"^{commit}"); err != nil {
		return refuseRecovered("prior candidate object", err)
	}
	if contains, err := gitroot.CommitIsAncestor(ctx, r.RepoRoot, prior.BaseSHA, state.CandidateSHA); err != nil || !contains {
		return refuseRecovered("prior candidate/base ancestry", err)
	}
	refs, err := run("for-each-ref", "--format=%(refname)", "--contains", state.CandidateSHA)
	if err != nil || strings.TrimSpace(refs) == "" {
		return refuseRecovered("prior candidate reachability", err)
	}
	return nil
}

func (r *LifecycleGenerationRebinder) validateFailedVerdict(state lifecycle.TaskState, req RecoveredLifecycleRequest) (string, error) {
	rows, err := readReviewRowsStrict(r.ReviewLedgerPath)
	if err != nil {
		return "", refuseRecovered("review ledger", err)
	}
	queue, err := readReviewRowsStrict(reviewledger.QueuePathFor(r.ReviewLedgerPath))
	if err != nil {
		return "", refuseRecovered("review queue", err)
	}
	records := map[string]reviewledger.LedgerRow{}
	verdicts := map[string]reviewledger.LedgerRow{}
	var evidence []reviewledger.LedgerRow
	for _, row := range rows {
		if row.SHA != state.CandidateSHA && row.CandidateSHA != state.CandidateSHA {
			continue
		}
		evidence = append(evidence, row)
		if row.Task != "" && row.Task != req.TaskRef {
			return "", refuseRecovered("failed verdict task", fmt.Errorf("candidate row belongs to %s", row.Task))
		}
		switch reviewledger.Event(row.Event) {
		case reviewledger.EventRecord:
			if row.SHA != state.CandidateSHA || row.Task != req.TaskRef || row.Branch != req.Branch || strings.TrimSpace(row.Reviewer) == "" {
				return "", refuseRecovered("review record identity", errors.New("record omitted exact task/candidate/branch/reviewer"))
			}
			records[row.Reviewer] = row
		case reviewledger.EventVerdict:
			if row.SHA != state.CandidateSHA || row.CandidateSHA != state.CandidateSHA || row.Task != req.TaskRef || strings.TrimSpace(row.Reviewer) == "" {
				return "", refuseRecovered("failed verdict identity", errors.New("verdict omitted exact task/candidate/reviewer"))
			}
			verdicts[row.Reviewer] = row
		case reviewledger.EventConsumed, reviewledger.EventSupersession:
			return "", refuseRecovered("merge admission", fmt.Errorf("candidate has %s evidence", row.Event))
		}
	}
	if len(records) == 0 {
		return "", refuseRecovered("failed verdict", errors.New("no exact review record"))
	}
	failures := 0
	for reviewer := range records {
		verdict, ok := verdicts[reviewer]
		if !ok {
			return "", refuseRecovered("active reviewer", fmt.Errorf("reviewer %s has no verdict", reviewer))
		}
		if verdict.Verdict != string(reviewledger.VerdictFAIL) {
			return "", refuseRecovered("failed verdict", fmt.Errorf("reviewer %s verdict is %s", reviewer, verdict.Verdict))
		}
		if verdict.Task != req.TaskRef || verdict.CandidateSHA != state.CandidateSHA {
			return "", refuseRecovered("failed verdict identity", fmt.Errorf("reviewer %s omitted exact task/candidate", reviewer))
		}
		failures++
	}
	if failures == 0 {
		return "", refuseRecovered("failed verdict", errors.New("no exact FAIL"))
	}
	latestQueue := ""
	for _, row := range queue {
		if row.SHA == state.CandidateSHA {
			if row.Branch != req.Branch || strings.TrimSpace(row.Reviewer) == "" {
				return "", refuseRecovered("merge admission queue identity", errors.New("queue row omitted exact branch or reviewer"))
			}
			latestQueue = row.Event
			evidence = append(evidence, row)
		}
	}
	if latestQueue != string(reviewledger.EventRevoked) {
		return "", refuseRecovered("merge admission queue", fmt.Errorf("latest candidate queue event is %q", latestQueue))
	}
	return jsonDigest(evidence), nil
}

func (r *LifecycleGenerationRebinder) validateCallbackHistory(state lifecycle.TaskState, req RecoveredLifecycleRequest) (string, error) {
	fh, err := os.Open(r.CallbackPath)
	if err != nil {
		return "", refuseRecovered("callback history", err)
	}
	defer fh.Close()
	var exact []mail.Callback
	priorCompletion := 0
	expectedSender := "shot:" + strings.ToLower(req.TaskRef)
	scanner := bufio.NewScanner(fh)
	scanner.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var envelope mail.Envelope
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			return "", refuseRecovered("callback history", err)
		}
		if envelope.Recipient != mail.CoordinatorInbox || (envelope.Subject != "complete: "+req.TaskRef && envelope.Subject != "blocked: "+req.TaskRef) {
			continue
		}
		var callback mail.Callback
		if err := json.Unmarshal([]byte(envelope.Body), &callback); err != nil {
			return "", refuseRecovered("callback history", err)
		}
		if callback.Ref != req.TaskRef {
			return "", refuseRecovered("callback identity", errors.New("subject/body task mismatch"))
		}
		if envelope.Sender != expectedSender || callback.SenderRole != expectedSender ||
			(callback.Repo != "" && callback.Repo != req.Repository) {
			return "", refuseRecovered("callback identity", errors.New("callback sender or optional repository differs from the exact shot/task binding"))
		}
		exact = append(exact, callback)
		if callback.Kind == mail.CallbackComplete {
			if callback.LeaseGeneration == state.LeaseGeneration && callback.SHA == state.CandidateSHA {
				priorCompletion++
				continue
			}
			return "", refuseRecovered("callback state", fmt.Errorf("conflicting completion at generation %d candidate %s", callback.LeaseGeneration, callback.SHA))
		}
		if callback.Kind != mail.CallbackBlocked {
			return "", refuseRecovered("callback kind", fmt.Errorf("unknown kind %s", callback.Kind))
		}
		if callback.LeaseGeneration != state.LeaseGeneration && callback.LeaseGeneration != req.LeaseGeneration {
			return "", refuseRecovered("callback lease", fmt.Errorf("blocked callback generation %d is outside recovery pair", callback.LeaseGeneration))
		}
	}
	if err := scanner.Err(); err != nil {
		return "", refuseRecovered("callback history", err)
	}
	if priorCompletion != 1 {
		return "", refuseRecovered("prior callback", fmt.Errorf("found %d exact generation-%d completions", priorCompletion, state.LeaseGeneration))
	}
	return jsonDigest(exact), nil
}

func readReviewRowsStrict(path string) ([]reviewledger.LedgerRow, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var rows []reviewledger.LedgerRow
	scanner := bufio.NewScanner(fh)
	scanner.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row reviewledger.LedgerRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		if strings.TrimSpace(row.Event) == "" {
			return nil, errors.New("review row omitted event")
		}
		rows = append(rows, row)
	}
	return rows, scanner.Err()
}

func receiptDigest(tc TaskContext) string { return jsonDigest(tc) }

func jsonDigest(value any) string {
	body, _ := json.Marshal(value)
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func exactCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func refuseRecovered(boundary string, cause error) error {
	if cause == nil {
		cause = errors.New("readback mismatch")
	}
	return fmt.Errorf("%w: %s: %w", ErrRecoveredLifecycleRefused, boundary, cause)
}
