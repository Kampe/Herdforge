package lifecycle

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/ledger"
	"github.com/Kampe/Herdforge/pkg/outbox"
)

var (
	ErrHumanPrincipalRequired = errors.New("lifecycle service: human principal required")
	ErrWorktreeNotOwned       = errors.New("lifecycle service: active task worktree is not owned")
	ErrSharedCheckout         = errors.New("lifecycle service: shared/root checkout is forbidden")
	ErrCandidateMismatch      = errors.New("lifecycle service: candidate does not match current immutable candidate")
	ErrEvidenceMissing        = errors.New("lifecycle service: exact candidate evidence is required")
	ErrApprovalMissing        = errors.New("lifecycle service: merge approval is missing")
	ErrApprovalStale          = errors.New("lifecycle service: merge approval is stale")
	ErrApprovalRejected       = errors.New("lifecycle service: merge approval was rejected")
	ErrApprovalSHAMismatch    = errors.New("lifecycle service: merge approval candidate SHA mismatch")
	ErrIntegrationLocked      = errors.New("lifecycle service: integration target is owned by another task")
	ErrCommandConflict        = errors.New("lifecycle service: idempotency key reused for another command")
)

var (
	exactSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Identity keeps Phase-1 Actor and Principal records private/local while
// making every command attributable. There is deliberately no transport or
// public authentication endpoint in this package.
type Identity struct {
	Actor     ledger.Actor     `json:"actor"`
	Principal ledger.Principal `json:"principal"`
}

// CommandContext is the common fenced context for lifecycle service commands.
// CheckoutPath is a runtime input only; persisted worktree paths are portable,
// repository-relative paths from ledger.OwnedWorktree.
type CommandContext struct {
	IdempotencyKey  string   `json:"idempotency_key"`
	TaskRef         string   `json:"task_ref"`
	Repo            string   `json:"repo"`
	CheckoutPath    string   `json:"checkout_path"`
	LeaseGeneration int64    `json:"lease_generation"`
	Identity        Identity `json:"identity"`
}

// CommandResult is durably stored with the idempotency key and returned on
// replay, including after reopening the SQLite database.
type CommandResult struct {
	Kind         string  `json:"kind"`
	TaskRef      string  `json:"task_ref"`
	State        State   `json:"state,omitempty"`
	CandidateID  string  `json:"candidate_id,omitempty"`
	CandidateSHA string  `json:"candidate_sha,omitempty"`
	ApprovalID   string  `json:"approval_id,omitempty"`
	Events       []Event `json:"events,omitempty"`
	Replayed     bool    `json:"replayed"`
}

// WorktreeFacts are authoritative facts read from Git for one linked checkout.
type WorktreeFacts struct {
	CheckoutPath string
	SharedRoot   string
	PortablePath string
	Branch       string
	HeadSHA      string
}

// WorktreeInspector permits deterministic tests while the default inspector
// performs real Git topology checks and rejects the principal/root checkout.
type WorktreeInspector interface {
	Inspect(context.Context, string) (WorktreeFacts, error)
}

type gitWorktreeInspector struct{}

func (gitWorktreeInspector) Inspect(ctx context.Context, path string) (WorktreeFacts, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return WorktreeFacts{}, fmt.Errorf("inspect worktree path: %w", err)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", abs}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	top, err := run("rev-parse", "--show-toplevel")
	if err != nil {
		return WorktreeFacts{}, err
	}
	common, err := run("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return WorktreeFacts{}, err
	}
	branch, err := run("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return WorktreeFacts{}, fmt.Errorf("linked task worktree must have a branch: %w", err)
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return WorktreeFacts{}, err
	}
	top, _ = filepath.Abs(top)
	shared := filepath.Dir(common)
	shared, _ = filepath.Abs(shared)
	if samePath(top, shared) {
		return WorktreeFacts{}, ErrSharedCheckout
	}
	rel, err := filepath.Rel(shared, top)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return WorktreeFacts{}, fmt.Errorf("%w: checkout must be inside repository worktree root", ErrSharedCheckout)
	}
	if !samePath(abs, top) {
		return WorktreeFacts{}, fmt.Errorf("checkout_path must name the worktree root %s", top)
	}
	return WorktreeFacts{
		CheckoutPath: top,
		SharedRoot:   shared,
		PortablePath: "./" + filepath.ToSlash(rel),
		Branch:       branch,
		HeadSHA:      head,
	}, nil
}

func samePath(a, b string) bool {
	a, _ = filepath.EvalSymlinks(filepath.Clean(a))
	b, _ = filepath.EvalSymlinks(filepath.Clean(b))
	return a == b
}

// Service is the private Phase-1 lifecycle command layer. It composes the
// canonical Machine/EventStore/outbox transaction primitives; it is not a
// second state engine.
type Service struct {
	machine   *Machine
	inspector WorktreeInspector
	now       func() time.Time
}

type ServiceOption func(*Service)

func WithWorktreeInspector(i WorktreeInspector) ServiceOption {
	return func(s *Service) { s.inspector = i }
}

func WithServiceClock(now func() time.Time) ServiceOption {
	return func(s *Service) { s.now = now }
}

