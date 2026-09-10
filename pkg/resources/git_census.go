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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/gitroot"
)

type ProcessUsage struct {
	CWD                 bool
	OpenFile            bool
	ReferencedPath      bool
	MetadataUnavailable bool
	PIDs                []int
}

type ProcessInspector interface {
	InUse(context.Context, string) (ProcessUsage, error)
}

// BatchProcessInspector performs one process-population census for several
// targets. Implementations must retain the same fail-closed evidence as InUse.
type BatchProcessInspector interface {
	InUseMany(context.Context, []string) (map[string]ProcessUsage, error)
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
	ClaimsPath         string
	LaunchClaimsPath   string
	RecoveryClaimsPath string
	TaskClaimsPath     string
	LedgerPath         string
	RepoID             string
	HostID             string
	SignedTarget       func(context.Context, string, RegisteredWorktree) (SignedTarget, error)
}

// SignedTarget is the authenticated receipt identity for a lane. It is kept
// deliberately small so resources does not import dispatch (which would
// create a package cycle through review/worktree).
type SignedTarget struct {
	LeaseID         string
	LeaseGeneration int64
	LeaseTaskRef    string
	Repository      string
	CandidateSHA    string
	Authenticated   bool
}

func (r SQLiteLifecycleEvidence) Read(ctx context.Context, repoRoot, hostID string, lane RegisteredWorktree) (LifecycleEvidence, error) {
	if strings.TrimSpace(r.LedgerPath) == "" || strings.TrimSpace(r.RepoID) == "" || strings.TrimSpace(hostID) == "" {
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
	evidence := LifecycleEvidence{}
	paths := []string{r.ClaimsPath, r.LaunchClaimsPath, r.RecoveryClaimsPath, r.TaskClaimsPath}
	seenStores := make(map[string]struct{})
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, seen := seenStores[path]; seen {
			continue
		}
		seenStores[path] = struct{}{}
		store, openErr := claim.OpenSQLiteLeaseStoreReadOnly(path)
		if openErr != nil {
			return LifecycleEvidence{}, fmt.Errorf("open canonical claim evidence %q: %w", reportPath(root, path), openErr)
		}
		active, activeErr := store.ActiveClaims(ctx, time.Now())
		if activeErr != nil {
			_ = store.Close()
			return LifecycleEvidence{}, fmt.Errorf("read canonical active claims: %w", activeErr)
		}
		knownPath, historyErr := exactClaimPath(ctx, store, root, worktree)
		if historyErr != nil {
			_ = store.Close()
			return LifecycleEvidence{}, historyErr
		}
		isRecoveryStore := filepath.Clean(path) == filepath.Clean(r.RecoveryClaimsPath)
		if knownPath {
			for _, lease := range active {
				if lease == nil {
					continue
				}
				leasePath, resolveErr := filepath.EvalSymlinks(lease.WorktreePath)
				if resolveErr != nil || filepath.Clean(leasePath) != filepath.Clean(worktree) {
					continue
				}
				if lease.Repo != r.RepoID || lease.HoldRepository != r.RepoID || lease.Generation <= 0 || lease.OwnerID == "" || lease.TaskRef == "" || lease.Project == "" || lease.Provider == "" {
					_ = store.Close()
					return LifecycleEvidence{}, errors.New("canonical claim record identity mismatch")
				}
				if r.SignedTarget != nil {
					target, targetErr := r.SignedTarget(ctx, worktree, lane)
					if targetErr != nil || !target.Authenticated || target.LeaseID != fmt.Sprintf("claim:%d", lease.ID) || target.LeaseGeneration != lease.Generation || target.LeaseTaskRef != lease.TaskRef || target.Repository != r.RepoID || (target.CandidateSHA != "" && target.CandidateSHA != lane.Head) {
						_ = store.Close()
						return LifecycleEvidence{}, errors.New("canonical claim receipt identity mismatch")
					}
				}
				evidence.ActiveLease = true
			}
		}
		// A live recovery claim with no target path cannot be joined to this
		// worktree; preserve every target until the signed target binding is
		// available instead of guessing from generation or owner naming.
		for _, lease := range active {
			if lease != nil && lease.WorktreePath == "" && lease.HoldRepository == r.RepoID {
				if !isRecoveryStore || r.SignedTarget == nil {
					_ = store.Close()
					return LifecycleEvidence{}, errors.New("canonical recovery claim has no signed target binding")
				}
				target, targetErr := r.SignedTarget(ctx, worktree, lane)
				if targetErr != nil || !target.Authenticated || target.LeaseID != fmt.Sprintf("claim:%d", lease.ID) || target.LeaseGeneration != lease.Generation || target.LeaseTaskRef != lease.TaskRef || target.Repository != r.RepoID || (target.CandidateSHA != "" && target.CandidateSHA != lane.Head) {
					_ = store.Close()
					return LifecycleEvidence{}, errors.New("canonical recovery claim signed target mismatch")
				}
				evidence.ActiveLease = true
			}
		}
		if err := store.Close(); err != nil {
			return LifecycleEvidence{}, fmt.Errorf("close canonical claim evidence: %w", err)
		}
	}
	if err := readReviewLifecycle(r.LedgerPath, lane.Head, &evidence); err != nil {
		return LifecycleEvidence{}, err
	}
	return evidence, nil
}

