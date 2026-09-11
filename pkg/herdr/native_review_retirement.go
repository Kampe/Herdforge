package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/reviewack"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// NativeReviewRetirementOp is the production adapter for the policy. It reads
// canonical ledger/ack state and live Herdr state on every phase; tests should
// use ReviewRetirementOp fakes instead of enabling this against a live fleet.
type NativeReviewRetirementOp struct {
	Root               string
	RepositoryIdentity string
	Ledger             *reviewledger.Ledger
	Pool               *worktree.Pool
	JournalPath        string
}

type retirementPhaseRecord struct {
	Generation, CandidateSHA, Reviewer, BindingDigest string
	Pool, Slot, Worktree, Nonce, Phase                string
	LeaseGeneration                                   int64 `json:"lease_generation"`
}

func canonicalRetirementVerdict(rows []reviewledger.LedgerRow, candidateSHA, reviewer string) (reviewledger.LedgerRow, error) {
	type indexedRow struct {
		row reviewledger.LedgerRow
		idx int
	}
	var verdicts []indexedRow
	byDigest := make(map[string]indexedRow)
	for i, row := range rows {
		if row.Event != string(reviewledger.EventVerdict) || row.SHA != candidateSHA || row.CandidateSHA != candidateSHA || row.Reviewer != reviewer {
			continue
		}
		if row.Verdict != string(reviewledger.VerdictPASS) && row.Verdict != string(reviewledger.VerdictFAIL) && row.Verdict != string(reviewledger.VerdictBLOCKED) {
			return reviewledger.LedgerRow{}, errors.New("matching terminal verdict has an invalid verdict")
		}
		item := indexedRow{row: row, idx: i}
		verdicts = append(verdicts, item)
		byDigest[reviewledger.VerdictEventDigest(row)] = item
	}
	if len(verdicts) == 0 {
		return reviewledger.LedgerRow{}, nil
	}
	superseded := make(map[string]bool)
	for _, item := range verdicts {
		if item.row.Reassesses == "" {
			continue
		}
		prior, ok := byDigest[item.row.Reassesses]
		if !ok || prior.idx >= item.idx {
			return reviewledger.LedgerRow{}, errors.New("matching verdict reassessment is stale or unbound")
		}
		superseded[item.row.Reassesses] = true
	}
	var terminal []reviewledger.LedgerRow
	for _, item := range verdicts {
		if !superseded[reviewledger.VerdictEventDigest(item.row)] {
			terminal = append(terminal, item.row)
		}
	}
	if len(terminal) != 1 {
		return reviewledger.LedgerRow{}, errors.New("ambiguous matching terminal verdict")
	}
	return terminal[0], nil
}

