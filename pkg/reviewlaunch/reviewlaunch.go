// Package reviewlaunch creates a reviewer agent transactionally (FAC-187).
//
// Order of side effects is load-bearing:
//  1. Validate herdr agent-name grammar.
//  2. Create the exact detached tab with cwd = the task worktree (never a
//     shared reviewer tree or a sibling task tree).
//  3. Start the agent and verify identity (name, pane, session when known,
//     process readiness).
//  4. Deliver the review packet and require consumption readback.
//  5. Only then transition Kaneo/board status to in-review.
//
// Any failure closes only the newly created tab/worktree artifacts and
// leaves the prior board status unchanged.
package reviewlaunch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// Board is the narrow provider mutation surface used after verified
// consumption. Implementations wrap provider.TaskProvider.UpdateStatus.
type Board interface {
	UpdateStatus(ctx context.Context, taskID, status string) error
}

// AgentSnapshot is the live identity read back after start / before prompt.
type AgentSnapshot struct {
	Name      string
	PaneID    string
	TabID     string
	Workspace string
	Cwd       string
	Session   string
	Status    string
	Kind      string
}

// Herdr is the injectable fleet surface. Production uses LiveHerdr.
type Herdr interface {
	RequireWorkspace(repoRoot string) (string, error)
	TabCreateForTask(workspaceID, label, cwd string, noFocus bool) (*herdr.TabInfo, error)
	AgentStart(req launch.Request, name, kind, paneID string) error
	DeliverAndProve(target, text string, timeout time.Duration) (*herdr.PromptReceipt, error)
	TabClose(tabID string) error
	ReadAgent(name string) (AgentSnapshot, error)
}

// LiveHerdr is the production adapter over pkg/herdr.
type LiveHerdr struct{}

func (LiveHerdr) RequireWorkspace(repoRoot string) (string, error) {
	return herdr.RequireWorkspace(repoRoot)
}
func (LiveHerdr) TabCreateForTask(workspaceID, label, cwd string, noFocus bool) (*herdr.TabInfo, error) {
	return herdr.TabCreateForTask(workspaceID, label, cwd, noFocus)
}
func (LiveHerdr) AgentStart(req launch.Request, name, kind, paneID string) error {
	return herdr.StartPreparedAgent(req.TabID, name, kind, paneID, req)
}
func (LiveHerdr) DeliverAndProve(target, text string, timeout time.Duration) (*herdr.PromptReceipt, error) {
	return herdr.DeliverAndProve(target, text, timeout)
}
func (LiveHerdr) TabClose(tabID string) error { return herdr.TabClose(tabID) }
func (LiveHerdr) ReadAgent(name string) (AgentSnapshot, error) {
	agents, err := herdr.AgentList()
	if err != nil {
		return AgentSnapshot{}, err
	}
	for _, a := range agents {
		if a.Name == name {
			return AgentSnapshot{
				Name: a.Name, PaneID: a.PaneID, TabID: a.TabID, Workspace: a.Workspace,
				Cwd: a.Cwd, Session: a.Session.Value, Status: a.Status, Kind: a.Kind,
			}, nil
		}
	}
	return AgentSnapshot{}, fmt.Errorf("%w: %s", ErrMissingSession, name)
}

// agentNameRE is the herdr 0.7.x grammar:
// must start with a lowercase letter and contain only lowercase letters,
// digits, '-' or '_' (1-32 characters).
var agentNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

var (
	ErrInvalidAgentName = errors.New("reviewlaunch: invalid_agent_name")
	ErrWorktreeRequired = errors.New("reviewlaunch: exact task worktree is required")
	ErrSharedRoot       = errors.New("reviewlaunch: shared repository root denied")
	ErrIdentityDrift    = errors.New("reviewlaunch: reviewer identity drift")
	ErrMissingSession   = errors.New("reviewlaunch: agent session missing")
	ErrPacketRejected   = errors.New("reviewlaunch: packet consumption not proven")
	ErrHarnessExit      = errors.New("reviewlaunch: harness exited before ready")
	// ErrBoardUnchanged is a sentinel documenting that board mutation did not run.
	ErrBoardUnchanged = errors.New("reviewlaunch: board status left unchanged")
)

