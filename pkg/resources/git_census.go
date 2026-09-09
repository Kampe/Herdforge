package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
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
	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return LifecycleEvidence{}, fmt.Errorf("resolve canonical evidence root: %w", err)
	}
	worktree, err := filepath.EvalSymlinks(lane.Path)
	if err != nil || !containedPath(root, worktree) {
		return LifecycleEvidence{}, errors.New("canonical claim worktree identity unavailable")
	}
	claims, err := claim.OpenSQLiteLeaseStoreReadOnly(r.ClaimsPath)
	if err != nil {
		return LifecycleEvidence{}, fmt.Errorf("open canonical claim evidence: %w", err)
	}
	defer claims.Close()
	active, err := claims.ActiveClaims(ctx, time.Now())
	if err != nil {
		return LifecycleEvidence{}, fmt.Errorf("read canonical active claims: %w", err)
	}
	paths, err := claims.DistinctWorktreePaths(ctx)
	if err != nil {
		return LifecycleEvidence{}, fmt.Errorf("read canonical claim history: %w", err)
	}
	knownPath := false
	for _, path := range paths {
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr == nil && filepath.Clean(resolved) == filepath.Clean(worktree) {
			knownPath = true
			break
		}
	}
	if !knownPath {
		return LifecycleEvidence{}, errors.New("canonical claim history has no exact worktree identity")
	}
	evidence := LifecycleEvidence{}
	for _, lease := range active {
		if lease == nil {
			continue
		}
		leasePath, resolveErr := filepath.EvalSymlinks(lease.WorktreePath)
		if resolveErr != nil || filepath.Clean(leasePath) != filepath.Clean(worktree) {
			continue
		}
		if lease.Repo != r.RepoID || lease.HoldRepository != r.RepoID || lease.Generation <= 0 || lease.OwnerID == "" || lease.TaskRef == "" || lease.Project == "" || lease.Provider == "" {
			return LifecycleEvidence{}, errors.New("canonical claim record identity mismatch")
		}
		// Host is a scope identity of the local canonical root and claim DB;
		// owner IDs are lane identities and must never be substring-matched.
		evidence.ActiveLease = true
	}
	if err := readReviewLifecycle(r.LedgerPath, lane.Head, &evidence); err != nil {
		return LifecycleEvidence{}, err
	}
	return evidence, nil
}

type lifecycleRow struct {
	Event        string `json:"event"`
	SHA          string `json:"sha"`
	CandidateSHA string `json:"candidate_sha"`
	Reviewer     string `json:"reviewer"`
	Host         string `json:"host"`
	Task         string `json:"task"`
	Lease        string `json:"lease"`
	Pane         string `json:"pane"`
	Lane         string `json:"lane"`
	Status       string `json:"status"`
	Verdict      string `json:"verdict"`
}

func readReviewLifecycle(path, head string, evidence *LifecycleEvidence) error {
	read := func(name string) ([]lifecycleRow, error) {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var out []lifecycleRow
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var row lifecycleRow
			if err := json.Unmarshal(line, &row); err != nil {
				return nil, err
			}
			out = append(out, row)
		}
		return out, nil
	}
	rows, err := read(path)
	if err != nil {
		return fmt.Errorf("read canonical review ledger: %w", err)
	}
	queue, err := read(filepath.Join(filepath.Dir(path), "harvest-queue.jsonl"))
	if err != nil {
		return fmt.Errorf("read canonical review queue: %w", err)
	}
	records := make(map[string]lifecycleRow)
	for _, row := range rows {
		if row.Event == "record" && row.SHA == head && row.Reviewer != "" && row.Host != "" && row.Task != "" && row.Lease != "" && row.Pane != "" {
			records[row.Reviewer+"\x00"+row.Host+"\x00"+row.Task] = row
		}
	}
	for _, row := range rows {
		if row.SHA != head || row.Event != "verdict" || row.Reviewer == "" || row.Host == "" || row.Task == "" || row.CandidateSHA != head {
			continue
		}
		key := row.Reviewer + "\x00" + row.Host + "\x00" + row.Task
		if _, ok := records[key]; !ok {
			continue
		}
		if row.Verdict == "FAIL" || row.Verdict == "BLOCKED" {
			evidence.FailedCandidate = true
		}
		if row.Verdict != "PASS" {
			continue
		}
		for _, q := range queue {
			if q.Event == "enqueue" && q.SHA == head && q.Reviewer == row.Reviewer && q.Host == row.Host && q.Task == row.Task && q.Lane != "" && q.Status == "queued" {
				evidence.ReviewHandoffAdmitted = true
			}
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
		if e.Evidence == nil {
			lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "lease_evidence_unavailable"
			continue
		}
		usage, processErr := processes.InUse(ctx, lanes[i].Path)
		if processErr != nil {
			lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "process_evidence_unavailable"
			continue
		}
		lanes[i].ActiveCWD, lanes[i].OpenFile = usage.CWD, usage.OpenFile
		evidence, evidenceErr := e.Evidence.Read(ctx, root, e.HostID, lanes[i])
		if evidenceErr != nil {
			lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "canonical_lifecycle_evidence_unavailable"
			continue
		}
		lanes[i].ActiveLease = evidence.ActiveLease
		lanes[i].FailedCandidate = evidence.FailedCandidate
		lanes[i].ReviewHandoffAdmitted = evidence.ReviewHandoffAdmitted
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

// activeTaskReceipt remains a parser for diagnostics and legacy tests only.
// GitWorktreeEnumerator never uses it as retirement authority; canonical
// lifecycle evidence is mandatory in the production census path.
func activeTaskReceipt(worktree string, now time.Time) (bool, error) {
	data, err := os.ReadFile(filepath.Join(worktree, TaskContextFile))
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
	if receipt.LeaseID == "" || receipt.LeaseGeneration <= 0 || receipt.SessionID == "" || receipt.ExpiresAt.IsZero() {
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
