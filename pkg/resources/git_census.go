package resources

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type ProcessUsage struct {
	CWD      bool
	OpenFile bool
	PIDs     []int
}

type ProcessInspector interface {
	InUse(context.Context, string) (ProcessUsage, error)
}

// LifecycleEvidence is read from canonical claim/review records. It is not
// inferred from a worktree filename or an unsigned TASK-CONTEXT file.
type LifecycleEvidence struct {
	ActiveLease           bool
	FailedCandidate       bool
	ReviewHandoffAdmitted bool
}

type LifecycleEvidenceReader interface {
	Read(context.Context, string, string, RegisteredWorktree) (LifecycleEvidence, error)
}

// SQLiteLifecycleEvidence reads the existing claim database read-only and the
// append-only review ledger. Missing or malformed authority is an error: an
// absent record does not prove that a lane is disposable.
type SQLiteLifecycleEvidence struct {
	ClaimsPath string
	LedgerPath string
	RepoID     string
	HostID     string
}

func (r SQLiteLifecycleEvidence) Read(ctx context.Context, repoRoot, hostID string, lane RegisteredWorktree) (LifecycleEvidence, error) {
	if strings.TrimSpace(r.ClaimsPath) == "" || strings.TrimSpace(r.LedgerPath) == "" || strings.TrimSpace(r.RepoID) == "" || strings.TrimSpace(hostID) == "" {
		return LifecycleEvidence{}, errors.New("canonical lifecycle evidence identity is incomplete")
	}
	if r.HostID != "" && r.HostID != hostID {
		return LifecycleEvidence{}, errors.New("canonical lifecycle evidence host mismatch")
	}
	dsn := "file:" + r.ClaimsPath + "?mode=ro&_pragma=busy_timeout(1000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return LifecycleEvidence{}, fmt.Errorf("open canonical claim evidence: %w", err)
	}
	defer db.Close()
	var repo, owner, status string
	var held, generation int64
	var expires time.Time
	err = db.QueryRowContext(ctx, `SELECT repo, owner_id, status, held, generation, expires_at FROM leases WHERE worktree_path = ? ORDER BY generation DESC LIMIT 1`, lane.Path).Scan(&repo, &owner, &status, &held, &generation, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return LifecycleEvidence{}, errors.New("canonical claim record unavailable for registered worktree")
	}
	if err != nil {
		return LifecycleEvidence{}, fmt.Errorf("read canonical claim evidence: %w", err)
	}
	if repo != r.RepoID || generation < 1 || strings.TrimSpace(owner) == "" || !strings.Contains(owner, hostID) {
		return LifecycleEvidence{}, errors.New("canonical claim record identity mismatch")
	}
	evidence := LifecycleEvidence{ActiveLease: status == "active" && (held != 0 || expires.After(time.Now()))}
	if err := readReviewLifecycle(r.LedgerPath, lane.Head, &evidence); err != nil {
		return LifecycleEvidence{}, err
	}
	_ = repoRoot
	return evidence, nil
}

func readReviewLifecycle(path, head string, evidence *LifecycleEvidence) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read canonical review ledger: %w", err)
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row struct{ Event, SHA, Verdict string }
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("canonical review ledger evidence: %w", err)
		}
		if row.SHA != head {
			continue
		}
		if row.Event == "verdict" && (row.Verdict == "FAIL" || row.Verdict == "BLOCKED") {
			evidence.FailedCandidate = true
		}
		if row.Event == "enqueue" {
			evidence.ReviewHandoffAdmitted = true
		}
	}
	return nil
}

type GitWorktreeEnumerator struct {
	Processes ProcessInspector
	Now       func() time.Time
	Evidence  LifecycleEvidenceReader
	HostID    string
}

type GitTrackedSourceInspector struct{}

func (GitTrackedSourceInspector) HasTrackedSource(ctx context.Context, worktree, relative string) (bool, error) {
	out, err := gitOutput(ctx, worktree, "--literal-pathspecs", "ls-files", "-z", "--", filepath.ToSlash(relative))
	if err != nil {
		return false, err
	}
	return len(out) != 0, nil
}

