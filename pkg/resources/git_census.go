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
	// MetadataCause carries the actionable reason the census was marked
	// unavailable (deadline vs per-PID probe error, with the pid or phase
	// that produced it). It never changes fail-closed behavior; consumers
	// surface it so an incomplete census is diagnosable instead of bare.
	MetadataCause string
	PIDs          []int
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
	// SharedPopulation, when non-nil, is populated by the registered census
	// with the host-wide process snapshot it captured, so a later orphan
	// census stage on the same run reuses it instead of rescanning the
	// process population and owner table. Consumers own the pointer and
	// hand the same one to the governor.
	SharedPopulation *ProcessPopulation
	// ProbeStats, when non-nil, receives the real target-probe progress of
	// the registered census batch (completed vs deferred lsof probes) so the
	// governor's report can state probe progress instead of inferring it
	// from scanned counts. Consumers own the pointer and hand the same one
	// to the governor.
	ProbeStats *BatchProbeStats
	// Landing, when non-nil, decides landing by CONTENT instead of ancestry
	// alone. It is injected rather than imported because the whole-range
	// proof lives above this package and depending on it from here is a
	// cycle. Nil keeps the ancestry default.
	Landing LandingPredicate
	// CensusWindow bounds one sweep's per-lane evidence to a deterministic
	// rotating window of the registered ring, so an expensive lane can no
	// longer consume the whole sweep budget before any lane qualifies (the
	// measured 0/571 failure: batchGitStatus over every registered lane ate
	// the registered phase's entire deadline). The full registered list is
	// still enumerated, still returned, and still deduplicated for the
	// report; unselected lanes are preserved fail-closed as unknown with the
	// census_window_deferred reason and never reach any expensive probe.
	// Non-positive uses registeredCensusWindow.
	CensusWindow int
	// WindowStart, when non-nil, activates windowing and reports where the
	// rotating window starts for a registered ring of the given length. The
	// durable-cursor convention (mirroring the orphan census): a valid
	// persisted cursor takes precedence; a missing or unparsable one falls
	// back to a deterministic stride and never infers eligibility. Nil
	// disables windowing entirely: every existing caller keeps the
	// evaluate-all-lanes behavior.
	WindowStart func(entryCount int) int
	// WindowAdvance, when non-nil, persists the next window start after the
	// window's lanes were all accounted. The advance runs only after the
	// per-lane loop completed, so an interrupted sweep re-processes the same
	// window next time (idempotent) instead of silently skipping it.
	WindowAdvance func(next int) error
	// WindowAdvanceErr, when non-nil, receives a failed cursor advance as a
	// partial diagnostic (same holder pattern as ProbeStats): broken
	// progress persistence must not be silent, and it must never change any
	// lane's evidence decision or eligibility.
	WindowAdvanceErr *string
}

// registeredCensusWindow is the default bounded window for one registered
// census sweep: wide enough to make steady progress per sweep, narrow enough
// that the batch git-status and process population stay a small fraction of
// the registered phase budget. Measured CI cost is ~78ms/lane for the batch
// status (571 lanes took 44.6s on 8 workers), so a 16-lane window costs
// ~1.3s of status plus one shared population capture and 16 bounded measure
// walks — a small fraction of a 45s sweep — while still qualifying lanes
// every sweep instead of starving the whole ring behind one slow probe.
const registeredCensusWindow = 16

// LandingProbe is the already-pinned identity one landing decision is made
// about. HEAD and the base are resolved to object names before the probe
// runs, so a ref that moves while the probe is in flight cannot change what
// was proved.
type LandingProbe struct {
	WorktreePath string
	Branch       string
	HeadSHA      string
	BaseRef      string
	BaseSHA      string
}

// LandingPredicate reports whether the probe's work is contained in the base
// at its current tip.
//
// Ancestry alone cannot answer this: a rebase or squash replays the work
// under a new object name, so a landed lane is never an ancestor and reports
// unmerged forever. An error means UNKNOWN, never "not landed" -- both
// preserve the lane, but the recorded reason must not claim knowledge the
// probe did not have.
type LandingPredicate func(ctx context.Context, probe LandingProbe) (bool, error)

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
	return e.evaluate(ctx, root, lanes, baseRef)
}