func exactClaimPath(ctx context.Context, store *claim.SQLiteLeaseStore, root, worktree string) (bool, error) {
	paths, err := store.DistinctWorktreePaths(ctx)
	if err != nil {
		return false, fmt.Errorf("read canonical claim history: %w", err)
	}
	for _, path := range paths {
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr == nil && filepath.Clean(resolved) == filepath.Clean(worktree) {
			return true, nil
		}
	}
	_ = root
	return false, nil
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
	queue, err := read(filepath.Join(filepath.Dir(path), gitroot.ReviewQueueLeaf))
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

type gitStatusResult struct {
	dirty, untracked bool
	err              error
}

func batchGitStatus(ctx context.Context, lanes []RegisteredWorktree) map[string]gitStatusResult {
	results := make(map[string]gitStatusResult, len(lanes))
	paths := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		resolved, err := filepath.EvalSymlinks(lane.Path)
		if err == nil {
			paths = append(paths, filepath.Clean(resolved))
		}
	}
	if len(paths) == 0 {
		return results
	}
	jobs := make(chan string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	workers := 8
	if len(paths) < workers {
		workers = len(paths)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case path, ok := <-jobs:
					if !ok {
						return
					}
					dirty, untracked, err := gitStatus(ctx, path)
					mu.Lock()
					results[path] = gitStatusResult{dirty: dirty, untracked: untracked, err: err}
					mu.Unlock()
				}
			}
		}()
	}
sendJobs:
	for _, path := range paths {
		select {
		case <-ctx.Done():
			break sendJobs
		case jobs <- path:
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

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
	statusResults := batchGitStatus(ctx, lanes)
	var batchUsage map[string]ProcessUsage
	var batchErr error
	batchAttempted := false
	if batch, ok := processes.(BatchProcessInspector); ok {
		batchAttempted = true
		paths := make([]string, 0, len(lanes))
		for i := range lanes {
			resolved, resolveErr := filepath.EvalSymlinks(lanes[i].Path)
			if resolveErr == nil {
				paths = append(paths, filepath.Clean(resolved))
			}
		}
		batchUsage, batchErr = batch.InUseMany(ctx, paths)
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
		statusResult, statusFound := statusResults[filepath.Clean(lanes[i].Path)]
		if !statusFound {
			lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "git_status_unavailable"
			continue
		}
		dirty, untracked, statusErr := statusResult.dirty, statusResult.untracked, statusResult.err
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
		var usage ProcessUsage
		var processErr error
		if batchAttempted {
			var found bool
			usage, found = batchUsage[filepath.Clean(lanes[i].Path)]
			if !found || batchErr != nil {
				processErr = errors.New("batched process evidence unavailable")
			}
		} else {
			usage, processErr = processes.InUse(ctx, lanes[i].Path)
		}
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

const maxBatchProcessTargets = 64

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
	openUsage, seen, err := p.lsofPath(ctx, executable, timeout, maxOutput, resolved)
	if err != nil {
		return ProcessUsage{}, err
	}
	usage.CWD, usage.OpenFile = openUsage.CWD, openUsage.OpenFile
	usage.PIDs = append(usage.PIDs, openUsage.PIDs...)
	sortInts(usage.PIDs)
	processCtx, cancelProcesses := context.WithTimeout(ctx, timeout)
	defer cancelProcesses()
	allPIDs, processListErr := listProcessIDs(processCtx)
	if processListErr != nil {
		return ProcessUsage{}, processListErr
	}
	for _, pid := range allPIDs {
		if processCtx.Err() != nil {
			break
		}
		if _, alreadySeen := seen[pid]; !alreadySeen {
			usage.PIDs = append(usage.PIDs, pid)
		}
		referenced, referenceErr := processReferences(processCtx, pid, resolved)
		if referenceErr != nil {
			usage.MetadataUnavailable = true
			continue
		}
		usage.ReferencedPath = usage.ReferencedPath || referenced
	}
	sortInts(usage.PIDs)
	return usage, nil
}

func lsofPositiveExitOne(err error, stdout, stderr []byte) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || len(bytes.TrimSpace(stderr)) != 0 {
		return false
	}
	currentPID := 0
	for _, line := range strings.Split(string(stdout), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, parseErr := strconv.Atoi(strings.TrimSpace(line[1:]))
			if parseErr == nil {
				currentPID = pid
			}
		case 'f':
			descriptor := strings.TrimSpace(line[1:])
			if currentPID != 0 && descriptor != "rtd" && descriptor != "txt" && descriptor != "mem" {
				return true
			}
		}
	}
	return false
}