// AgentName builds a herdr-valid agent name for a reviewer of taskRef.
// Format: review-<role>-<refslug>, all lowercase, truncated to 32 chars
// without breaking the grammar. Uppercase task refs (FAC-151) are lowercased
// so the invalid_agent_name failure from the 2026-08-03 incident cannot recur.
func AgentName(role, taskRef string) (string, error) {
	role = sanitizeSegment(role)
	ref := sanitizeSegment(taskRef)
	if role == "" {
		role = "reviewer"
	}
	if ref == "" {
		return "", fmt.Errorf("%w: empty task ref", ErrInvalidAgentName)
	}
	name := "review-" + role + "-" + ref
	if len(name) > 32 {
		// Prefer keeping the ref tail (task identity) over a long role.
		// "review-" (7) + role + "-" + ref must fit.
		budget := 32 - 7 // after "review-"
		// role gets at most 8 chars, ref the rest, with a hyphen between.
		roleMax := 8
		if len(role) < roleMax {
			roleMax = len(role)
		}
		rolePart := role[:roleMax]
		refBudget := budget - len(rolePart) - 1
		if refBudget < 1 {
			return "", fmt.Errorf("%w: cannot fit task ref into 32-char agent name", ErrInvalidAgentName)
		}
		if len(ref) > refBudget {
			ref = ref[len(ref)-refBudget:]
			// Ensure still starts with letter/digit after truncation.
			ref = strings.TrimLeftFunc(ref, func(r rune) bool {
				return r == '-' || r == '_'
			})
			if ref == "" || !unicode.IsLetter(rune(ref[0])) && !unicode.IsDigit(rune(ref[0])) {
				return "", fmt.Errorf("%w: truncated ref is empty", ErrInvalidAgentName)
			}
		}
		name = "review-" + rolePart + "-" + ref
	}
	if !agentNameRE.MatchString(name) {
		return "", fmt.Errorf("%w: %q does not match herdr grammar", ErrInvalidAgentName, name)
	}
	return name, nil
}

func sanitizeSegment(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == '/' || r == '.' || r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return out
}

// ValidateAgentName rejects names that herdr would refuse with
// invalid_agent_name. Callers use this before any tab create side effect.
func ValidateAgentName(name string) error {
	if !agentNameRE.MatchString(name) {
		return fmt.Errorf("%w: agent name must start with a lowercase letter and contain only lowercase letters, digits, '-' or '_' (1-32 characters); got %q", ErrInvalidAgentName, name)
	}
	return nil
}

// Request is one transactional reviewer launch.
type Request struct {
	TaskRef      string
	TaskID       string
	Role         string // "reviewer" or "assayer"
	Lane         string
	Repository   string
	RepoRoot     string
	WorktreePath string // exact task worktree; required
	Packet       string
	// LaunchRequest is the complete process identity for AgentStart.
	// When nil, a minimal request is built from TaskRef/Role/Lane fields
	// (test path only; production must supply a validated decision).
	LaunchRequest *launch.Request
	Timeout       time.Duration
	// InReviewStatus is the board status written only after consumption.
	// Default: "in-review".
	InReviewStatus string
}

// Result is the durable outcome of a successful launch.
type Result struct {
	AgentName string
	TabID     string
	PaneID    string
	Workspace string
	Cwd       string
	Session   string
	Receipt   *herdr.PromptReceipt
	BoardTo   string
}

// Launcher runs the transactional path with injectable seams.
type Launcher struct {
	Herdr Herdr
	Board Board
	// ReceiptPath optional JSONL for compensated failures / successes.
	ReceiptPath string
	Now         func() time.Time
}

// FailureReceipt is appended on every compensated failure.
type FailureReceipt struct {
	At      time.Time `json:"at"`
	TaskRef string    `json:"task_ref"`
	TaskID  string    `json:"task_id"`
	Stage   string    `json:"stage"`
	Error   string    `json:"error"`
	TabID   string    `json:"tab_id,omitempty"`
	Closed  bool      `json:"closed"`
	// BoardMutated is always false on the failure path by design.
	BoardMutated bool `json:"board_mutated"`
}