func (n *NativeReviewRetirementOp) Observe(m ReviewRetirementManifest) (ReviewRetirementEvidence, error) {
	if n == nil || n.Ledger == nil || strings.TrimSpace(n.Root) == "" || strings.TrimSpace(n.RepositoryIdentity) == "" {
		return ReviewRetirementEvidence{}, errors.New("native review retirement authority is incomplete")
	}
	if err := ValidateReviewRetirementManifest(m); err != nil {
		return ReviewRetirementEvidence{}, err
	}
	poolGone := false
	if _, err := n.exactPoolSlot(m, true); err != nil {
		if !errors.Is(err, errRetirementPoolAlreadyRemoved) {
			if superseded, supersededErr := n.reviewerSuperseded(m); supersededErr != nil {
				return ReviewRetirementEvidence{}, supersededErr
			} else if superseded {
				return ReviewRetirementEvidence{}, fmt.Errorf("%w: %v", errRetirementSuperseded, err)
			}
			return ReviewRetirementEvidence{}, err
		}
		poolGone = true
	}
	rows, err := n.Ledger.AllRows()
	if err != nil {
		return ReviewRetirementEvidence{}, fmt.Errorf("read review ledger: %w", err)
	}
	var launch, verdict reviewledger.LedgerRow
	launchFound := false
	for _, row := range rows {
		if row.Event == string(reviewledger.EventRecord) && row.SHA == m.CandidateSHA && row.Reviewer == m.Reviewer && row.Lease == m.Nonce {
			if !launchBeforeManifest(row, m.RecordedAt) {
				continue
			}
			if launchFound && !reflect.DeepEqual(launch, row) {
				return ReviewRetirementEvidence{}, errors.New("ambiguous matching launch provenance")
			}
			launch = row
			launchFound = true
		}
	}
	verdict, err = canonicalRetirementVerdict(rows, m.CandidateSHA, m.Reviewer)
	if err != nil {
		return ReviewRetirementEvidence{}, err
	}
	ack, ackErr := reviewack.ReadArtifact(n.Root, m.CandidateSHA, m.Reviewer, verdict.ArtifactDigest)
	if ackErr != nil {
		return ReviewRetirementEvidence{Manifest: m, Launch: launch, Verdict: ReviewRetirementVerdict{Row: verdict}, Repository: n.RepositoryIdentity}, nil
	}
	focused := (*bool)(nil)
	live := ReviewRetirementLive{}
	agents, err := AgentList()
	if err != nil {
		return ReviewRetirementEvidence{}, err
	}
	for _, a := range agents {
		if a.Name != m.Reviewer || a.TabID != m.TabID || a.PaneID != m.PaneID || a.Workspace != m.Workspace || a.TerminalID != m.TerminalID {
			continue
		}
		if m.SessionID != "" && a.Session.Value != m.SessionID {
			return ReviewRetirementEvidence{}, errors.New("review session identity differs from the bound launch")
		}
		live = ReviewRetirementLive{Status: a.Status, Focused: a.Focused, TabPresent: true, Workspace: a.Workspace, TabID: a.TabID, PaneID: a.PaneID, TerminalID: a.TerminalID, SessionID: a.Session.Value, SessionGeneration: ""}
		focused = a.Focused
		procs, pErr := paneProcessesForRetirement(a.PaneID)
		if pErr != nil {
			return ReviewRetirementEvidence{}, pErr
		}
		live.ProcessPresent = len(procs) > 0
		break
	}
	if focused == nil {
		live.Focused = boolPtr(false)
		procs, pErr := paneProcessesForRetirement(m.PaneID)
		if pErr != nil {
			return ReviewRetirementEvidence{}, pErr
		}
		if len(procs) == 0 {
			tabs, tErr := TabList(m.Workspace)
			if tErr != nil {
				return ReviewRetirementEvidence{}, tErr
			}
			present := false
			for _, tab := range tabs {
				if tab.TabID == m.TabID {
					present = true
					break
				}
			}
			live.TabPresent, live.ProcessPresent, live.Status = present, false, "done"
		} else {
			live.ProcessPresent, live.Status = true, "unknown"
		}
	}
	wt, wtErr := n.observeWorktree(m)
	if wtErr != nil {
		if poolGone {
			ok, phaseErr := n.hasPhase(m, "worktree-intent", "worktree-done", "ref-intent", "ref-done", "artifacts-intent", "artifacts-done", "complete")
			if phaseErr != nil {
				return ReviewRetirementEvidence{}, phaseErr
			}
			if ok {
				wt = ReviewRetirementWorktree{Known: true, Head: m.CandidateSHA, Branch: m.Branch}
				wtErr = nil
			}
		}
		if wtErr != nil {
			return ReviewRetirementEvidence{}, wtErr
		}
	}
	return ReviewRetirementEvidence{Manifest: m, Launch: launch, Verdict: ReviewRetirementVerdict{Row: verdict, Ack: ack}, Live: live, Worktree: wt, WorktreeRoot: m.Pool, PromptRoot: filepath.Dir(m.PromptArtifact), Repository: n.RepositoryIdentity}, nil
}

var errRetirementPoolAlreadyRemoved = errors.New("review retirement pool already removed under authenticated phase intent")
var errRetirementSuperseded = errors.New("review retirement manifest is superseded by a later pool incarnation")

func launchBeforeManifest(row reviewledger.LedgerRow, recordedAt string) bool {
	if strings.TrimSpace(row.Timestamp) == "" {
		return true
	}
	launchAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(row.Timestamp))
	if err != nil {
		return false
	}
	manifestAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(recordedAt))
	return err == nil && !launchAt.After(manifestAt)
}