func (e GitWorktreeEnumerator) List(ctx context.Context, repoRoot, baseRef string) ([]RegisteredWorktree, error) {
	if strings.TrimSpace(repoRoot) == "" || strings.TrimSpace(baseRef) == "" {
		return nil, errors.New("worktree census requires repository root and base ref")
	}
	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	lanes, err := listRegisteredWorktrees(ctx, root, gitOutput)
	if err != nil {
		return nil, err
	}
	if len(lanes) == 0 {
		return nil, errors.New("registered worktree allowlist is empty")
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	processes := e.Processes
	if processes == nil {
		processes = LSOFProcessInspector{Timeout: 2 * time.Second, MaxOutputBytes: 1 << 20}
	}
	for i := range lanes {
		resolved, resolveErr := filepath.EvalSymlinks(lanes[i].Path)
		if resolveErr != nil {
			lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "worktree_realpath_unavailable"
			continue
		}
		lanes[i].Path = filepath.Clean(resolved)
		lanes[i].Category = classifyWorktree(root, lanes[i])
		if info, statErr := os.Stat(lanes[i].Path); statErr == nil {
			lanes[i].LastUse = info.ModTime().UTC()
		} else {
			lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "worktree_stat_unavailable"
			continue
		}
		dirty, untracked, statusErr := gitStatus(ctx, lanes[i].Path)
		if statusErr != nil {
			lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "git_status_unavailable"
			continue
		}
		lanes[i].Dirty, lanes[i].Untracked = dirty, untracked
		merged, mergeErr := gitMerged(ctx, lanes[i].Path, baseRef)
		if mergeErr != nil {
			lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "merge_state_unavailable"
			continue
		}
		lanes[i].Unmerged = !merged
		lease, leaseErr := activeTaskReceipt(lanes[i].Path, now())
		if leaseErr != nil {
			lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "lease_evidence_unavailable"
			continue
		}
		lanes[i].ActiveLease = lease
		usage, processErr := processes.InUse(ctx, lanes[i].Path)
		if processErr != nil {
			lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "process_evidence_unavailable"
			continue
		}
		lanes[i].ActiveCWD, lanes[i].OpenFile = usage.CWD, usage.OpenFile
		if e.Evidence != nil {
			evidence, evidenceErr := e.Evidence.Read(ctx, root, e.HostID, lanes[i])
			if evidenceErr != nil {
				lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "canonical_lifecycle_evidence_unavailable"
				continue
			}
			lanes[i].ActiveLease = evidence.ActiveLease
			lanes[i].FailedCandidate = evidence.FailedCandidate
			lanes[i].ReviewHandoffAdmitted = evidence.ReviewHandoffAdmitted
		}
		lanes[i].Held = lanes[i].Held || lanes[i].State == LaneHeld
		switch {
		case lanes[i].Category == LaneCurrent:
			lanes[i].State, lanes[i].PreserveReason = LaneActive, "canonical_checkout"
		case lanes[i].ActiveCWD || lanes[i].OpenFile || lanes[i].ActiveLease:
			lanes[i].State = LaneActive
		case lanes[i].Held:
			lanes[i].State = LaneHeld
		case merged:
			lanes[i].State = LaneDone
		default:
			lanes[i].State = LaneIdle
		}
	}
	return lanes, nil
}

type gitOutputRunner func(context.Context, string, ...string) ([]byte, error)

func listRegisteredWorktrees(ctx context.Context, root string, run gitOutputRunner) ([]RegisteredWorktree, error) {
	out, err := run(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("list registered worktrees: %w", err)
	}
	return parseWorktreePorcelain(out)
}

func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

func parseWorktreePorcelain(data []byte) ([]RegisteredWorktree, error) {
	var lanes []RegisteredWorktree
	var lane RegisteredWorktree
	flush := func() error {
		if lane.Path == "" {
			return nil
		}
		if lane.Head == "" {
			return fmt.Errorf("registered worktree %q has no HEAD", lane.Path)
		}
		lanes = append(lanes, lane)
		lane = RegisteredWorktree{}
		return nil
	}
	for _, raw := range bytes.Split(data, []byte{'\n'}) {
		line := strings.TrimSuffix(string(raw), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		decoded, err := decodePorcelainValue(value)
		if err != nil {
			return nil, fmt.Errorf("registered worktree %s value: %w", key, err)
		}
		switch key {
		case "worktree":
			if decoded == "" {
				return nil, errors.New("registered worktree path is empty")
			}
			if lane.Path != "" {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			lane.Path = decoded
		case "HEAD":
			lane.Head = decoded
		case "branch":
			lane.Branch = strings.TrimPrefix(decoded, "refs/heads/")
		case "locked", "prunable":
			lane.Held = true
			lane.State = LaneHeld
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return lanes, nil
}

func decodePorcelainValue(value string) (string, error) {
	if !strings.HasPrefix(value, `"`) {
		return value, nil
	}
	decoded, err := strconv.Unquote(value)
	if err != nil {
		return "", fmt.Errorf("decode Git quoted value %q: %w", value, err)
	}
	return decoded, nil
}

func classifyWorktree(root string, lane RegisteredWorktree) LaneCategory {
	path := filepath.ToSlash(lane.Path)
	base := strings.ToLower(filepath.Base(lane.Path))
	branch := strings.ToLower(lane.Branch)
	if filepath.Clean(lane.Path) == filepath.Clean(root) {
		return LaneCurrent
	}
	if strings.Contains(path, ReviewPoolPathFragment) {
		return LaneReviewPool
	}
	if strings.HasPrefix(base, "rv-") || strings.Contains(path, "/review-") || (lane.Branch == "" && strings.Contains(path, ManagedWorktreePathFragment)) {
		return LaneReviewSurface
	}
	if strings.Contains(base, "harvest") {
		return LaneHarvest
	}
	if strings.Contains(path, LegacyWorktreePathFragment) {
		return LaneLegacyStanding
	}
	if strings.Contains(path, ManagedWorktreePathFragment) && lane.Branch != "" {
		if strings.HasPrefix(branch, "herd/") || strings.HasPrefix(branch, "fac-") {
			return LaneTask
		}
		return LaneNew
	}
	return LaneLegacyStanding
}

func gitStatus(ctx context.Context, path string) (dirty, untracked bool, err error) {
	out, err := gitOutput(ctx, path, "status", "--porcelain=v1", "--untracked-files=all", "-z")
	if err != nil {
		return false, false, err
	}
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) < 2 {
			continue
		}
		if string(raw[:2]) == "??" {
			untracked = true
		} else {
			dirty = true
		}
	}
	return dirty, untracked, nil
}

func gitMerged(ctx context.Context, path, baseRef string) (bool, error) {
	return GitCommitIsAncestor(ctx, path, "HEAD", baseRef)
}

func activeTaskReceipt(worktree string, now time.Time) (bool, error) {
	path := filepath.Join(worktree, TaskContextFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var receipt struct {
		LeaseID         string    `json:"lease_id"`
		LeaseGeneration int64     `json:"lease_generation"`
		SessionID       string    `json:"session_id"`
		ExpiresAt       time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		return false, err
	}
	if strings.TrimSpace(receipt.LeaseID) == "" || receipt.LeaseGeneration <= 0 || strings.TrimSpace(receipt.SessionID) == "" || receipt.ExpiresAt.IsZero() {
		return false, errors.New("task receipt is incomplete")
	}
	return receipt.ExpiresAt.After(now), nil
}

type LSOFProcessInspector struct {
	Executable     string
	Timeout        time.Duration
	MaxOutputBytes int
}

func (p LSOFProcessInspector) InUse(ctx context.Context, path string) (ProcessUsage, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ProcessUsage{}, err
	}
	var usage ProcessUsage
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		if cwdResolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil && containedPath(resolved, cwdResolved) {
			usage.CWD = true
		}
	} else {
		return ProcessUsage{}, cwdErr
	}
	executable := strings.TrimSpace(p.Executable)
	if executable == "" {
		executable, err = exec.LookPath("lsof")
		if err != nil {
			return ProcessUsage{}, err
		}
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	maxOutput := p.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = 1 << 20
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, executable, "-nP", "-Ffnp", "+D", resolved)
	var output limitedOutput
	output.remaining = maxOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	if output.overflow {
		return ProcessUsage{}, errors.New("lsof output exceeded bound")
	}
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(output.Bytes()) == 0) {
		return ProcessUsage{}, err
	}
	seen := make(map[int]struct{})
	currentPID := 0
	for _, line := range strings.Split(string(output.Bytes()), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, parseErr := strconv.Atoi(strings.TrimSpace(line[1:]))
			if parseErr == nil {
				currentPID = pid
				seen[pid] = struct{}{}
			}
		case 'f':
			descriptor := strings.TrimSpace(line[1:])
			if descriptor == "cwd" {
				usage.CWD = true
			} else if currentPID != 0 && descriptor != "rtd" && descriptor != "txt" && descriptor != "mem" {
				usage.OpenFile = true
			}
		}
	}
	for pid := range seen {
		usage.PIDs = append(usage.PIDs, pid)
	}
	sortInts(usage.PIDs)
	return usage, nil
}

type limitedOutput struct {
	buf       bytes.Buffer
	remaining int
	overflow  bool
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		if w.remaining > 0 {
			_, _ = w.buf.Write(p[:w.remaining])
		}
		w.remaining, w.overflow = 0, true
		return len(p), errors.New("output limit exceeded")
	}
	w.remaining -= len(p)
	return w.buf.Write(p)
}

func (w *limitedOutput) Bytes() []byte { return w.buf.Bytes() }

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