func NewService(machine *Machine, opts ...ServiceOption) (*Service, error) {
	if machine == nil {
		return nil, errors.New("lifecycle service: machine is required")
	}
	s := &Service{machine: machine, inspector: gitWorktreeInspector{}, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	if s.inspector == nil || s.now == nil {
		return nil, errors.New("lifecycle service: inspector and clock are required")
	}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS lifecycle_service_commands (
			idempotency_key TEXT PRIMARY KEY, kind TEXT NOT NULL, request_digest TEXT NOT NULL,
			result_json TEXT NOT NULL, created_at DATETIME NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lifecycle_service_actors (
			id TEXT PRIMARY KEY, record_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lifecycle_service_principals (
			id TEXT PRIMARY KEY, actor_id TEXT NOT NULL, kind TEXT NOT NULL, record_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lifecycle_service_owned_worktrees (
			id TEXT PRIMARY KEY, task_ref TEXT NOT NULL UNIQUE, repo TEXT NOT NULL,
			portable_path TEXT NOT NULL, branch TEXT NOT NULL, base_sha TEXT NOT NULL,
			owner_principal_id TEXT NOT NULL, record_json TEXT NOT NULL,
			created_at DATETIME NOT NULL, released_at DATETIME)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_lifecycle_service_active_worktree
			ON lifecycle_service_owned_worktrees(repo, portable_path) WHERE released_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS lifecycle_service_plan_approvals (
			task_ref TEXT NOT NULL, plan_digest TEXT NOT NULL, approver_principal_id TEXT NOT NULL,
			command_key TEXT NOT NULL UNIQUE, created_at DATETIME NOT NULL,
			PRIMARY KEY(task_ref, plan_digest))`,
		`CREATE TABLE IF NOT EXISTS lifecycle_service_candidates (
			id TEXT PRIMARY KEY, task_ref TEXT NOT NULL, git_sha TEXT NOT NULL, base_sha TEXT NOT NULL,
			evidence_digest TEXT NOT NULL, record_json TEXT NOT NULL, created_at DATETIME NOT NULL,
			UNIQUE(task_ref, git_sha))`,
		`CREATE TABLE IF NOT EXISTS lifecycle_service_receipts (
			id TEXT PRIMARY KEY, candidate_id TEXT NOT NULL, candidate_sha TEXT NOT NULL,
			kind TEXT NOT NULL, outcome TEXT NOT NULL, evidence_digest TEXT NOT NULL,
			record_json TEXT NOT NULL, created_at DATETIME NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lifecycle_service_reviews (
			id TEXT PRIMARY KEY, candidate_id TEXT NOT NULL, candidate_sha TEXT NOT NULL,
			outcome TEXT NOT NULL, record_json TEXT NOT NULL, created_at DATETIME NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lifecycle_service_approvals (
			seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE,
			candidate_id TEXT NOT NULL, candidate_sha TEXT NOT NULL,
			decision TEXT NOT NULL, approver_principal_id TEXT NOT NULL,
			record_json TEXT NOT NULL, created_at DATETIME NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lifecycle_service_integration_locks (
			target_branch TEXT PRIMARY KEY, task_ref TEXT NOT NULL, candidate_id TEXT NOT NULL,
			candidate_sha TEXT NOT NULL, owner_principal_id TEXT NOT NULL,
			command_key TEXT NOT NULL UNIQUE, acquired_at DATETIME NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := s.machine.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate lifecycle service: %w", err)
		}
	}
	return nil
}

type OwnWorktreeRequest struct {
	Command    CommandContext `json:"command"`
	WorktreeID string         `json:"worktree_id"`
	Branch     string         `json:"branch"`
	BaseSHA    string         `json:"base_sha"`
}

// OwnWorktree registers a linked, non-root checkout as the sole active
// worktree for a task. Registration is idempotent but deliberately does not
// invent a state transition; ApprovePlan remains the human Draft->Eligible
// gate and StartWork advances through the canonical ownership/build states.
func (s *Service) OwnWorktree(ctx context.Context, req OwnWorktreeRequest) (CommandResult, error) {
	if req.WorktreeID == "" || req.Branch == "" || !validSHA(req.BaseSHA) {
		return CommandResult{}, errors.New("own worktree: id, branch, and exact base SHA are required")
	}
	return s.run(ctx, "worktree.own", req.Command, req, false, func(f WorktreeFacts) error {
		if f.Branch != req.Branch {
			return fmt.Errorf("own worktree: branch %q does not match checkout branch %q", req.Branch, f.Branch)
		}
		if f.HeadSHA != req.BaseSHA {
			return fmt.Errorf("own worktree: checkout HEAD %s does not match base SHA %s", f.HeadSHA, req.BaseSHA)
		}
		return nil
	}, func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		now := s.now().UTC()
		record := ledger.OwnedWorktree{
			Version: ledger.Version1, ID: req.WorktreeID, RunID: req.Command.TaskRef,
			Path: f.PortablePath, Branch: req.Branch, BaseSHA: req.BaseSHA,
			OwnerID: req.Command.Identity.Principal.ID, CreatedAt: now,
		}
		blob, _ := json.Marshal(record)
		var existingTask string
		err := tx.QueryRow(`SELECT task_ref FROM lifecycle_service_owned_worktrees WHERE task_ref = ? AND released_at IS NULL`, req.Command.TaskRef).Scan(&existingTask)
		if err == nil {
			return CommandResult{}, fmt.Errorf("%w: task %s already has an active worktree", ErrWorktreeNotOwned, existingTask)
		}
		if err != sql.ErrNoRows {
			return CommandResult{}, err
		}
		_, err = tx.Exec(`INSERT INTO lifecycle_service_owned_worktrees
			(id, task_ref, repo, portable_path, branch, base_sha, owner_principal_id, record_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.ID, req.Command.TaskRef, req.Command.Repo,
			record.Path, record.Branch, record.BaseSHA, record.OwnerID, string(blob), now)
		if err != nil {
			return CommandResult{}, fmt.Errorf("own worktree: %w", err)
		}
		return CommandResult{Kind: "worktree.own", TaskRef: req.Command.TaskRef, State: StateDraft}, nil
	})
}

type ApprovePlanRequest struct {
	Command    CommandContext `json:"command"`
	PlanDigest string         `json:"plan_digest"`
}

func (s *Service) ApprovePlan(ctx context.Context, req ApprovePlanRequest) (CommandResult, error) {
	if !validDigest(req.PlanDigest) {
		return CommandResult{}, fmt.Errorf("approve plan: %w", ErrEvidenceMissing)
	}
	return s.run(ctx, "plan.approve", req.Command, req, true, nil, func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		if !req.Command.Identity.human() {
			return CommandResult{}, ErrHumanPrincipalRequired
		}
		now := s.now().UTC()
		if _, err := tx.Exec(`INSERT INTO lifecycle_service_plan_approvals
			(task_ref, plan_digest, approver_principal_id, command_key, created_at) VALUES (?, ?, ?, ?, ?)`,
			req.Command.TaskRef, req.PlanDigest, req.Command.Identity.Principal.ID, req.Command.IdempotencyKey, now); err != nil {
			return CommandResult{}, fmt.Errorf("approve plan: record approval: %w", err)
		}
		res, err := s.transition(tx, req.Command, f, StateEligible, req.PlanDigest, "", nil, "plan-approved")
		return commandResult("plan.approve", req.Command.TaskRef, res), err
	})
}

type StartWorkRequest struct {
	Command CommandContext `json:"command"`
}

// StartWork atomically walks only legal canonical edges
// Eligible->Claimed->Dispatched->Building. A crash can commit all three or
// none, and replay returns the same event set.
func (s *Service) StartWork(ctx context.Context, req StartWorkRequest) (CommandResult, error) {
	return s.run(ctx, "work.start", req.Command, req, true, nil, func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		states := []State{StateClaimed, StateDispatched, StateBuilding}
		var events []Event
		for i, state := range states {
			res, err := s.transitionWithKey(tx, req.Command, f, state, "", "", nil,
				fmt.Sprintf("%s:step:%d", req.Command.IdempotencyKey, i+1), "work-started")
			if err != nil {
				return CommandResult{}, err
			}
			events = append(events, res.Event)
		}
		return CommandResult{Kind: "work.start", TaskRef: req.Command.TaskRef, State: StateBuilding, Events: events}, nil
	})
}

type SubmitCandidateRequest struct {
	Command   CommandContext   `json:"command"`
	Candidate ledger.Candidate `json:"candidate"`
}

func (s *Service) SubmitCandidate(ctx context.Context, req SubmitCandidateRequest) (CommandResult, error) {
	c := req.Candidate
	if c.Version != ledger.Version1 || c.ID == "" || c.RunID == "" || c.PhaseID == "" ||
		!validSHA(c.GitSHA) || !validSHA(c.BaseSHA) || !validDigest(c.EvidenceDigest) {
		return CommandResult{}, fmt.Errorf("submit candidate: invalid Phase-1 candidate: %w", ErrEvidenceMissing)
	}
	return s.run(ctx, "candidate.submit", req.Command, req, true, func(f WorktreeFacts) error {
		if f.HeadSHA != c.GitSHA {
			return fmt.Errorf("%w: checkout HEAD %s, candidate %s", ErrCandidateMismatch, f.HeadSHA, c.GitSHA)
		}
		return nil
	}, func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		c.CreatedAt = s.now().UTC()
		blob, _ := json.Marshal(c)
		if _, err := tx.Exec(`INSERT INTO lifecycle_service_candidates
			(id, task_ref, git_sha, base_sha, evidence_digest, record_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, c.ID, req.Command.TaskRef, c.GitSHA, c.BaseSHA,
			c.EvidenceDigest, string(blob), c.CreatedAt); err != nil {
			return CommandResult{}, fmt.Errorf("submit candidate: %w", err)
		}
		res, err := s.transition(tx, req.Command, f, StateVerifying, c.EvidenceDigest, c.GitSHA, nil, "candidate-submitted")
		out := commandResult("candidate.submit", req.Command.TaskRef, res)
		out.CandidateID, out.CandidateSHA = c.ID, c.GitSHA
		return out, err
	})
}

type VerificationRequest struct {
	Command      CommandContext `json:"command"`
	CandidateID  string         `json:"candidate_id"`
	CandidateSHA string         `json:"candidate_sha"`
	Outcome      string         `json:"outcome"`
	Receipt      ledger.Receipt `json:"receipt"`
}

func (s *Service) RecordVerification(ctx context.Context, req VerificationRequest) (CommandResult, error) {
	if req.Outcome != "pass" && req.Outcome != "fail" && req.Outcome != "blocked" {
		return CommandResult{}, errors.New("verification outcome must be pass, fail, or blocked")
	}
	if err := validateReceipt(req.Receipt, req.CandidateID, "verification"); err != nil {
		return CommandResult{}, err
	}
	return s.run(ctx, "candidate.verify", req.Command, req, true, candidateAtHead(req.CandidateSHA), func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		if err := s.requireCurrentCandidateTx(tx, req.Command.TaskRef, req.CandidateID, req.CandidateSHA); err != nil {
			return CommandResult{}, err
		}
		if err := insertReceiptTx(tx, req.Receipt, req.CandidateSHA, req.Outcome, s.now().UTC()); err != nil {
			return CommandResult{}, err
		}
		target := StateReviewing
		if req.Outcome == "fail" {
			target = StateBuilding
		}
		if req.Outcome == "blocked" {
			target = StateBlocked
		}
		res, err := s.transition(tx, req.Command, f, target, req.Receipt.EvidenceDigest, req.CandidateSHA, nil, "candidate-verified")
		out := commandResult("candidate.verify", req.Command.TaskRef, res)
		out.CandidateID, out.CandidateSHA = req.CandidateID, req.CandidateSHA
		return out, err
	})
}

type ReviewRequest struct {
	Command      CommandContext `json:"command"`
	CandidateID  string         `json:"candidate_id"`
	CandidateSHA string         `json:"candidate_sha"`
	Receipt      ledger.Receipt `json:"receipt"`
	Review       ledger.Review  `json:"review"`
}

func (s *Service) RecordReview(ctx context.Context, req ReviewRequest) (CommandResult, error) {
	if req.Review.Version != ledger.Version1 || req.Review.ID == "" || req.Review.CandidateID != req.CandidateID ||
		req.Review.ReceiptID != req.Receipt.ID || (req.Review.Outcome != "pass" && req.Review.Outcome != "rejected" && req.Review.Outcome != "blocked") {
		return CommandResult{}, errors.New("record review: invalid Phase-1 review")
	}
	if err := validateReceipt(req.Receipt, req.CandidateID, "review"); err != nil {
		return CommandResult{}, err
	}
	return s.run(ctx, "candidate.review", req.Command, req, true, candidateAtHead(req.CandidateSHA), func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		if err := s.requireStateTx(tx, req.Command.TaskRef, StateReviewing); err != nil {
			return CommandResult{}, err
		}
		if err := s.requireCurrentCandidateTx(tx, req.Command.TaskRef, req.CandidateID, req.CandidateSHA); err != nil {
			return CommandResult{}, err
		}
		if req.Review.ReviewerID != req.Command.Identity.Principal.ID {
			return CommandResult{}, errors.New("record review: reviewer principal mismatch")
		}
		if err := insertReceiptTx(tx, req.Receipt, req.CandidateSHA, req.Review.Outcome, s.now().UTC()); err != nil {
			return CommandResult{}, err
		}
		req.Review.CreatedAt = s.now().UTC()
		blob, _ := json.Marshal(req.Review)
		if _, err := tx.Exec(`INSERT INTO lifecycle_service_reviews (id, candidate_id, candidate_sha, outcome, record_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			req.Review.ID, req.CandidateID, req.CandidateSHA, req.Review.Outcome, string(blob), req.Review.CreatedAt); err != nil {
			return CommandResult{}, fmt.Errorf("record review: %w", err)
		}
		out := CommandResult{Kind: "candidate.review", TaskRef: req.Command.TaskRef, State: StateReviewing, CandidateID: req.CandidateID, CandidateSHA: req.CandidateSHA}
		if req.Review.Outcome != "pass" {
			target := StateBuilding
			if req.Review.Outcome == "blocked" {
				target = StateBlocked
			}
			res, err := s.transition(tx, req.Command, f, target, req.Receipt.EvidenceDigest, req.CandidateSHA, nil, "candidate-reviewed")
			if err != nil {
				return CommandResult{}, err
			}
			out.State, out.Events = target, []Event{res.Event}
		}
		return out, nil
	})
}

type PromotePRRequest struct {
	Command      CommandContext `json:"command"`
	CandidateID  string         `json:"candidate_id"`
	CandidateSHA string         `json:"candidate_sha"`
	PullRequest  string         `json:"pull_request"`
}

func (s *Service) PromotePullRequest(ctx context.Context, req PromotePRRequest) (CommandResult, error) {
	if req.PullRequest == "" {
		return CommandResult{}, errors.New("promote pull request: immutable PR reference is required")
	}
	return s.run(ctx, "pr.promote", req.Command, req, true, candidateAtHead(req.CandidateSHA), func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		if err := s.requireCurrentCandidateTx(tx, req.Command.TaskRef, req.CandidateID, req.CandidateSHA); err != nil {
			return CommandResult{}, err
		}
		if err := requirePassingEvidenceTx(tx, req.CandidateID, req.CandidateSHA); err != nil {
			return CommandResult{}, err
		}
		payload, _ := json.Marshal(map[string]string{"candidate_id": req.CandidateID, "candidate_sha": req.CandidateSHA, "pull_request": req.PullRequest})
		items := []outbox.Item{{IdempotencyKey: req.Command.IdempotencyKey + ":pr", Kind: "git.pr.promote", Payload: string(payload)}}
		res, err := s.transition(tx, req.Command, f, StateIntegrationQueued, evidenceDigest(payload), req.CandidateSHA, items, "pr-promoted")
		out := commandResult("pr.promote", req.Command.TaskRef, res)
		out.CandidateID, out.CandidateSHA = req.CandidateID, req.CandidateSHA
		return out, err
	})
}

type MergeApprovalRequest struct {
	Command      CommandContext  `json:"command"`
	CandidateID  string          `json:"candidate_id"`
	CandidateSHA string          `json:"candidate_sha"`
	Approval     ledger.Approval `json:"approval"`
}

func (s *Service) RecordMergeApproval(ctx context.Context, req MergeApprovalRequest) (CommandResult, error) {
	a := req.Approval
	if a.Version != ledger.Version1 || a.ID == "" || a.CandidateID != req.CandidateID || a.ApproverID != req.Command.Identity.Principal.ID ||
		(a.Decision != "approved" && a.Decision != "rejected") {
		return CommandResult{}, errors.New("merge approval: invalid Phase-1 approval")
	}
	return s.run(ctx, "merge.approve", req.Command, req, true, candidateAtHead(req.CandidateSHA), func(tx *sql.Tx, _ WorktreeFacts) (CommandResult, error) {
		if !req.Command.Identity.human() {
			return CommandResult{}, ErrHumanPrincipalRequired
		}
		if err := s.requireStateTx(tx, req.Command.TaskRef, StateIntegrationQueued); err != nil {
			return CommandResult{}, err
		}
		if err := s.requireCurrentCandidateTx(tx, req.Command.TaskRef, req.CandidateID, req.CandidateSHA); err != nil {
			return CommandResult{}, err
		}
		var receiptOutcome string
		if err := tx.QueryRow(`SELECT outcome FROM lifecycle_service_receipts WHERE id = ? AND candidate_id = ? AND candidate_sha = ?`,
			a.ReceiptID, req.CandidateID, req.CandidateSHA).Scan(&receiptOutcome); err != nil {
			return CommandResult{}, fmt.Errorf("merge approval: exact-candidate evidence receipt is required: %w", ErrEvidenceMissing)
		}
		if receiptOutcome != "pass" {
			return CommandResult{}, fmt.Errorf("merge approval: evidence receipt did not pass: %w", ErrEvidenceMissing)
		}
		a.CreatedAt = s.now().UTC()
		blob, _ := json.Marshal(a)
		if _, err := tx.Exec(`INSERT INTO lifecycle_service_approvals
			(id, candidate_id, candidate_sha, decision, approver_principal_id, record_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, a.ID, a.CandidateID, req.CandidateSHA, a.Decision, a.ApproverID, string(blob), a.CreatedAt); err != nil {
			return CommandResult{}, fmt.Errorf("merge approval: %w", err)
		}
		return CommandResult{Kind: "merge.approve", TaskRef: req.Command.TaskRef, State: StateIntegrationQueued,
			CandidateID: req.CandidateID, CandidateSHA: req.CandidateSHA, ApprovalID: a.ID}, nil
	})
}

type BeginIntegrationRequest struct {
	Command      CommandContext `json:"command"`
	CandidateID  string         `json:"candidate_id"`
	CandidateSHA string         `json:"candidate_sha"`
	ApprovalID   string         `json:"approval_id"`
	TargetBranch string         `json:"target_branch"`
}

func (s *Service) BeginIntegration(ctx context.Context, req BeginIntegrationRequest) (CommandResult, error) {
	if req.TargetBranch != "main" {
		return CommandResult{}, errors.New("begin integration: target branch must be main")
	}
	return s.run(ctx, "integration.begin", req.Command, req, true, candidateAtHead(req.CandidateSHA), func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		if err := s.requireStateTx(tx, req.Command.TaskRef, StateIntegrationQueued); err != nil {
			return CommandResult{}, err
		}
		if err := s.admitMergeTx(tx, req); err != nil {
			return CommandResult{}, err
		}
		now := s.now().UTC()
		if _, err := tx.Exec(`INSERT INTO lifecycle_service_integration_locks
			(target_branch, task_ref, candidate_id, candidate_sha, owner_principal_id, command_key, acquired_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, req.TargetBranch, req.Command.TaskRef, req.CandidateID,
			req.CandidateSHA, req.Command.Identity.Principal.ID, req.Command.IdempotencyKey, now); err != nil {
			var owner string
			_ = tx.QueryRow(`SELECT task_ref FROM lifecycle_service_integration_locks WHERE target_branch = ?`, req.TargetBranch).Scan(&owner)
			return CommandResult{}, fmt.Errorf("%w: %s is held by %s", ErrIntegrationLocked, req.TargetBranch, owner)
		}
		payload, _ := json.Marshal(map[string]string{"approval_id": req.ApprovalID, "candidate_id": req.CandidateID, "candidate_sha": req.CandidateSHA, "target_branch": req.TargetBranch})
		items := []outbox.Item{{IdempotencyKey: req.Command.IdempotencyKey + ":merge", Kind: "git.merge", Payload: string(payload)}}
		res, err := s.transition(tx, req.Command, f, StateIntegrated, evidenceDigest(payload), req.CandidateSHA, items, "integration-begun")
		out := commandResult("integration.begin", req.Command.TaskRef, res)
		out.CandidateID, out.CandidateSHA, out.ApprovalID = req.CandidateID, req.CandidateSHA, req.ApprovalID
		return out, err
	})
}

type CompleteIntegrationRequest struct {
	Command        CommandContext `json:"command"`
	CandidateID    string         `json:"candidate_id"`
	CandidateSHA   string         `json:"candidate_sha"`
	TargetBranch   string         `json:"target_branch"`
	EvidenceDigest string         `json:"evidence_digest"`
}

func (s *Service) CompleteIntegration(ctx context.Context, req CompleteIntegrationRequest) (CommandResult, error) {
	if req.TargetBranch != "main" || !validDigest(req.EvidenceDigest) {
		return CommandResult{}, ErrEvidenceMissing
	}
	return s.run(ctx, "integration.complete", req.Command, req, true, candidateAtHead(req.CandidateSHA), func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		var task, candidate, sha, owner string
		err := tx.QueryRow(`SELECT task_ref, candidate_id, candidate_sha, owner_principal_id FROM lifecycle_service_integration_locks WHERE target_branch = ?`, req.TargetBranch).Scan(&task, &candidate, &sha, &owner)
		if err == sql.ErrNoRows {
			return CommandResult{}, ErrIntegrationLocked
		}
		if err != nil {
			return CommandResult{}, err
		}
		if task != req.Command.TaskRef || candidate != req.CandidateID || sha != req.CandidateSHA || owner != req.Command.Identity.Principal.ID {
			return CommandResult{}, ErrIntegrationLocked
		}
		res, err := s.transition(tx, req.Command, f, StateReconciled, req.EvidenceDigest, req.CandidateSHA, nil, "integration-completed")
		if err != nil {
			return CommandResult{}, err
		}
		if _, err := tx.Exec(`DELETE FROM lifecycle_service_integration_locks WHERE target_branch = ? AND task_ref = ? AND candidate_id = ?`, req.TargetBranch, task, candidate); err != nil {
			return CommandResult{}, err
		}
		out := commandResult("integration.complete", req.Command.TaskRef, res)
		out.CandidateID, out.CandidateSHA = req.CandidateID, req.CandidateSHA
		return out, nil
	})
}

type RecoverRequest struct {
	Command        CommandContext `json:"command"`
	EvidenceDigest string         `json:"evidence_digest"`
}

func (s *Service) Recover(ctx context.Context, req RecoverRequest) (CommandResult, error) {
	if !validDigest(req.EvidenceDigest) {
		return CommandResult{}, ErrEvidenceMissing
	}
	return s.run(ctx, "recovery.enter", req.Command, req, true, nil, func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		_, sha, _ := s.currentCandidateTx(tx, req.Command.TaskRef)
		res, err := s.transition(tx, req.Command, f, StateRecovering, req.EvidenceDigest, sha, nil, "recovery-entered")
		return commandResult("recovery.enter", req.Command.TaskRef, res), err
	})
}

type ResumeRecoveryRequest struct {
	Command        CommandContext `json:"command"`
	Target         State          `json:"target"`
	CandidateSHA   string         `json:"candidate_sha,omitempty"`
	EvidenceDigest string         `json:"evidence_digest"`
}

func (s *Service) ResumeRecovery(ctx context.Context, req ResumeRecoveryRequest) (CommandResult, error) {
	if !validDigest(req.EvidenceDigest) {
		return CommandResult{}, ErrEvidenceMissing
	}
	return s.run(ctx, "recovery.resume", req.Command, req, true, nil, func(tx *sql.Tx, f WorktreeFacts) (CommandResult, error) {
		if err := s.requireStateTx(tx, req.Command.TaskRef, StateRecovering); err != nil {
			return CommandResult{}, err
		}
		_, sha, _ := s.currentCandidateTx(tx, req.Command.TaskRef)
		if req.CandidateSHA != "" && req.CandidateSHA != sha {
			return CommandResult{}, ErrCandidateMismatch
		}
		res, err := s.transition(tx, req.Command, f, req.Target, req.EvidenceDigest, sha, nil, "recovery-resumed")
		return commandResult("recovery.resume", req.Command.TaskRef, res), err
	})
}

func (s *Service) run(ctx context.Context, kind string, command CommandContext, request any, requireOwned bool,
	preflight func(WorktreeFacts) error, mutate func(*sql.Tx, WorktreeFacts) (CommandResult, error)) (CommandResult, error) {
	if err := validateCommandContext(command); err != nil {
		return CommandResult{}, err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return CommandResult{}, err
	}
	digest := evidenceDigest(requestJSON)
	if replay, ok, err := s.lookupCommand(command.IdempotencyKey, kind, digest); err != nil || ok {
		return replay, err
	}
	facts, err := s.inspector.Inspect(ctx, command.CheckoutPath)
	if err != nil {
		return CommandResult{}, err
	}
	if preflight != nil {
		if err := preflight(facts); err != nil {
			return CommandResult{}, err
		}
	}

	s.machine.mu.Lock()
	defer s.machine.mu.Unlock()
	tx, err := s.machine.db.Begin()
	if err != nil {
		return CommandResult{}, fmt.Errorf("%s: begin: %w", kind, err)
	}
	defer tx.Rollback()
	if replay, ok, err := lookupCommandTx(tx, command.IdempotencyKey, kind, digest); err != nil || ok {
		return replay, err
	}
	if err := recordIdentityTx(tx, command.Identity); err != nil {
		return CommandResult{}, err
	}
	if requireOwned {
		if err := requireOwnedTx(tx, command, facts); err != nil {
			return CommandResult{}, err
		}
	}
	result, err := mutate(tx, facts)
	if err != nil {
		return CommandResult{}, err
	}
	result.Kind, result.TaskRef = kind, command.TaskRef
	resultJSON, _ := json.Marshal(result)
	if _, err := tx.Exec(`INSERT INTO lifecycle_service_commands (idempotency_key, kind, request_digest, result_json, created_at) VALUES (?, ?, ?, ?, ?)`,
		command.IdempotencyKey, kind, digest, string(resultJSON), s.now().UTC()); err != nil {
		return CommandResult{}, fmt.Errorf("%s: persist command: %w", kind, err)
	}
	if err := tx.Commit(); err != nil {
		return CommandResult{}, fmt.Errorf("%s: commit: %w", kind, err)
	}
	return result, nil
}

func (s *Service) transition(tx *sql.Tx, command CommandContext, facts WorktreeFacts, to State, digest, sha string, items []outbox.Item, note string) (TransitionResult, error) {
	return s.transitionWithKey(tx, command, facts, to, digest, sha, items, command.IdempotencyKey+":state", note)
}

func (s *Service) transitionWithKey(tx *sql.Tx, command CommandContext, facts WorktreeFacts, to State, digest, sha string, items []outbox.Item, key, note string) (TransitionResult, error) {
	payload, _ := json.Marshal(map[string]string{"command": note, "principal_id": command.Identity.Principal.ID})
	return s.machine.transitionTx(tx, TransitionRequest{
		TaskRef: command.TaskRef, Repo: command.Repo, To: to, Actor: command.Identity.Actor.ID,
		IdempotencyKey: key, LeaseGeneration: command.LeaseGeneration, Branch: facts.Branch,
		CandidateSHA: sha, EvidenceDigest: digest, Payload: string(payload), OutboxItems: items,
	})
}

func commandResult(kind, task string, result TransitionResult) CommandResult {
	if result.Event.ID == 0 {
		return CommandResult{Kind: kind, TaskRef: task}
	}
	return CommandResult{Kind: kind, TaskRef: task, State: result.Event.ToState, CandidateSHA: result.Event.CandidateSHA, Events: []Event{result.Event}}
}

func validateCommandContext(c CommandContext) error {
	if c.IdempotencyKey == "" || c.TaskRef == "" || c.Repo == "" || c.CheckoutPath == "" || c.LeaseGeneration <= 0 {
		return errors.New("lifecycle service: idempotency key, task, repo, checkout, and positive lease generation are required")
	}
	return c.Identity.validate()
}

func (i Identity) validate() error {
	if i.Actor.Version != ledger.Version1 || i.Principal.Version != ledger.Version1 ||
		i.Actor.ID == "" || i.Principal.ID == "" || i.Principal.ActorID != i.Actor.ID ||
		i.Actor.Kind == "" || i.Principal.Kind == "" {
		return errors.New("lifecycle service: invalid Phase-1 actor/principal identity")
	}
	return nil
}

func (i Identity) human() bool {
	return i.Actor.Kind == "operator" && i.Principal.Kind == "local_operator"
}

func validSHA(value string) bool    { return exactSHA.MatchString(value) }
func validDigest(value string) bool { return digestRE.MatchString(value) }

func evidenceDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Service) lookupCommand(key, kind, digest string) (CommandResult, bool, error) {
	row := s.machine.db.QueryRow(`SELECT kind, request_digest, result_json FROM lifecycle_service_commands WHERE idempotency_key = ?`, key)
	return scanCommand(row, kind, digest)
}

func lookupCommandTx(tx *sql.Tx, key, kind, digest string) (CommandResult, bool, error) {
	row := tx.QueryRow(`SELECT kind, request_digest, result_json FROM lifecycle_service_commands WHERE idempotency_key = ?`, key)
	return scanCommand(row, kind, digest)
}

func scanCommand(row *sql.Row, kind, digest string) (CommandResult, bool, error) {
	var storedKind, storedDigest, resultJSON string
	if err := row.Scan(&storedKind, &storedDigest, &resultJSON); err == sql.ErrNoRows {
		return CommandResult{}, false, nil
	} else if err != nil {
		return CommandResult{}, false, err
	}
	if storedKind != kind || storedDigest != digest {
		return CommandResult{}, false, ErrCommandConflict
	}
	var result CommandResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return CommandResult{}, false, err
	}
	result.Replayed = true
	return result, true, nil
}

func recordIdentityTx(tx *sql.Tx, identity Identity) error {
	actorJSON, _ := json.Marshal(identity.Actor)
	principalJSON, _ := json.Marshal(identity.Principal)
	if err := insertImmutableTx(tx, "lifecycle_service_actors", identity.Actor.ID, string(actorJSON)); err != nil {
		return err
	}
	var existingJSON string
	err := tx.QueryRow(`SELECT record_json FROM lifecycle_service_principals WHERE id = ?`, identity.Principal.ID).Scan(&existingJSON)
	if err == nil {
		if existingJSON != string(principalJSON) {
			return errors.New("lifecycle service: principal identity mutation rejected")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = tx.Exec(`INSERT INTO lifecycle_service_principals (id, actor_id, kind, record_json) VALUES (?, ?, ?, ?)`,
		identity.Principal.ID, identity.Principal.ActorID, identity.Principal.Kind, string(principalJSON))
	return err
}

func insertImmutableTx(tx *sql.Tx, table, id, value string) error {
	var existing string
	err := tx.QueryRow(`SELECT record_json FROM `+table+` WHERE id = ?`, id).Scan(&existing)
	if err == nil {
		if existing != value {
			return errors.New("lifecycle service: actor identity mutation rejected")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = tx.Exec(`INSERT INTO `+table+` (id, record_json) VALUES (?, ?)`, id, value)
	return err
}

func requireOwnedTx(tx *sql.Tx, command CommandContext, facts WorktreeFacts) error {
	var repo, portable, branch string
	err := tx.QueryRow(`SELECT repo, portable_path, branch FROM lifecycle_service_owned_worktrees WHERE task_ref = ? AND released_at IS NULL`, command.TaskRef).Scan(&repo, &portable, &branch)
	if err == sql.ErrNoRows {
		return ErrWorktreeNotOwned
	}
	if err != nil {
		return err
	}
	if repo != command.Repo || portable != facts.PortablePath || branch != facts.Branch {
		return fmt.Errorf("%w: expected %s on %s, got %s on %s", ErrWorktreeNotOwned, portable, branch, facts.PortablePath, facts.Branch)
	}
	return nil
}

func (s *Service) requireStateTx(tx *sql.Tx, task string, want State) error {
	var state string
	if err := tx.QueryRow(`SELECT state FROM lifecycle_task_state WHERE task_ref = ?`, task).Scan(&state); err != nil {
		return err
	}
	if State(state) != want {
		return fmt.Errorf("%w: %s is %s, want %s", ErrInvalidTransition, task, state, want)
	}
	return nil
}

func (s *Service) currentCandidateTx(tx *sql.Tx, task string) (id, sha string, err error) {
	err = tx.QueryRow(`SELECT c.id, c.git_sha FROM lifecycle_task_state t
		JOIN lifecycle_service_candidates c ON c.task_ref = t.task_ref AND c.git_sha = t.candidate_sha
		WHERE t.task_ref = ?`, task).Scan(&id, &sha)
	return
}

func (s *Service) requireCurrentCandidateTx(tx *sql.Tx, task, candidateID, sha string) error {
	if !validSHA(sha) {
		return ErrCandidateMismatch
	}
	id, currentSHA, err := s.currentCandidateTx(tx, task)
	if err == sql.ErrNoRows {
		return ErrCandidateMismatch
	}
	if err != nil {
		return err
	}
	if id != candidateID || currentSHA != sha {
		return ErrCandidateMismatch
	}
	return nil
}

func candidateAtHead(sha string) func(WorktreeFacts) error {
	return func(f WorktreeFacts) error {
		if !validSHA(sha) || f.HeadSHA != sha {
			return fmt.Errorf("%w: checkout HEAD %s, candidate %s", ErrCandidateMismatch, f.HeadSHA, sha)
		}
		return nil
	}
}

func validateReceipt(r ledger.Receipt, candidateID, kind string) error {
	if r.Version != ledger.Version1 || r.ID == "" || r.CandidateID != candidateID || r.Kind != kind || !validDigest(r.EvidenceDigest) || len(r.Payload) == 0 {
		return fmt.Errorf("record %s receipt: invalid exact-candidate Phase-1 receipt: %w", kind, ErrEvidenceMissing)
	}
	return nil
}

func insertReceiptTx(tx *sql.Tx, r ledger.Receipt, sha, outcome string, now time.Time) error {
	r.CreatedAt = now
	blob, _ := json.Marshal(r)
	_, err := tx.Exec(`INSERT INTO lifecycle_service_receipts
		(id, candidate_id, candidate_sha, kind, outcome, evidence_digest, record_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, r.CandidateID, sha, r.Kind, outcome, r.EvidenceDigest, string(blob), now)
	if err != nil {
		return fmt.Errorf("record receipt: %w", err)
	}
	return nil
}

func requirePassingEvidenceTx(tx *sql.Tx, candidateID, sha string) error {
	var verification string
	if err := tx.QueryRow(`SELECT outcome FROM lifecycle_service_receipts WHERE candidate_id = ? AND candidate_sha = ? AND kind = 'verification' ORDER BY created_at DESC, id DESC LIMIT 1`, candidateID, sha).Scan(&verification); err != nil || verification != "pass" {
		return fmt.Errorf("%w: passing verification missing", ErrEvidenceMissing)
	}
	var review string
	if err := tx.QueryRow(`SELECT outcome FROM lifecycle_service_reviews WHERE candidate_id = ? AND candidate_sha = ? ORDER BY created_at DESC, id DESC LIMIT 1`, candidateID, sha).Scan(&review); err != nil || review != "pass" {
		return fmt.Errorf("%w: passing review missing", ErrEvidenceMissing)
	}
	return nil
}

func (s *Service) admitMergeTx(tx *sql.Tx, req BeginIntegrationRequest) error {
	id, sha, err := s.currentCandidateTx(tx, req.Command.TaskRef)
	if err != nil {
		return ErrApprovalStale
	}
	var approvalCandidate, approvalSHA, decision string
	err = tx.QueryRow(`SELECT candidate_id, candidate_sha, decision FROM lifecycle_service_approvals WHERE id = ?`, req.ApprovalID).Scan(&approvalCandidate, &approvalSHA, &decision)
	if err == sql.ErrNoRows {
		return ErrApprovalMissing
	}
	if err != nil {
		return err
	}
	if approvalCandidate != id || approvalCandidate != req.CandidateID {
		return ErrApprovalStale
	}
	var latestID string
	if err := tx.QueryRow(`SELECT id FROM lifecycle_service_approvals WHERE candidate_id = ? AND candidate_sha = ? ORDER BY seq DESC LIMIT 1`, id, sha).Scan(&latestID); err != nil {
		return ErrApprovalMissing
	}
	if latestID != req.ApprovalID {
		return ErrApprovalStale
	}
	if decision != "approved" {
		return ErrApprovalRejected
	}
	if approvalSHA != sha || approvalSHA != req.CandidateSHA {
		return ErrApprovalSHAMismatch
	}
	return nil
}