func (n *NativeReviewRetirementOp) reviewerSuperseded(m ReviewRetirementManifest) (bool, error) {
	agents, err := AgentList()
	if err != nil {
		return false, err
	}
	for _, a := range agents {
		if a.Name == m.Reviewer {
			return false, nil
		}
	}
	return true, nil
}

// ReviewRetirementPhasesFile is the canonical location of the native review
// retirement phase journal, relative to a repository root.
const ReviewRetirementPhasesFile = ".herd/review/retirement-phases.jsonl"

// ReviewRetirementPhasesPath is the single authoritative phase-journal path
// for the native review retirement record; every reader (including the
// idle-pool completion probe) must resolve it through this helper so the
// decision exists in exactly one place.
func ReviewRetirementPhasesPath(root string) string {
	return filepath.Join(root, ReviewRetirementPhasesFile)
}

func (n *NativeReviewRetirementOp) phasePath() string {
	if n.JournalPath != "" {
		return n.JournalPath
	}
	return ReviewRetirementPhasesPath(n.Root)
}

func (n *NativeReviewRetirementOp) phaseRecords(m ReviewRetirementManifest) ([]retirementPhaseRecord, error) {
	f, err := os.Open(n.phasePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var records []retirementPhaseRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec retirementPhaseRecord
		if err := json.Unmarshal([]byte(sc.Text()), &rec); err != nil {
			return nil, fmt.Errorf("decode retirement journal: %w", err)
		}
		if rec.Generation == m.Generation && rec.CandidateSHA == m.CandidateSHA && rec.Reviewer == m.Reviewer {
			if rec.BindingDigest != m.BindingDigest || rec.Pool != m.Pool || rec.Slot != m.Slot || rec.Worktree != m.Worktree || rec.Nonce != m.Nonce || rec.LeaseGeneration != m.LeaseGeneration {
				return nil, errors.New("retirement journal identity conflicts with manifest")
			}
		}
		records = append(records, rec)
	}
	return records, sc.Err()
}

func (n *NativeReviewRetirementOp) hasPhase(m ReviewRetirementManifest, phases ...string) (bool, error) {
	records, err := n.phaseRecords(m)
	if err != nil {
		return false, err
	}
	for _, rec := range records {
		for _, phase := range phases {
			if rec.Phase == phase {
				return true, nil
			}
		}
	}
	return false, nil
}

// paneProcessesForRetirement distinguishes the supported closed-pane
// envelope from transport/tool failures. Only pane_not_found is absence;
// every other error remains a hard observation failure.
func paneProcessesForRetirement(paneID string) ([]PaneProcess, error) {
	procs, err := PaneProcessInfo(paneID)
	if errors.Is(err, ErrPaneNotFound) {
		return nil, nil
	}
	return procs, err
}