// InUseMany keeps the expensive process population and metadata walk shared
// across a bounded orphan batch. lsof remains target-scoped, so open handles
// and cwd evidence cannot be confused between targets.
func (p LSOFProcessInspector) InUseMany(ctx context.Context, paths []string) (map[string]ProcessUsage, error) {
	if len(paths) == 0 {
		return map[string]ProcessUsage{}, nil
	}
	executable := strings.TrimSpace(p.Executable)
	if executable == "" {
		var err error
		executable, err = exec.LookPath("lsof")
		if err != nil {
			return nil, err
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
	usage := make(map[string]ProcessUsage, len(paths))
	resolvedPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		resolved = filepath.Clean(resolved)
		if _, seen := usage[resolved]; seen {
			continue
		}
		resolvedPaths = append(resolvedPaths, resolved)
	}
	probePaths := resolvedPaths
	if len(probePaths) > maxBatchProcessTargets {
		probePaths = probePaths[:maxBatchProcessTargets]
	}
	openUsage, err := p.lsofPaths(ctx, executable, timeout, maxOutput, probePaths)
	if err != nil {
		for _, path := range probePaths {
			usage[path] = ProcessUsage{MetadataUnavailable: true}
		}
		for _, path := range resolvedPaths[len(probePaths):] {
			usage[path] = ProcessUsage{MetadataUnavailable: true}
		}
	} else {
		for path, entry := range openUsage {
			usage[path] = entry
		}
	}
	processCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	allPIDs, err := listProcessIDs(processCtx)
	if err != nil {
		for path, entry := range usage {
			entry.MetadataUnavailable = true
			usage[path] = entry
		}
		return usage, nil
	}
	owners, ownerErr := snapshotProcessOwners(processCtx)
	if ownerErr != nil {
		for path, entry := range usage {
			entry.MetadataUnavailable = true
			usage[path] = entry
		}
		return usage, ownerErr
	}
	for _, pid := range allPIDs {
		if processCtx.Err() != nil {
			return nil, processCtx.Err()
		}
		references, referenceErr := processReferencesManyWithOwners(processCtx, pid, resolvedPaths, owners)
		if referenceErr != nil {
			for path := range usage {
				usage[path] = markMetadataUnavailable(usage[path], pid)
			}
			continue
		}
		for path, referenced := range references {
			entry := usage[path]
			entry.ReferencedPath = entry.ReferencedPath || referenced
			entry.PIDs = append(entry.PIDs, pid)
			usage[path] = entry
		}
	}
	for path, entry := range usage {
		sortInts(entry.PIDs)
		usage[path] = entry
	}
	return usage, nil
}

func (p LSOFProcessInspector) lsofPath(ctx context.Context, executable string, timeout time.Duration, maxOutput int, resolved string) (ProcessUsage, map[int]struct{}, error) {
	all, err := p.lsofPaths(ctx, executable, timeout, maxOutput, []string{resolved})
	if err != nil {
		return ProcessUsage{}, nil, err
	}
	usage := all[resolved]
	seen := make(map[int]struct{}, len(usage.PIDs))
	for _, pid := range usage.PIDs {
		seen[pid] = struct{}{}
	}
	return usage, seen, nil
}

func (p LSOFProcessInspector) lsofPaths(ctx context.Context, executable string, timeout time.Duration, maxOutput int, paths []string) (map[string]ProcessUsage, error) {
	if len(paths) == 0 {
		return map[string]ProcessUsage{}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"-nP", "-Ffnp"}
	for _, path := range paths {
		args = append(args, "+D", path)
	}
	cmd := exec.CommandContext(probeCtx, executable, args...)
	var stdout, stderr limitedOutput
	stdout.remaining, stderr.remaining = maxOutput, maxOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if stdout.overflow || stderr.overflow {
		return nil, errors.New("lsof output exceeded bound")
	}
	if err != nil && !lsofNoMatch(err, stdout.Bytes(), stderr.Bytes()) {
		// macOS lsof can return status 1 after emitting a useful positive
		// record when a process exits during the recursive +D walk. That is
		// not an idle result: preserve the positive owner evidence and let the
		// caller refuse removal. Output with diagnostics, or output without a
		// positive slot descriptor, remains an unavailable/failed census.
		if !lsofPositiveExitOne(err, stdout.Bytes(), stderr.Bytes()) {
			return nil, err
		}
	}
	usage := make(map[string]ProcessUsage, len(paths))
	for _, path := range paths {
		usage[path] = ProcessUsage{}
	}
	seen := make(map[int]struct{})
	currentPID := 0
	currentDescriptor := ""
	for _, line := range strings.Split(string(stdout.Bytes()), "\n") {
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
			currentDescriptor = strings.TrimSpace(line[1:])
		case 'n':
			name := filepath.Clean(strings.TrimSpace(line[1:]))
			for path, entry := range usage {
				if !containedPath(path, name) {
					continue
				}
				if currentDescriptor == "cwd" {
					entry.CWD = true
				} else if currentPID != 0 && currentDescriptor != "rtd" && currentDescriptor != "txt" && currentDescriptor != "mem" {
					entry.OpenFile = true
				}
				usage[path] = entry
			}
		}
	}
	for path, entry := range usage {
		for pid := range seen {
			entry.PIDs = append(entry.PIDs, pid)
		}
		sortInts(entry.PIDs)
		usage[path] = entry
	}
	return usage, nil
}