// Launch executes the transactional reviewer create. Board.UpdateStatus is
// called only after a verified consumption receipt.
func (l *Launcher) Launch(ctx context.Context, req Request) (Result, error) {
	if l == nil || l.Herdr == nil {
		return Result{}, errors.New("reviewlaunch: herdr surface is required")
	}
	if l.Board == nil {
		return Result{}, errors.New("reviewlaunch: board surface is required")
	}
	if strings.TrimSpace(req.TaskRef) == "" || strings.TrimSpace(req.TaskID) == "" {
		return Result{}, errors.New("reviewlaunch: task ref and task id are required")
	}
	if strings.TrimSpace(req.Packet) == "" {
		return Result{}, errors.New("reviewlaunch: review packet is required")
	}
	if strings.TrimSpace(req.WorktreePath) == "" {
		return Result{}, ErrWorktreeRequired
	}
	// Production herdr returns absolute TabInfo.Cwd; callers often pass a
	// repo-relative path (e.g. .herd/worktrees/fac-151). Normalize once so
	// every identity gate compares apples to apples.
	absWT, absErr := filepath.Abs(req.WorktreePath)
	if absErr != nil {
		return Result{}, fmt.Errorf("reviewlaunch: resolve worktree path: %w", absErr)
	}
	req.WorktreePath = absWT
	if req.RepoRoot != "" {
		if err := worktree.RejectSharedRoot(req.RepoRoot, req.WorktreePath); err != nil {
			return Result{}, fmt.Errorf("%w: %v", ErrSharedRoot, err)
		}
	}

	name, err := AgentName(req.Role, req.TaskRef)
	if err != nil {
		return Result{}, err
	}
	if err := ValidateAgentName(name); err != nil {
		return Result{}, err
	}

	status := req.InReviewStatus
	if status == "" {
		status = "in-review"
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	ws, err := l.Herdr.RequireWorkspace(req.RepoRoot)
	if err != nil {
		return Result{}, l.fail(req, "", "workspace", err, false)
	}

	// --- side effects begin; track tab for exact compensation ---
	tab, err := l.Herdr.TabCreateForTask(ws, name, req.WorktreePath, true)
	if err != nil {
		// invalid_agent_name surfaces here when grammar slipped past Validate.
		return Result{}, l.fail(req, "", "tab_create", err, false)
	}
	tabID := tab.ID
	paneID := tab.Pane.ID

	// Cwd must be the exact task worktree, not a sibling (FAC-172 vs FAC-151).
	// Compare Abs-normalized paths: herdr always reports absolute cwd while
	// operators may have handed a relative path before normalization above.
	if tab.Cwd != "" && !sameWorktreePath(tab.Cwd, req.WorktreePath) {
		return Result{}, l.fail(req, tabID, "cwd_drift", fmt.Errorf("%w: tab cwd %q != task worktree %q", ErrIdentityDrift, tab.Cwd, req.WorktreePath), true)
	}

	startReq := launch.Request{Name: name, TaskRef: req.TaskRef, Repository: req.Repository, Lane: req.Lane, CWD: req.WorktreePath}
	kind := ""
	if req.LaunchRequest != nil {
		startReq = *req.LaunchRequest
		startReq.Name = name
		startReq.TaskRef = req.TaskRef
		startReq.CWD = req.WorktreePath
		if startReq.Repository == "" {
			startReq.Repository = req.Repository
		}
		if startReq.Lane == "" {
			startReq.Lane = req.Lane
		}
		if startReq.Decision != nil {
			kind = startReq.Decision.Harness
		}
	}
	if kind == "" {
		kind = "pi"
	}
	// Bind tab/pane before start so launch receipts carry identity.
	startReq.TabID = tabID
	startReq.PaneID = paneID

	if err := l.Herdr.AgentStart(startReq, name, kind, paneID); err != nil {
		return Result{}, l.fail(req, tabID, "agent_start", fmt.Errorf("%w: %v", ErrHarnessExit, err), true)
	}

	snap, err := l.Herdr.ReadAgent(name)
	if err != nil {
		return Result{}, l.fail(req, tabID, "missing_session", err, true)
	}
	if err := validateSnapshot(snap, name, tabID, paneID, ws, req.WorktreePath, ""); err != nil {
		return Result{}, l.fail(req, tabID, "identity_drift", err, true)
	}

	// Herdr prompt addressing is name-based. The surrounding exact identity
	// reads bind that name to this tab, pane, cwd, harness and real model
	// session; a terminal/pane fallback is never accepted as conversation
	// identity (FAC-663).
	receipt, err := l.Herdr.DeliverAndProve(name, req.Packet, timeout)
	if err != nil {
		return Result{}, l.fail(req, tabID, "packet_delivery", fmt.Errorf("%w: %v", ErrPacketRejected, err), true)
	}
	if receipt == nil || !receipt.Consumed || !receipt.Verified {
		return Result{}, l.fail(req, tabID, "packet_receipt", ErrPacketRejected, true)
	}
	if !herdr.ConsumptionProvenSeen(receipt.BaselineStatus, receipt.FinalStatus, receipt.SawWorking) {
		return Result{}, l.fail(req, tabID, "packet_sequence", fmt.Errorf("%w: sequence %s", ErrPacketRejected, receipt.SequenceToken), true)
	}
	post, err := l.Herdr.ReadAgent(name)
	if err != nil {
		return Result{}, l.fail(req, tabID, "post_delivery_session", fmt.Errorf("%w: %v", ErrIdentityDrift, err), true)
	}
	if err := validateSnapshot(post, name, tabID, paneID, ws, req.WorktreePath, snap.Session); err != nil {
		return Result{}, l.fail(req, tabID, "post_delivery_identity", err, true)
	}

	// --- only now mutate the board ---
	if err := l.Board.UpdateStatus(ctx, req.TaskID, status); err != nil {
		// Packet is already consumed; close tab is still correct compensation
		// so we do not leave an orphan reviewer claiming a board card we
		// failed to mark. Board stays at prior status.
		return Result{}, l.fail(req, tabID, "board_transition", err, true)
	}

	return Result{
		AgentName: name,
		TabID:     tabID,
		PaneID:    paneID,
		Workspace: ws,
		Cwd:       req.WorktreePath,
		Session:   post.Session,
		Receipt:   receipt,
		BoardTo:   status,
	}, nil
}

func validateSnapshot(s AgentSnapshot, name, tabID, paneID, workspace, cwd, expectedSession string) error {
	if s.Name != name || s.TabID != tabID || s.PaneID != paneID {
		return fmt.Errorf("%w: name/tab/pane mismatch", ErrIdentityDrift)
	}
	if s.Workspace != "" && s.Workspace != workspace {
		return fmt.Errorf("%w: workspace %q != %q", ErrIdentityDrift, s.Workspace, workspace)
	}
	if s.Cwd != "" && !sameWorktreePath(s.Cwd, cwd) {
		return fmt.Errorf("%w: agent cwd %q != %q", ErrIdentityDrift, s.Cwd, cwd)
	}
	if !herdr.RealModelSessionID(s.Session) {
		return fmt.Errorf("%w: no real model session for %s", ErrMissingSession, name)
	}
	if expectedSession != "" && s.Session != expectedSession {
		return fmt.Errorf("%w: model session changed after packet delivery: want %q got %q", ErrIdentityDrift, expectedSession, s.Session)
	}
	return nil
}

func (l *Launcher) fail(req Request, tabID, stage string, primary error, closeTab bool) error {
	closed := false
	if closeTab && tabID != "" {
		if err := l.Herdr.TabClose(tabID); err != nil {
			primary = errors.Join(primary, fmt.Errorf("compensate tab close %s: %w", tabID, err))
		} else {
			closed = true
		}
	}
	fr := FailureReceipt{
		At:           l.now().UTC(),
		TaskRef:      req.TaskRef,
		TaskID:       req.TaskID,
		Stage:        stage,
		Error:        primary.Error(),
		TabID:        tabID,
		Closed:       closed,
		BoardMutated: false,
	}
	if err := l.appendFailure(fr); err != nil {
		primary = errors.Join(primary, err)
	}
	return errors.Join(primary, ErrBoardUnchanged)
}

func (l *Launcher) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// sameWorktreePath reports whether a and b name the same directory after
// absolute resolution and optional symlink evaluation. filepath.Clean alone
// is not enough: production herdr returns absolute TabInfo.Cwd while callers
// often pass a repo-relative worktree path.
func sameWorktreePath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	absA, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(absA); err == nil {
		absA = resolved
	}
	if resolved, err := filepath.EvalSymlinks(absB); err == nil {
		absB = resolved
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}

var failureMu sync.Mutex

func (l *Launcher) appendFailure(fr FailureReceipt) error {
	if l == nil || strings.TrimSpace(l.ReceiptPath) == "" {
		return nil
	}
	failureMu.Lock()
	defer failureMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.ReceiptPath), 0o755); err != nil {
		return fmt.Errorf("reviewlaunch: receipt dir: %w", err)
	}
	b, err := json.Marshal(fr)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.ReceiptPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