func (n *NativeReviewRetirementOp) exactPoolSlot(m ReviewRetirementManifest, allowReleased bool) (worktree.PoolSlot, error) {
	poolPath, err := n.boundPath(m.Pool)
	if err != nil {
		abs, absErr := filepath.Abs(filepath.Join(n.Root, filepath.Clean(m.Pool)))
		if absErr == nil {
			if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
				ok, phaseErr := n.hasPhase(m, "worktree-intent", "worktree-done", "ref-intent", "ref-done", "artifacts-intent", "artifacts-done", "complete")
				if phaseErr != nil {
					return worktree.PoolSlot{}, phaseErr
				}
				if ok {
					return worktree.PoolSlot{}, errRetirementPoolAlreadyRemoved
				}
			}
		}
		return worktree.PoolSlot{}, err
	}
	p := worktree.NewPool(n.Root, poolPath, 0)
	slots, err := p.Slots()
	if err != nil {
		return worktree.PoolSlot{}, err
	}
	for _, slot := range slots {
		if slot.Name != m.Slot {
			continue
		}
		slotPath, slotPathErr := n.canonicalPoolRecordPath(slot.Path)
		worktreePath, worktreePathErr := n.boundPath(m.Worktree)
		if slotPathErr != nil || worktreePathErr != nil || !sameRealPath(slotPath, filepath.Join(poolPath, m.Slot)) || !sameRealPath(slotPath, worktreePath) {
			return worktree.PoolSlot{}, errors.New("review pool slot path differs from authenticated manifest")
		}
		if slot.LeaseID == "" && allowReleased {
			releasePath, releasePathErr := n.canonicalPoolRecordPath(slot.LastReleasePath)
			if releasePathErr != nil || slot.LastReleaseLeaseID != m.Nonce || slot.LastReleaseGeneration != m.LeaseGeneration || !sameRealPath(releasePath, slotPath) {
				return worktree.PoolSlot{}, errors.New("review pool slot has no matching authenticated release history")
			}
			slot.Path = slotPath
			return slot, nil
		}
		if slot.LeaseID != m.Nonce || slot.LeasedAt.UnixNano() != m.LeaseGeneration {
			return worktree.PoolSlot{}, errors.New("review pool lease incarnation differs from authenticated manifest")
		}
		slot.Path = slotPath
		return slot, nil
	}
	if allowReleased {
		ok, phaseErr := n.hasPhase(m, "worktree-intent", "worktree-done", "ref-intent", "ref-done", "artifacts-intent", "artifacts-done", "complete")
		if phaseErr != nil {
			return worktree.PoolSlot{}, phaseErr
		}
		if ok {
			return worktree.PoolSlot{}, errRetirementPoolAlreadyRemoved
		}
	}
	return worktree.PoolSlot{}, errors.New("authenticated review pool slot is missing")
}

// canonicalPoolRecordPath resolves the two path formats emitted by pool
// producers (repository-relative and absolute) against the authenticated
// repository root. Relative paths are never interpreted using the caller's
// CWD, and both forms must resolve inside the root before identity comparison.
func (n *NativeReviewRetirementOp) canonicalPoolRecordPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("review pool record path is empty")
	}
	root, err := filepath.Abs(n.Root)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	candidate := filepath.Clean(raw)
	if !filepath.IsAbs(candidate) {
		if candidate == "." || strings.HasPrefix(candidate, ".."+string(filepath.Separator)) {
			return "", errors.New("review pool record path is not repository-relative")
		}
		candidate = filepath.Join(root, candidate)
	}
	if candidate == root || !strings.HasPrefix(candidate, root+string(filepath.Separator)) {
		return "", errors.New("review pool record path escaped repository root")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if realCandidate == realRoot || !strings.HasPrefix(realCandidate, realRoot+string(filepath.Separator)) {
		return "", errors.New("review pool record path real path escaped repository root")
	}
	return candidate, nil
}

func sameRealPath(a, b string) bool {
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	if ea == nil && eb == nil {
		return filepath.Clean(ra) == filepath.Clean(rb)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func boolPtr(v bool) *bool { return &v }
func (n *NativeReviewRetirementOp) observeWorktree(m ReviewRetirementManifest) (ReviewRetirementWorktree, error) {
	path, err := n.boundPath(m.Worktree)
	if err != nil {
		return ReviewRetirementWorktree{}, err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return ReviewRetirementWorktree{Known: true, Head: m.CandidateSHA, Branch: m.Branch}, nil
		}
		return ReviewRetirementWorktree{}, err
	}
	status, err := n.git(path, "status", "--porcelain")
	if err != nil {
		return ReviewRetirementWorktree{}, err
	}
	head, err := n.git(path, "rev-parse", "HEAD")
	if err != nil {
		return ReviewRetirementWorktree{}, err
	}
	branch, _ := n.git(path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if strings.TrimSpace(branch) == "" {
		branch = m.Branch
	}
	return ReviewRetirementWorktree{Known: true, Dirty: strings.TrimSpace(status) != "", Head: strings.TrimSpace(head), Branch: strings.TrimSpace(branch)}, nil
}

func (n *NativeReviewRetirementOp) boundPath(rel string) (string, error) {
	if filepath.IsAbs(rel) || filepath.Clean(rel) == "." || strings.HasPrefix(filepath.Clean(rel), ".."+string(filepath.Separator)) {
		return "", errors.New("review retirement path is not repository-relative")
	}
	root, err := filepath.Abs(n.Root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, rel)
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(p)
	if cleanPath == cleanRoot || !strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator)) {
		return "", errors.New("review retirement path escaped repository root")
	}
	// Lexical containment is not authorization: a symlinked parent can escape
	// the repository while retaining an innocent-looking relative name. Walk
	// every existing component and require realpath containment; missing leaves
	// are safe only when their existing parent has passed the same check.
	cur := cleanRoot
	parts := strings.Split(strings.TrimPrefix(cleanPath, cleanRoot+string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		info, statErr := os.Lstat(cur)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("review retirement path contains symlink component %q", part)
		}
	}
	realRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(cleanPath)
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("review retirement path parent does not exist: %w", err)
		}
		return "", err
	}
	if realParent == realRoot || !strings.HasPrefix(realParent, realRoot+string(filepath.Separator)) {
		return "", errors.New("review retirement path real parent escaped repository root")
	}
	return p, nil
}