func markMetadataUnavailable(usage ProcessUsage, pid int) ProcessUsage {
	usage.MetadataUnavailable = true
	usage.PIDs = append(usage.PIDs, pid)
	return usage
}

// lsof uses exit status 1 for both "no matching open files" and diagnostics
// from unrelated namespaces/processes. Only its two known WSL filesystem
// warnings, including their continuation lines, are ignorable. Permission,
// target, incomplete, and unknown diagnostics remain observation errors.
func lsofNoMatch(err error, stdout, stderr []byte) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || len(bytes.TrimSpace(stdout)) != 0 {
		return false
	}
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return true
	}
	warnings := map[string]bool{
		"lsof: WARNING: can't stat() hugetlbfs file system /dev/hugepages": true,
		"lsof: WARNING: can't stat() mqueue file system /dev/mqueue":       true,
	}
	const continuation = "Output information may be incomplete."
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !warnings[line] || i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != continuation {
			return false
		}
		i++
	}
	return true
}

func listProcessIDs(ctx context.Context) ([]int, error) {
	ps, err := exec.LookPath("ps")
	if err != nil {
		return nil, fmt.Errorf("process list unavailable: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, ps, "-axo", "pid=").Output()
	if err != nil {
		return nil, fmt.Errorf("read process list: %w", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		pid, parseErr := strconv.Atoi(strings.TrimSpace(line))
		if parseErr == nil && pid > 0 {
			pids = append(pids, pid)
		}
		if len(pids) >= 32768 {
			break
		}
	}
	if len(pids) == 0 {
		return nil, errors.New("process list is empty")
	}
	return pids, nil
}

func snapshotProcessOwners(ctx context.Context) (map[int]int, error) {
	ps, err := exec.LookPath("ps")
	if err != nil {
		return nil, fmt.Errorf("process owner snapshot unavailable: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Darwin and Linux both support this exact BSD ps field contract. The
	// complete pid/uid table avoids one ps process per PID and does not use a
	// truncated command column as ownership evidence.
	out, err := exec.CommandContext(probeCtx, ps, "-axo", "pid=,uid=").Output()
	if err != nil {
		return nil, fmt.Errorf("read process owner snapshot: %w", err)
	}
	owners := make(map[int]int)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, errors.New("process owner snapshot has malformed row")
		}
		pid, pidErr := strconv.Atoi(fields[0])
		uid, uidErr := strconv.Atoi(fields[1])
		if pidErr != nil || uidErr != nil || pid <= 0 || uid < 0 {
			return nil, errors.New("process owner snapshot has invalid pid or uid")
		}
		owners[pid] = uid
	}
	if len(owners) == 0 {
		return nil, errors.New("process owner snapshot is empty")
	}
	return owners, nil
}

// processReferences closes the gap between filesystem handles and a process
// that intends to recreate/use a cache through GOCACHE, argv, or a mapped
// executable/database. Metadata for a foreign process is not deletion
// authority for a private target: that process cannot name or open a target
// whose owner-only permissions have already been proved by the governor. A
// same-owner or owner-unknown failure remains an error and is fail-closed.
func processReferences(ctx context.Context, pid int, path string) (bool, error) {
	references, err := processReferencesMany(ctx, pid, []string{path})
	return references[path], err
}

func processReferencesMany(ctx context.Context, pid int, paths []string) (map[string]bool, error) {
	return processReferencesManyWithOwners(ctx, pid, paths, nil)
}

func processReferencesManyWithOwners(ctx context.Context, pid int, paths []string, owners map[int]int) (map[string]bool, error) {
	references := make(map[string]bool, len(paths))
	for _, path := range paths {
		references[path] = false
	}
	if procData, procErr := readProcessProc(pid); procErr == nil {
		for path := range references {
			needle := []byte(path)
			for _, data := range procData {
				if bytes.Contains(data, needle) {
					references[path] = true
					break
				}
			}
		}
		return references, nil
	} else if os.IsNotExist(procErr) && runtime.GOOS != "darwin" {
		return references, nil
	} else if owner, ok := owners[pid]; ok {
		if owner != os.Getuid() {
			return references, nil
		}
		if runtime.GOOS == "darwin" {
			args, argsErr := readDarwinProcessArgs(pid)
			if argsErr != nil {
				if errors.Is(argsErr, os.ErrProcessDone) {
					return references, nil
				}
				return references, fmt.Errorf("read process argv/environment for pid %d: %w", pid, argsErr)
			}
			for path := range references {
				references[path] = bytes.Contains(args, []byte(path))
			}
			return references, nil
		}
		return references, fmt.Errorf("read same-owner process metadata for pid %d: %w", pid, procErr)
	} else if owners != nil {
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return references, nil
		}
		return references, fmt.Errorf("process owner missing from snapshot for pid %d", pid)
	} else if foreign, gone, ownerErr := foreignOrGoneProcess(ctx, pid); ownerErr != nil {
		return references, ownerErr
	} else if gone || foreign {
		return references, nil
	}
	ps, err := exec.LookPath("ps")
	if err != nil {
		return references, fmt.Errorf("process metadata unavailable for pid %d: %w", pid, err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	psArgs := []string{"-ww", "-p", strconv.Itoa(pid), "-o", "command="}
	if runtime.GOOS == "darwin" {
		// BSD ps requires each of -E and -ww to be an option. Do not add -o:
		// custom output fields suppress the environment that -E displays.
		psArgs = []string{"-E", "-ww", "-p", strconv.Itoa(pid)}
	}
	out, err := exec.CommandContext(probeCtx, ps, psArgs...).Output()
	if err != nil {
		if foreign, gone, ownerErr := foreignOrGoneProcess(ctx, pid); ownerErr == nil && (foreign || gone) {
			return references, nil
		}
		return references, fmt.Errorf("read process argv/environment for pid %d: %w", pid, err)
	}
	for path := range references {
		if bytes.Contains(out, []byte(path)) {
			references[path] = true
		}
	}
	if runtime.GOOS == "darwin" {
		// kern.procargs2 is the unprivileged same-user Darwin process surface
		// that includes the environment; ps -E is not reliable with custom
		// output and launchctl procinfo is root-only.
		if args, argsErr := readDarwinProcessArgs(pid); argsErr == nil {
			for path := range references {
				references[path] = bytes.Contains(args, []byte(path))
			}
			return references, nil
		} else if foreign, gone, ownerErr := foreignOrGoneProcess(ctx, pid); ownerErr == nil && (foreign || gone) {
			return references, nil
		} else {
			return references, fmt.Errorf("read process argv/environment for pid %d: %w", pid, argsErr)
		}
	}
	// lsof's `+D` census above includes mapped files (the `mem` descriptor),
	// so a second Darwin vmmap walk would duplicate that proof while turning
	// unrelated same-user processes into false metadata failures. Keep vmmap
	// out of the per-PID path: relevant mapped references are already in
	// usage.OpenFile and remain a hard guard.
	return references, nil
}

// foreignOrGoneProcess distinguishes an inaccessible unrelated process from
// an inaccessible process owned by this user. The former cannot authorize a
// reference into an owner-only cache; the latter must retain the target.
func foreignOrGoneProcess(ctx context.Context, pid int) (foreign, gone bool, err error) {
	if pid <= 0 {
		return false, true, nil
	}
	if runtime.GOOS == "linux" {
		data, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
		if errors.Is(readErr, os.ErrNotExist) {
			return false, true, nil
		}
		if readErr != nil {
			// Linux may deny /proc metadata for root or otherwise foreign
			// processes even when the public ps owner field is readable. That
			// owner result is sufficient to classify a foreign process, but it
			// never authorizes ignoring an inaccessible same-owner process.
			if foreign, gone, ownerErr := processOwnerViaPS(ctx, pid); ownerErr == nil {
				return foreign, gone, nil
			}
			return false, false, fmt.Errorf("read process owner for pid %d: %w", pid, readErr)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "Uid:") {
				continue
			}
			fields := strings.Fields(line[len("Uid:"):])
			if len(fields) == 0 {
				break
			}
			uid, parseErr := strconv.Atoi(fields[0])
			if parseErr != nil {
				return false, false, fmt.Errorf("parse process owner for pid %d: %w", pid, parseErr)
			}
			return uid != os.Getuid(), false, nil
		}
		return false, false, fmt.Errorf("process owner unavailable for pid %d", pid)
	}
	return processOwnerViaPS(ctx, pid)
}

func processOwnerViaPS(ctx context.Context, pid int) (foreign, gone bool, err error) {
	ps, lookErr := exec.LookPath("ps")
	if lookErr != nil {
		return false, false, fmt.Errorf("process owner unavailable for pid %d: %w", pid, lookErr)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	out, runErr := exec.CommandContext(probeCtx, ps, "-p", strconv.Itoa(pid), "-o", "uid=").Output()
	if runErr != nil {
		if probeCtx.Err() != nil || errors.Is(runErr, os.ErrProcessDone) {
			return false, true, nil
		}
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return false, true, nil
		}
		return false, false, fmt.Errorf("read process owner for pid %d: %w", pid, runErr)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return false, true, nil
	}
	uid, parseErr := strconv.Atoi(strings.Fields(value)[0])
	if parseErr != nil {
		return false, false, fmt.Errorf("parse process owner for pid %d: %w", pid, parseErr)
	}
	return uid != os.Getuid(), false, nil
}

func readProcessProc(pid int) ([][]byte, error) {
	base := filepath.Join("/proc", strconv.Itoa(pid))
	names := []string{"environ", "cmdline", "maps"}
	data := make([][]byte, 0, len(names))
	for _, name := range names {
		value, err := os.ReadFile(filepath.Join(base, name))
		if err != nil {
			return nil, err
		}
		data = append(data, value)
	}
	return data, nil
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