// evaluate runs the registered-lane census (status, measure, process batch,
// evidence) over the enumerated lanes. Split from List so the shared
// population capture/reuse is exercisable without a live git enumeration.
func (e GitWorktreeEnumerator) evaluate(ctx context.Context, root string, lanes []RegisteredWorktree, baseRef string) ([]RegisteredWorktree, error) {
	if len(lanes) == 0 {
		return nil, errors.New("registered worktree allowlist is empty")
	}
	processes := e.Processes
	if processes == nil {
		processes = LSOFProcessInspector{Timeout: 2 * time.Second, MaxOutputBytes: 1 << 20}
	}
	// Deterministic bounded rotating window: when the durable-cursor hook is
	// wired, only the selected slice of the registered ring pays for the
	// expensive per-lane operations this sweep. Selection happens by index
	// before any probe runs, and the full ring stays in the returned report.
	var statusResults map[string]gitStatusResult
	selectedIdx := map[int]struct{}{}
	windowActive := false
	windowStart := 0
	windowLimit := 0
	if e.WindowStart != nil && len(lanes) > 0 {
		windowActive = true
		limit := e.CensusWindow
		if limit <= 0 {
			limit = registeredCensusWindow
		}
		if limit > len(lanes) {
			limit = len(lanes)
		}
		windowLimit = limit
		windowStart = e.WindowStart(len(lanes))
		switch {
		case windowStart < 0:
			// A negative cursor is corrupt persisted state, not a position.
			// Reducing it would NOT bring it into range: Go's % keeps the
			// dividend's sign, so -5 % 16 stays -5 and the selection below
			// indexes lanes[-5] and panics. The deterministic policy is to
			// start at the ring's head: the sweep still makes progress, every
			// lane remains reachable, and the advance at the end writes a
			// sane cursor back over the corrupt one.
			windowStart = 0
		case windowStart >= len(lanes):
			windowStart %= len(lanes)
		}
		for offset := 0; offset < limit; offset++ {
			selectedIdx[(windowStart+offset)%len(lanes)] = struct{}{}
		}
		selected := make([]RegisteredWorktree, 0, limit)
		for offset := 0; offset < limit; offset++ {
			selected = append(selected, lanes[(windowStart+offset)%len(lanes)])
		}
		statusResults = batchGitStatus(ctx, selected)
	} else {
		statusResults = batchGitStatus(ctx, lanes)
	}
	var batchUsage map[string]ProcessUsage
	var batchErr error
	var batchStats BatchProbeStats
	batchAttempted := false
	if batch, ok := processes.(BatchProcessInspector); ok {
		batchAttempted = true
		// The population batch pays for the window's paths only: unselected
		// lanes must not appear in any process probe this sweep.
		paths := make([]string, 0, len(lanes))
		for i := range lanes {
			if windowActive {
				if _, ok := selectedIdx[i]; !ok {
					continue
				}
			}
			resolved, resolveErr := filepath.EvalSymlinks(lanes[i].Path)
			if resolveErr == nil {
				paths = append(paths, filepath.Clean(resolved))
			}
		}
		reused := false
		if shared, isPopAware := batch.(PopulationAwareBatchInspector); isPopAware && e.SharedPopulation != nil {
			if len(e.SharedPopulation.PIDs) > 0 {
				batchUsage, batchStats, batchErr = shared.InUseManyPopulation(ctx, paths, e.SharedPopulation)
				reused = true
			} else if snap, canSnap := processes.(PopulationSnapshooter); canSnap {
				// FAC-613: capture the population ONCE, before the batch,
				// so both census stages share this exact snapshot instead
				// of the batch rescanning and a second capture following.
				// A failed capture fails the batch closed: no lane may run
				// on evidence the census could not gather.
				if population, snapErr := snap.SnapshotPopulation(ctx); snapErr != nil {
					batchErr = fmt.Errorf("process population snapshot failed: %w", snapErr)
				} else if population != nil {
					*e.SharedPopulation = *population
					batchUsage, batchStats, batchErr = shared.InUseManyPopulation(ctx, paths, e.SharedPopulation)
					reused = true
				}
			}
		}
		if !reused && batchErr == nil {
			batchUsage, batchErr = batch.InUseMany(ctx, paths)
		}
		if e.ProbeStats != nil {
			*e.ProbeStats = batchStats
		}
	}
	// Pin the base ONCE per enumeration, so every lane in one census is judged
	// against the same tip. An unpinnable base leaves this empty and each
	// content probe then fails closed rather than proving against a ref that
	// may move underneath it.
	basePin := e.pinBase(ctx, root, baseRef)
	accounted := 0
	for i := range lanes {
		// Unselected lanes of the rotating window are preserved fail-closed
		// BEFORE any expensive per-lane operation: no status, process,
		// ownership, landing, or lease evidence call may reach them this
		// sweep. The full ring stays in the report; their next evidence
		// comes when the cursor rotates to them.
		if windowActive {
			if _, ok := selectedIdx[i]; !ok {
				lanes[i].State, lanes[i].PreserveReason = LaneUnknown, "census_window_deferred"
				continue
			}
			// A cancelled sweep truncates the window: this lane and every
			// remaining selected lane stay unexamined (conservatively
			// unknown, mirroring the budget-exhaustion convention), the
			// loop stops, and the cursor advances only by the lanes that
			// were actually accounted - so the next sweep re-processes the
			// unexamined remainder instead of skipping it. Unselected
			// lanes of the remainder keep the explicit deferred reason.
			if ctx.Err() != nil {
				for j := i; j < len(lanes); j++ {
					if _, ok := selectedIdx[j]; ok {
						lanes[j].State, lanes[j].PreserveReason = LaneUnknown, "census_budget_exhausted"
					} else {
						lanes[j].State, lanes[j].PreserveReason = LaneUnknown, "census_window_deferred"
					}
				}
				break
			}
		}
		// This lane is now EXAMINED, and the cursor accounts for examination
		// rather than for success.
		//
		// Counting at the end of the body instead made the cursor a count of
		// lanes that completed the whole pipeline, and eight paths below leave
		// early with a conservative unknown state: unreadable realpath, stat,
		// git status (absent or failed), merge state, missing lease evidence,
		// process evidence, and lifecycle evidence. A window whose lanes all
		// took one of those advanced the cursor by nothing, so the next sweep
		// selected the same slice, failed the same way, and the fleet could
		// starve on an unprovable window forever.
		//
		// Everything that must NOT count is already excluded above: an
		// unselected lane continues before this point, and a cancelled sweep
		// breaks before it, so a lane the sweep never looked at is never
		// charged to the cursor.
		accounted++
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
		merged, mergeErr := e.landed(ctx, lanes[i], baseRef, basePin)
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
			} else if usage.MetadataUnavailable {
				// The batch ran but this target's evidence is incomplete
				// (probe failed or budget expired before it): the lane must
				// stay unknown, never read as a definitive inactive result.
				processErr = fmt.Errorf("batched process metadata unavailable: %s", usage.MetadataCause)
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
	// The cursor advances by the lanes actually EXAMINED: a window whose
	// selected lanes were all reached advances its full width whatever each
	// lane concluded, and a sweep truncated by cancellation advances exactly
	// the examined prefix so the unexamined remainder is re-processed next
	// sweep instead of skipped. A failed persistence write is recorded as a
	// partial diagnostic and never changes any lane's evidence or eligibility.
	if accounted > windowLimit {
		accounted = windowLimit
	}
	if windowActive && e.WindowAdvance != nil {
		next := (windowStart + accounted) % len(lanes)
		if err := e.WindowAdvance(next); err != nil && e.WindowAdvanceErr != nil {
			*e.WindowAdvanceErr = fmt.Sprintf("registered census cursor advance: %v", err)
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

// pinBase resolves the base ref to an object name for this enumeration. It is
// only needed by a wired content predicate, so an enumerator without one pays
// nothing. An unresolvable base returns empty and every probe then refuses.
func (e GitWorktreeEnumerator) pinBase(ctx context.Context, root, baseRef string) string {
	if e.Landing == nil {
		return ""
	}
	out, err := gitOutput(ctx, root, "rev-parse", "--verify", baseRef+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// landed decides one lane's landing, preserving the ancestry-only default for
// any consumer that wires no predicate.
//
// Ancestry runs first even when a predicate exists: it is cheap, it is
// authoritative when it says yes, and it keeps the expensive proof for the
// rewritten cases that actually need it.
//
// Cancellation bound, stated honestly: the predicate is one synchronous proof
// and is NOT interruptible once begun, so the granularity is a whole proof,
// never part of one. The deadline is observed on both sides -- a cancelled
// census neither starts a proof nor trusts one that finished after its caller
// gave up -- and the call is inline, so no goroutine is left running into a
// result nobody will read.
func (e GitWorktreeEnumerator) landed(ctx context.Context, lane RegisteredWorktree, baseRef, basePin string) (bool, error) {
	if e.Landing == nil {
		// Unwired: live HEAD against the live ref, exactly as this census has
		// always asked. Preserved deliberately; only the wired branch changes.
		return gitMerged(ctx, lane.Path, baseRef)
	}
	// Wired: EVERY answer must be about the identity the census reported.
	// Validate the pins before trusting either path -- the cheap ancestry
	// answer included. Asking git about live HEAD and a live ref here would
	// let a branch that moved after enumeration bless a lane whose pinned
	// candidate never landed, and the report would name a probe the yes was
	// not about.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if strings.TrimSpace(basePin) == "" {
		return false, fmt.Errorf("landing base %q could not be pinned", baseRef)
	}
	if strings.TrimSpace(lane.Head) == "" || strings.TrimSpace(lane.Branch) == "" {
		return false, fmt.Errorf("landing probe for %q requires a pinned head and branch", lane.Path)
	}
	// Ancestry stays the cheap authoritative yes, now asked about the pinned
	// pair rather than whatever the refs point at this instant.
	if merged, err := GitCommitIsAncestor(ctx, lane.Path, lane.Head, basePin); err != nil || merged {
		return merged, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	landedNow, probeErr := e.Landing(ctx, LandingProbe{
		WorktreePath: lane.Path,
		Branch:       lane.Branch,
		HeadSHA:      lane.Head,
		BaseRef:      baseRef,
		BaseSHA:      basePin,
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if probeErr != nil {
		return false, probeErr
	}
	return landedNow, nil
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
	// processReferencesFn and processReferencesManyFn are test seams for the
	// per-PID reference walk. nil selects the production implementations.
	processReferencesFn     func(ctx context.Context, pid int, path string) (bool, error)
	processReferencesManyFn func(ctx context.Context, pid int, paths []string, owners map[int]int) (map[string]bool, error)
	populationFn            func(ctx context.Context) ([]int, map[int]int, error)
	// openFilesFn is the test seam for the one-shot full-table open-file
	// capture. nil selects the production implementation.
	openFilesFn func(ctx context.Context) (map[int][]ProcessOpenFile, error)
}

const maxBatchProcessTargets = 64

// maxMetadataCauses bounds the per-pid cause sample retained on
// ProcessUsage.MetadataCause: a churny host can error hundreds of probes and
// the census report stays bounded while remaining actionable.
const maxMetadataCauses = 5

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
	usage.CWD, usage.OpenFile, usage.MetadataUnavailable = openUsage.CWD, openUsage.OpenFile, openUsage.MetadataUnavailable
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
			// The walk stopped before consulting every pid: the census is
			// incomplete and must never read as a definitive no-owner
			// result. Mark it unavailable so consumers fail closed.
			usage.MetadataUnavailable = true
			break
		}
		if _, alreadySeen := seen[pid]; !alreadySeen {
			usage.PIDs = append(usage.PIDs, pid)
		}
		referenced, referenceErr := p.referenceProbe()(processCtx, pid, resolved)
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
// across a bounded orphan batch. Production worktree-reap calls it directly,
// with no governor to hand down a population, so it captures the shared
// population ONCE up front — process list, owner table, then a single
// full-table lsof — and derives every target from that one capture. The
// previous order ran the per-target +D descents first and snapshotted the
// process list afterwards, by which point the descents had consumed the
// budget and the snapshot's child context was born expired, deferring every
// target.
//
// Recursion fence: InUseManyPopulation delegates back here exactly when the
// population is nil or carries no PIDs, so it is only ever called from here
// with PIDs present. A population without an open-file table still goes
// through it: the per-target +D probes then run against an already captured
// population, the same fallback minus the fatal ordering.
func (p LSOFProcessInspector) InUseMany(ctx context.Context, paths []string) (map[string]ProcessUsage, error) {
	if len(paths) == 0 {
		return map[string]ProcessUsage{}, nil
	}
	if population, snapErr := p.SnapshotPopulation(ctx); snapErr == nil && population != nil && len(population.PIDs) > 0 {
		usage, _, popErr := p.InUseManyPopulation(ctx, paths, population)
		return usage, popErr
	}
	return p.inUseManyPerTarget(ctx, paths)
}

// inUseManyPerTarget is the legacy target-scoped census: one lsof +D probe
// per target chunk, then the shared reference walk. It is the terminal
// fallback for a population that could not be captured at all, and keeps
// the original fail-closed semantics — an unobservable target is marked
// metadata-unavailable, never reported definitively clean.
func (p LSOFProcessInspector) inUseManyPerTarget(ctx context.Context, paths []string) (map[string]ProcessUsage, error) {
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
	usage, resolvedPaths, _, err := p.lsofBatch(ctx, executable, timeout, maxOutput, paths)
	if err != nil {
		return nil, err
	}
	processCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	allPIDs, err := listProcessIDs(processCtx)
	if err != nil {
		for path, entry := range usage {
			entry.MetadataUnavailable = true
			entry.MetadataCause = fmt.Sprintf("process list snapshot failed: %v", err)
			usage[path] = entry
		}
		return usage, nil
	}
	owners, ownerErr := snapshotProcessOwners(processCtx)
	if ownerErr != nil {
		for path, entry := range usage {
			entry.MetadataUnavailable = true
			entry.MetadataCause = fmt.Sprintf("process owner snapshot failed: %v", ownerErr)
			usage[path] = entry
		}
		return usage, ownerErr
	}
	return p.inUseManyWalk(ctx, timeout, usage, resolvedPaths, allPIDs, owners)
}

// InUseManyPopulation runs the same batched census over an already-captured
// host-wide population snapshot, so a later census stage does not rescan the
// process population and owner table the earlier stage just captured. The
// per-path lsof and reference evidence is still produced fresh for the given
// paths; only the shared population is reused. The returned stats carry the
// real target-probe progress (completed vs deferred lsof probes).
func (p LSOFProcessInspector) InUseManyPopulation(ctx context.Context, paths []string, population *ProcessPopulation) (map[string]ProcessUsage, BatchProbeStats, error) {
	stats := BatchProbeStats{}
	if population == nil || len(population.PIDs) == 0 {
		usage, err := p.InUseMany(ctx, paths)
		return usage, stats, err
	}
	if len(paths) == 0 {
		return map[string]ProcessUsage{}, stats, nil
	}
	// Bulk evidence path: the population snapshot already carries the
	// host-wide open-file table, so every target is observed by in-memory
	// subtree matching — no per-target lsof +D descent, which cannot
	// complete for hundreds of targets inside a bounded census budget.
	if len(population.OpenFiles) > 0 {
		resolvedPaths := make([]string, 0, len(paths))
		seenResolve := make(map[string]struct{}, len(paths))
		for _, path := range paths {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return nil, stats, err
			}
			resolved = filepath.Clean(resolved)
			if _, seen := seenResolve[resolved]; seen {
				continue
			}
			seenResolve[resolved] = struct{}{}
			resolvedPaths = append(resolvedPaths, resolved)
		}
		usage, walkErr := p.bulkUsageFromOpenFiles(ctx, resolvedPaths, population)
		if walkErr != nil {
			return nil, stats, walkErr
		}
		// Per-target truth: a target whose cross-check walk was cut short
		// carries MetadataUnavailable and counts as deferred, never as a
		// completed observation.
		for _, entry := range usage {
			if entry.MetadataUnavailable {
				stats.Deferred++
			} else {
				stats.Completed++
			}
		}
		return usage, stats, nil
	}
	executable := strings.TrimSpace(p.Executable)
	if executable == "" {
		var err error
		executable, err = exec.LookPath("lsof")
		if err != nil {
			return nil, stats, err
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
	usage, resolvedPaths, probeStats, err := p.lsofBatch(ctx, executable, timeout, maxOutput, paths)
	if err != nil {
		return nil, probeStats, err
	}
	walked, walkErr := p.inUseManyWalk(ctx, timeout, usage, resolvedPaths, population.PIDs, population.Owners)
	return walked, probeStats, walkErr
}

// bulkUsageFromOpenFiles derives per-path usage from the captured full-table
// open files. Every resolved path receives an entry: empty PIDs mean the
// table showed no open file under the subtree — a definitive observed
// no-owner result, never a skipped probe. FD semantics mirror the chunked
// +D parser: cwd entries mark CWD, rtd/txt/mem entries are ignored, every
// other fd marks OpenFile.
func (p LSOFProcessInspector) bulkUsageFromOpenFiles(ctx context.Context, resolvedPaths []string, population *ProcessPopulation) (map[string]ProcessUsage, error) {
	usage := make(map[string]ProcessUsage, len(resolvedPaths))
	targets := make(map[string]struct{}, len(resolvedPaths))
	for _, path := range resolvedPaths {
		usage[path] = ProcessUsage{}
		targets[path] = struct{}{}
	}
	// Ancestor lookup reproduces containedPath(target, openPath) for every
	// target: an open path is attributed to EVERY target it equals or sits
	// under, by walking all of the open path's own ancestors. Targets nest —
	// a repository root and its worktrees can both be census targets — and
	// stopping at the nearest ancestor would leave every outer target with
	// zero evidence, indistinguishable from an observed no-owner result and
	// so reap-eligible while a process holds the subtree.
	for pid, opens := range population.OpenFiles {
		for _, open := range opens {
			openPath := filepath.Clean(open.Path)
			if openPath == "." || !filepath.IsAbs(openPath) {
				continue
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var cwd, openFile bool
			switch {
			case open.FD == "cwd":
				cwd = true
			case open.FD != "rtd" && open.FD != "txt" && open.FD != "mem":
				openFile = true
			default:
				continue
			}
			for ancestor := openPath; ; {
				if _, ok := targets[ancestor]; ok {
					entry := usage[ancestor]
					entry.CWD = entry.CWD || cwd
					entry.OpenFile = entry.OpenFile || openFile
					entry.PIDs = append(entry.PIDs, pid)
					usage[ancestor] = entry
				}
				parent := filepath.Dir(ancestor)
				if parent == ancestor {
					break
				}
				ancestor = parent
			}
		}
	}
	for path, entry := range usage {
		sortInts(entry.PIDs)
		usage[path] = entry
	}
	// Keep the argument-reference cross-check: it is cheap and can only
	// widen evidence (it never downgrades a completed observation).
	return p.inUseManyWalk(ctx, p.walkTimeout(), usage, resolvedPaths, population.PIDs, population.Owners)
}

// walkTimeout is the budget for the cheap per-PID argument cross-check.
func (p LSOFProcessInspector) walkTimeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 2 * time.Second
}

// inUseManyWalk is the shared per-PID reference walk over captured evidence.
// It mutates usage in place and returns it.
func (p LSOFProcessInspector) inUseManyWalk(ctx context.Context, timeout time.Duration, usage map[string]ProcessUsage, resolvedPaths []string, allPIDs []int, owners map[int]int) (map[string]ProcessUsage, error) {
	processCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var probeCauses []string
	for _, pid := range allPIDs {
		if processCtx.Err() != nil {
			// An incomplete reference walk must never assert no owner:
			// mark every entry's census unavailable, then propagate the
			// cancellation to callers that check the error as well.
			cause := fmt.Sprintf("reference walk stopped at budget after %d/%d pids: %v", pid, len(allPIDs), processCtx.Err())
			for path, entry := range usage {
				entry = markMetadataUnavailable(entry, pid)
				entry.MetadataCause = cause
				usage[path] = entry
			}
			return usage, processCtx.Err()
		}
		references, referenceErr := p.referencesManyProbe()(processCtx, pid, resolvedPaths, owners)
		if referenceErr != nil {
			if len(probeCauses) < maxMetadataCauses {
				probeCauses = append(probeCauses, fmt.Sprintf("pid %d reference probe: %v", pid, referenceErr))
			}
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
	if len(probeCauses) > 0 {
		summary := fmt.Sprintf("%d per-pid reference probe error(s)", len(probeCauses))
		if len(probeCauses) == maxMetadataCauses {
			summary += " (sample truncated)"
		}
		for _, cause := range probeCauses {
			summary += "; " + cause
		}
		for path, entry := range usage {
			if entry.MetadataUnavailable {
				entry.MetadataCause = summary
				usage[path] = entry
			}
		}
	}
	for path, entry := range usage {
		sortInts(entry.PIDs)
		usage[path] = entry
	}
	return usage, nil
}

// BatchProbeStats reports the real target-probe progress of one batched
// census: Completed counts targets whose target-scoped lsof evidence finished,
// Deferred counts targets that received no lsof evidence (budget exhausted
// before the target, or the probe failed). Deferred targets always carry
// MetadataUnavailable so consumers fail closed; they are never "safe".
type BatchProbeStats struct {
	Completed int `json:"completed"`
	Deferred  int `json:"deferred"`
}

// lsofBatch resolves the sent paths and runs the target-scoped lsof probe
// over ALL of them in consecutive bounded chunks (maxBatchProcessTargets
// paths per spawn), producing the initial per-path usage map both
// InUseMany and InUseManyPopulation share. Every resolved path receives an
// entry: probed chunks keep their real evidence; failed or cancelled chunks
// are marked MetadataUnavailable with the cause — a target whose probe never
// ran can never read as a definitive no-owner result.
func (p LSOFProcessInspector) lsofBatch(ctx context.Context, executable string, timeout time.Duration, maxOutput int, paths []string) (map[string]ProcessUsage, []string, BatchProbeStats, error) {
	usage := make(map[string]ProcessUsage, len(paths))
	resolvedPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, nil, BatchProbeStats{}, err
		}
		resolved = filepath.Clean(resolved)
		if _, seen := usage[resolved]; seen {
			continue
		}
		resolvedPaths = append(resolvedPaths, resolved)
	}
	var stats BatchProbeStats
	chunksRemaining := (len(resolvedPaths) + maxBatchProcessTargets - 1) / maxBatchProcessTargets
	for start := 0; start < len(resolvedPaths); start += maxBatchProcessTargets {
		end := start + maxBatchProcessTargets
		if end > len(resolvedPaths) {
			end = len(resolvedPaths)
		}
		chunk := resolvedPaths[start:end]
		if ctx.Err() != nil {
			// The census budget expired before these targets were probed.
			// They stay metadata-unknown, never "evidence clean".
			cause := fmt.Sprintf("lsof target probe budget exhausted before %d remaining target(s): %v", len(resolvedPaths)-start, ctx.Err())
			for _, path := range resolvedPaths[start:] {
				usage[path] = ProcessUsage{MetadataUnavailable: true, MetadataCause: cause}
				stats.Deferred++
			}
			break
		}
		// The per-chunk deadline is a fair share of the census budget that
		// actually remains, not the tiny per-PID walk knob: a fixed 2s
		// deadline cannot observe a 64-target +D descent on a real host and
		// silently turned every chunk into a failure. The share is bounded
		// by the outer ctx, so the phase budget is never inflated. Without
		// an outer deadline the legacy knob applies.
		chunkTimeout := timeout
		if dl, ok := ctx.Deadline(); ok {
			chunkTimeout = time.Until(dl) / time.Duration(chunksRemaining)
			if chunkTimeout < time.Second {
				chunkTimeout = time.Second
			}
			if time.Until(dl) < time.Second {
				cause := fmt.Sprintf("lsof target probe budget exhausted before %d remaining target(s): %v", len(resolvedPaths)-start, ctx.Err())
				for _, path := range resolvedPaths[start:] {
					usage[path] = ProcessUsage{MetadataUnavailable: true, MetadataCause: cause}
					stats.Deferred++
				}
				break
			}
		}
		openUsage, err := p.lsofPaths(ctx, executable, chunkTimeout, maxOutput, chunk)
		if err != nil {
			cause := fmt.Sprintf("lsof target probe failed: %v", err)
			for _, path := range chunk {
				usage[path] = ProcessUsage{MetadataUnavailable: true, MetadataCause: cause}
			}
			stats.Deferred += len(chunk)
			chunksRemaining--
			continue
		}
		for path, entry := range openUsage {
			usage[path] = entry
		}
		stats.Completed += len(chunk)
		chunksRemaining--
	}
	return usage, resolvedPaths, stats, nil
}

// ProcessOpenFile is one captured open-file entry: the lsof fd field (used
// to distinguish cwd/rtd/txt/mem from ordinary open files) and the path.
type ProcessOpenFile struct {
	FD   string
	Path string
}

// ProcessPopulation is one host-wide process/owner snapshot.
type ProcessPopulation struct {
	PIDs   []int
	Owners map[int]int
	// OpenFiles maps pid -> open-file entries from ONE authoritative
	// full-table lsof capture. When present it is the bulk evidence source:
	// per-target subtree lsof +D descents (which cost ~1s per target on a
	// real host and cannot complete for hundreds of worktrees inside any
	// honest census budget) are skipped in favor of one in-memory match
	// over the captured table.
	OpenFiles map[int][]ProcessOpenFile
}

// PopulationSnapshooter captures the host-wide population a batch census
// walk consumes.
type PopulationSnapshooter interface {
	SnapshotPopulation(ctx context.Context) (*ProcessPopulation, error)
}

// PopulationAwareBatchInspector runs the batched census over a caller-provided
// population snapshot instead of rescanning it, reporting the real
// target-probe progress of the batch.
type PopulationAwareBatchInspector interface {
	BatchProcessInspector
	InUseManyPopulation(ctx context.Context, paths []string, population *ProcessPopulation) (map[string]ProcessUsage, BatchProbeStats, error)
}

// SnapshotPopulation captures the bounded process list and owner table once
// so later census stages can reuse it. It also captures the host-wide
// open-file table in ONE full-table lsof run; a failed capture returns the
// population without OpenFiles and the caller falls back to per-target
// probes rather than pretending targets were observed.
func (p LSOFProcessInspector) SnapshotPopulation(ctx context.Context) (*ProcessPopulation, error) {
	if p.populationFn != nil {
		pids, owners, err := p.populationFn(ctx)
		if err != nil {
			return nil, err
		}
		population := &ProcessPopulation{PIDs: pids, Owners: owners}
		if p.openFilesFn != nil {
			if opens, err := p.openFilesFn(ctx); err == nil {
				population.OpenFiles = opens
			}
		}
		return population, nil
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	processCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pids, err := listProcessIDs(processCtx)
	if err != nil {
		return nil, err
	}
	owners, err := snapshotProcessOwners(processCtx)
	if err != nil {
		return nil, err
	}
	population := &ProcessPopulation{PIDs: pids, Owners: owners}
	// One bounded full-table capture: the census budget shares the same
	// outer ctx, so this can never extend past the run's own deadline.
	if opens, openErr := p.captureOpenFiles(ctx); openErr == nil {
		population.OpenFiles = opens
	}
	return population, nil
}

// captureOpenFiles runs ONE full-table lsof (no per-target +D descent) and
// parses pid -> open paths from the same -Ffnp format the chunked probes
// use. The output bound is generous because the table covers every process.
func (p LSOFProcessInspector) captureOpenFiles(ctx context.Context) (map[int][]ProcessOpenFile, error) {
	if p.openFilesFn != nil {
		return p.openFilesFn(ctx)
	}
	executable := strings.TrimSpace(p.Executable)
	if executable == "" {
		var err error
		executable, err = exec.LookPath("lsof")
		if err != nil {
			return nil, fmt.Errorf("open-file table unavailable: %w", err)
		}
	}
	maxOutput := p.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = 1 << 20
	}
	tableBound := maxOutput * 16
	// Bound the capture. An outer deadline stays authoritative; when the
	// caller set none the probe knob applies, because InUseMany is a public
	// entry point reached with unbounded contexts and a stuck full-table
	// lsof would otherwise run with nothing to stop it. Every other lsof
	// invocation in this file is bounded the same way.
	if _, ok := ctx.Deadline(); !ok {
		fallback := p.Timeout
		if fallback <= 0 {
			fallback = 2 * time.Second
		}
		bounded, cancel := context.WithTimeout(ctx, fallback)
		defer cancel()
		ctx = bounded
	}
	cmd := exec.CommandContext(ctx, executable, "-nP", "-Ffnp")
	var stdout, stderr limitedOutput
	stdout.remaining, stderr.remaining = tableBound, maxOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if stdout.overflow {
		return nil, errors.New("open-file table exceeded bound")
	}
	if stderr.overflow {
		return nil, errors.New("open-file table diagnostics exceeded bound")
	}
	if runErr != nil {
		return nil, fmt.Errorf("open-file table capture: %w", runErr)
	}
	// A PARTIAL full table is worse than no table: it is read as authoritative
	// for every target, so each target the omitted rows would have protected
	// reads as definitively unheld. lsof reports that partiality on stderr and
	// can still exit 0, so the same strict diagnostic contract the per-target
	// probes apply is applied here — anything outside the shared warning
	// allowlist rejects the capture and defers to those probes.
	if !lsofIgnorableDiagnostics(stderr.Bytes()) {
		return nil, fmt.Errorf("open-file table diagnostics: %s", strings.TrimSpace(string(stderr.Bytes())))
	}
	openFiles := make(map[int][]ProcessOpenFile)
	currentPID := 0
	currentDescriptor := ""
	for _, line := range strings.Split(string(stdout.Bytes()), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			if pid, err := strconv.Atoi(strings.TrimSpace(line[1:])); err == nil && pid > 0 {
				currentPID = pid
			}
		case 'f':
			currentDescriptor = strings.TrimSpace(line[1:])
		case 'n':
			if currentPID <= 0 || ctx.Err() != nil {
				continue
			}
			openFiles[currentPID] = append(openFiles[currentPID], ProcessOpenFile{FD: currentDescriptor, Path: strings.TrimSpace(line[1:])})
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return openFiles, nil
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
	if err != nil {
		if lsofNoMatch(err, stdout.Bytes(), stderr.Bytes()) {
			err = nil
		} else if !lsofPositiveExitOne(err, stdout.Bytes(), stderr.Bytes()) {
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
		if (err != nil || len(seen) > 0) && !entry.CWD && !entry.OpenFile {
			entry.MetadataUnavailable = true
		}
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

func (p LSOFProcessInspector) referenceProbe() func(ctx context.Context, pid int, path string) (bool, error) {
	if p.processReferencesFn != nil {
		return p.processReferencesFn
	}
	return processReferences
}

func (p LSOFProcessInspector) referencesManyProbe() func(ctx context.Context, pid int, paths []string, owners map[int]int) (map[string]bool, error) {
	if p.processReferencesManyFn != nil {
		return p.processReferencesManyFn
	}
	return processReferencesManyWithOwners
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
	return lsofIgnorableDiagnostics(stderr)
}

// lsofIgnorableDiagnostics reports whether lsof's stderr carries nothing but
// the two known WSL filesystem warnings, each with its continuation line.
// Every other diagnostic — permission, target, incomplete, unknown — means
// the output cannot be trusted as a complete observation. This is the single
// warning allowlist, shared by the per-target probes and the full-table
// capture so the two cannot drift apart.
func lsofIgnorableDiagnostics(stderr []byte) bool {
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