// Surface is the one intentionally symlinked artifact: it points from the
// repository review namespace into the leased pool slot. Only that final leaf
// may be a symlink, and its resolved target must remain below the owned pool.
// A dangling leaf resolves only through authorizeDanglingSurface: the exact
// authenticated pool slot target must be proven absent and unregistered. The
// returned authorized raw link target and resolution premise must be
// re-verified at the mutation boundary; between authorization and removal
// the link may have been replaced, its parent swapped, or its target
// recreated.
func (n *NativeReviewRetirementOp) boundSurfacePath(m ReviewRetirementManifest) (string, string, bool, error) {
	rel, poolRel := m.Surface, m.Pool
	if filepath.IsAbs(rel) || filepath.Clean(rel) == "." || strings.HasPrefix(filepath.Clean(rel), ".."+string(filepath.Separator)) {
		return "", "", false, errors.New("review surface is not repository-relative")
	}
	root, err := filepath.Abs(n.Root)
	if err != nil {
		return "", "", false, err
	}
	p := filepath.Join(root, filepath.Clean(rel))
	relPath, err := filepath.Rel(root, p)
	if err != nil {
		return "", "", false, err
	}
	parentRel := filepath.Dir(relPath)
	if _, err := n.boundPath(parentRel); err != nil {
		return "", "", false, err
	}
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return p, "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", "", false, errors.New("review surface is not an owned symlink")
	}
	raw, err := os.Readlink(p)
	if err != nil {
		return "", "", false, err
	}
	target, err := filepath.EvalSymlinks(p)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", "", false, err
		}
		// The surface dangles because its exact pool slot was already
		// retired on disk. Removing that link requires the full
		// dangling-surface ownership proof; every other dangling target
		// stays refused.
		if authErr := n.authorizeDanglingSurface(m, p); authErr != nil {
			return "", "", false, authErr
		}
		return p, raw, true, nil
	}
	poolRoot, err := filepath.EvalSymlinks(filepath.Join(root, filepath.Clean(poolRel)))
	if err != nil {
		return "", "", false, err
	}
	if target == poolRoot || !strings.HasPrefix(target, poolRoot+string(filepath.Separator)) {
		return "", "", false, errors.New("review surface target escaped the owned pool")
	}
	return p, raw, false, nil
}

// reviewSurfaceFenceProbe, when non-nil, runs between surface authorization
// and the final identity recheck so the regression harness can
// deterministically inject the replacement, parent-escape, and recreation
// window the boundary fence closes. Production leaves it nil.
var reviewSurfaceFenceProbe func()

// hasExactIdentityPhase reports whether the phase journal holds a record
// carrying the manifest's complete identity in one of the given phases.
// Unlike hasPhase, a record belonging to a different generation or lease
// incarnation never satisfies it.
func (n *NativeReviewRetirementOp) hasExactIdentityPhase(m ReviewRetirementManifest, phases ...string) (bool, error) {
	records, err := n.phaseRecords(m)
	if err != nil {
		return false, err
	}
	for _, rec := range records {
		if rec.Generation != m.Generation || rec.CandidateSHA != m.CandidateSHA || rec.Reviewer != m.Reviewer ||
			rec.BindingDigest != m.BindingDigest || rec.Pool != m.Pool || rec.Slot != m.Slot ||
			rec.Worktree != m.Worktree || rec.Nonce != m.Nonce || rec.LeaseGeneration != m.LeaseGeneration {
			continue
		}
		for _, phase := range phases {
			if rec.Phase == phase {
				return true, nil
			}
		}
	}
	return false, nil
}

// authorizeDanglingSurface proves a dangling review-surface symlink is the
// exact owned link for this manifest before its removal. A dangling link
// into an arbitrary missing path is not ownership proof: the raw link target
// must be exactly the authenticated pool slot path, that intended worktree
// must be absent, the manifest must hold an identity-matched authenticated
// worktree phase record, and the FAC-807 pool fencing must classify the slot
// as already removed (absent and unregistered under the same
// authentication, including every release-history and incarnation refusal).
// Only the symlink itself is ever removed; target content is never touched.
func (n *NativeReviewRetirementOp) authorizeDanglingSurface(m ReviewRetirementManifest, surfacePath string) error {
	root, err := filepath.Abs(n.Root)
	if err != nil {
		return err
	}
	expected := filepath.Join(filepath.Clean(root), filepath.Clean(m.Pool), m.Slot)
	raw, err := os.Readlink(surfacePath)
	if err != nil {
		return err
	}
	target := raw
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(surfacePath), target)
	}
	if filepath.Clean(target) != expected {
		return errors.New("dangling review surface does not target the authenticated pool slot")
	}
	// The dangling detection itself is the primary absence proof for the
	// exact target; this re-read closes the recreation window between that
	// detection and this authorization boundary. Target content is never
	// touched either way.
	if _, err := os.Lstat(expected); err == nil {
		return errors.New("dangling review surface target exists; absence was not proven")
	} else if !os.IsNotExist(err) {
		return err
	}
	ok, err := n.hasExactIdentityPhase(m, "worktree-intent", "worktree-done", "ref-intent", "ref-done", "artifacts-intent", "artifacts-done", "complete")
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("dangling review surface lacks an identity-matched authenticated worktree phase record")
	}
	if _, err := n.exactPoolSlot(m, true); err == nil {
		return errors.New("dangling review surface pool slot is still registered")
	} else if !errors.Is(err, errRetirementPoolAlreadyRemoved) {
		return fmt.Errorf("dangling review surface pool fencing: %w", err)
	}
	return nil
}

func (n *NativeReviewRetirementOp) git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (n *NativeReviewRetirementOp) Revalidate(m ReviewRetirementManifest, phase string) error {
	agents, err := AgentList()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.Name == m.Reviewer || a.TabID == m.TabID || a.PaneID == m.PaneID {
			if phase != "close" {
				return fmt.Errorf("review incarnation still present before %s", phase)
			}
			if a.Name != m.Reviewer || a.TabID != m.TabID || a.PaneID != m.PaneID || a.Workspace != m.Workspace || a.TerminalID != m.TerminalID || a.Session.Value != m.SessionID {
				return errors.New("review incarnation changed before close")
			}
			if a.Status != "idle" && a.Status != "done" {
				return fmt.Errorf("review status %q is not settled", a.Status)
			}
			if a.Focused == nil || *a.Focused {
				return errors.New("review focus is not explicitly unfocused")
			}
			procs, pErr := paneProcessesForRetirement(a.PaneID)
			if pErr != nil {
				return pErr
			}
			if len(procs) > 0 && phase != "close" {
				return errors.New("review process remains before dependent mutation")
			}
		}
	}
	if phase != "close" {
		procs, pErr := paneProcessesForRetirement(m.PaneID)
		if pErr != nil {
			return fmt.Errorf("process absence readback: %w", pErr)
		}
		if len(procs) != 0 {
			return fmt.Errorf("review process remains before %s", phase)
		}
		tabs, tErr := TabList(m.Workspace)
		if tErr != nil {
			return fmt.Errorf("tab absence readback: %w", tErr)
		}
		for _, tab := range tabs {
			if tab.TabID == m.TabID {
				return fmt.Errorf("review tab remains before %s", phase)
			}
		}
	}
	if phase == "worktree" {
		slot, slotErr := n.exactPoolSlot(m, true)
		if slotErr != nil && !errors.Is(slotErr, errRetirementPoolAlreadyRemoved) {
			return fmt.Errorf("worktree pool readback: %w", slotErr)
		}
		wt, wtErr := n.observeWorktree(m)
		if wtErr != nil {
			if _, statErr := os.Stat(filepath.Join(n.Root, filepath.Clean(m.Worktree))); os.IsNotExist(statErr) {
				ok, phaseErr := n.hasPhase(m, "worktree-intent", "worktree-done")
				if phaseErr != nil {
					return phaseErr
				}
				if ok {
					return nil
				}
			}
			return fmt.Errorf("worktree mutation readback: %w", wtErr)
		}
		expectedHead := m.CandidateSHA
		if slotErr == nil && slot.LeaseID == "" {
			if slot.LastReleaseTargetHead != "" {
				expectedHead = slot.LastReleaseTargetHead
			} else {
				expectedHead = m.BaseSHA
			}
		}
		if !wt.Known || wt.Dirty || wt.Head != expectedHead || wt.Branch != m.Branch {
			return fmt.Errorf("worktree changed before destructive removal: head=%s want=%s dirty=%t branch=%s want-branch=%s", wt.Head, expectedHead, wt.Dirty, wt.Branch, m.Branch)
		}
	}
	return nil
}

func (n *NativeReviewRetirementOp) Journal(m ReviewRetirementManifest, phase string) error {
	p := n.phasePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	rec := retirementPhaseRecord{Generation: m.Generation, CandidateSHA: m.CandidateSHA, Reviewer: m.Reviewer, BindingDigest: m.BindingDigest, Pool: m.Pool, Slot: m.Slot, Worktree: m.Worktree, Nonce: m.Nonce, LeaseGeneration: m.LeaseGeneration, Phase: phase}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (n *NativeReviewRetirementOp) Completed(m ReviewRetirementManifest) (bool, error) {
	records, err := n.phaseRecords(m)
	if err != nil {
		return false, err
	}
	for _, rec := range records {
		if rec.Phase == "complete" && rec.Generation == m.Generation && rec.CandidateSHA == m.CandidateSHA && rec.Reviewer == m.Reviewer && rec.BindingDigest == m.BindingDigest {
			return true, nil
		}
	}
	return false, nil
}

func (n *NativeReviewRetirementOp) Close(m ReviewRetirementManifest) error {
	agents, err := AgentList()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.Name == m.Reviewer && a.TabID == m.TabID && a.PaneID == m.PaneID && a.TerminalID == m.TerminalID {
			return CloseSettledReviewTab(a)
		}
	}
	return nil
}

func (n *NativeReviewRetirementOp) LeaseReleased(m ReviewRetirementManifest) (bool, error) {
	slot, err := n.exactPoolSlot(m, true)
	if errors.Is(err, errRetirementPoolAlreadyRemoved) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return slot.LeaseID == "", nil
}
func (n *NativeReviewRetirementOp) ReleaseLease(ctx context.Context, m ReviewRetirementManifest) error {
	slot, err := n.exactPoolSlot(m, false)
	if err != nil {
		return err
	}
	p := worktree.NewPool(n.Root, filepath.Join(n.Root, filepath.Clean(m.Pool)), 0)
	p.DefaultBase = slot.Base
	return p.ReleaseExact(ctx, m.Slot, m.Nonce, m.LeaseGeneration, slot.Path)
}

func (n *NativeReviewRetirementOp) RemoveWorktree(m ReviewRetirementManifest) error {
	if m.Surface != "" {
		p, authorizedRaw, authorizedDangling, err := n.boundSurfacePath(m)
		if err != nil {
			return err
		}
		if reviewSurfaceFenceProbe != nil {
			reviewSurfaceFenceProbe()
		}
		info, err := os.Lstat(p)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink == 0 {
			return errors.New("review surface is not an owned symlink")
		}
		if err == nil {
			// Final identity fence: between authorization and this removal
			// the link may have been replaced, its parent swapped out of the
			// repository, or its target recreated. Re-verify the parent
			// bound, the exact raw link identity, and the resolution premise
			// that was authorized; any drift refuses without mutation.
			if _, pErr := n.boundPath(filepath.Dir(m.Surface)); pErr != nil {
				return pErr
			}
			rawNow, rawErr := os.Readlink(p)
			if rawErr != nil || rawNow != authorizedRaw {
				return errors.New("review surface identity changed before removal")
			}
			if _, evalErr := filepath.EvalSymlinks(p); authorizedDangling != os.IsNotExist(evalErr) {
				return errors.New("review surface resolution changed before removal")
			}
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	poolPath, err := n.boundPath(m.Pool)
	if err != nil {
		abs, absErr := filepath.Abs(filepath.Join(n.Root, filepath.Clean(m.Pool)))
		if absErr == nil {
			if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
				ok, phaseErr := n.hasPhase(m, "worktree-intent", "worktree-done", "ref-intent", "ref-done", "artifacts-intent", "artifacts-done", "complete")
				if phaseErr != nil {
					return phaseErr
				}
				if ok {
					return nil
				}
			}
		}
		return err
	}
	slot, err := n.exactPoolSlot(m, true)
	if errors.Is(err, errRetirementPoolAlreadyRemoved) {
		return nil
	}
	if err != nil {
		return err
	}
	return worktree.NewPool(n.Root, poolPath, 0).RetireExact(context.Background(), m.Slot, slot.Path, m.Nonce, m.LeaseGeneration)
}
func (n *NativeReviewRetirementOp) RemoveBranch(m ReviewRetirementManifest) error {
	if m.ReviewRef == "" {
		return errors.New("review retirement ref is missing")
	}
	cmd := exec.Command("git", "-C", n.Root, "show-ref", "--verify", "--quiet", m.ReviewRef)
	if out, err := cmd.CombinedOutput(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// A completed retry may already have removed this exact owned ref.
			return nil
		}
		return fmt.Errorf("git show-ref %s: %w (%s)", m.ReviewRef, err, strings.TrimSpace(string(out)))
	}
	_, err := n.git(n.Root, "update-ref", "-d", m.ReviewRef, m.CandidateSHA)
	return err
}
func (n *NativeReviewRetirementOp) RemoveArtifact(m ReviewRetirementManifest) error {
	if m.ManifestArtifact != "" {
		mp, err := n.boundPath(m.ManifestArtifact)
		if err != nil {
			return err
		}
		if body, readErr := os.ReadFile(mp); readErr == nil {
			var recorded ReviewRetirementManifest
			if json.Unmarshal(body, &recorded) != nil || recorded.BindingDigest != m.BindingDigest || recorded.Generation != m.Generation || recorded.CandidateSHA != m.CandidateSHA {
				return errors.New("review manifest artifact content changed; refusing removal")
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
	}
	p, err := n.boundPath(m.PromptArtifact)
	if err != nil {
		return err
	}
	if m.PromptDigest == "" {
		return errors.New("review prompt lacks authenticated content digest")
	}
	if body, readErr := os.ReadFile(p); readErr == nil {
		if reviewack.ArtifactDigest(body) != m.PromptDigest {
			return errors.New("review prompt content changed; refusing removal")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	if m.ManifestArtifact != "" {
		mp, err := n.boundPath(m.ManifestArtifact)
		if err != nil {
			return err
		}
		if body, readErr := os.ReadFile(mp); readErr == nil {
			var recorded ReviewRetirementManifest
			if json.Unmarshal(body, &recorded) != nil || recorded.BindingDigest != m.BindingDigest || recorded.Generation != m.Generation || recorded.CandidateSHA != m.CandidateSHA {
				return errors.New("review manifest artifact content changed; refusing removal")
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		if err := os.Remove(mp); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
func (n *NativeReviewRetirementOp) Receipt(m ReviewRetirementManifest, d ReviewRetirementDecision) error {
	return n.Journal(m, "complete")
}

var _ ReviewRetirementOp = (*NativeReviewRetirementOp)(nil)
