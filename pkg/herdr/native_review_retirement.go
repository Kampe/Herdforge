package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

func (n *NativeReviewRetirementOp) Observe(m ReviewRetirementManifest) (ReviewRetirementEvidence, error) {
	if n == nil || n.Ledger == nil || strings.TrimSpace(n.Root) == "" || strings.TrimSpace(n.RepositoryIdentity) == "" {
		return ReviewRetirementEvidence{}, errors.New("native review retirement authority is incomplete")
	}
	if err := ValidateReviewRetirementManifest(m); err != nil {
		return ReviewRetirementEvidence{}, err
	}
	rows, err := n.Ledger.AllRows()
	if err != nil {
		return ReviewRetirementEvidence{}, fmt.Errorf("read review ledger: %w", err)
	}
	var launch, verdict reviewledger.LedgerRow
	for _, row := range rows {
		if row.Event == string(reviewledger.EventRecord) && row.SHA == m.CandidateSHA && row.Reviewer == m.Reviewer && row.Lease == m.Nonce {
			launch = row
		}
		if row.Event == string(reviewledger.EventVerdict) && row.SHA == m.CandidateSHA && row.Reviewer == m.Reviewer {
			verdict = row
		}
	}
	ack, ackErr := reviewack.Read(n.Root, m.CandidateSHA, m.Reviewer)
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
		live = ReviewRetirementLive{Status: a.Status, Focused: a.Focused, TabPresent: true, Workspace: a.Workspace, TabID: a.TabID, PaneID: a.PaneID, TerminalID: a.TerminalID, SessionGeneration: m.SessionGeneration}
		focused = a.Focused
		procs, pErr := PaneProcessInfo(a.PaneID)
		if pErr != nil {
			return ReviewRetirementEvidence{}, pErr
		}
		live.ProcessPresent = len(procs) > 0
		break
	}
	if focused == nil {
		live.Focused = boolPtr(false)
		procs, pErr := PaneProcessInfo(m.PaneID)
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
		return ReviewRetirementEvidence{}, wtErr
	}
	return ReviewRetirementEvidence{Manifest: m, Launch: launch, Verdict: ReviewRetirementVerdict{Row: verdict, Ack: ack}, Live: live, Worktree: wt, WorktreeRoot: m.Pool, PromptRoot: filepath.Dir(m.PromptArtifact), Repository: n.RepositoryIdentity}, nil
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
func (n *NativeReviewRetirementOp) boundSurfacePath(rel, poolRel string) (string, error) {
	if filepath.IsAbs(rel) || filepath.Clean(rel) == "." || strings.HasPrefix(filepath.Clean(rel), ".."+string(filepath.Separator)) {
		return "", errors.New("review surface is not repository-relative")
	}
	root, err := filepath.Abs(n.Root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, filepath.Clean(rel))
	relPath, err := filepath.Rel(root, p)
	if err != nil {
		return "", err
	}
	parentRel := filepath.Dir(relPath)
	if _, err := n.boundPath(parentRel); err != nil {
		return "", err
	}
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", errors.New("review surface is not an owned symlink")
	}
	target, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	poolRoot, err := filepath.EvalSymlinks(filepath.Join(root, filepath.Clean(poolRel)))
	if err != nil {
		return "", err
	}
	if target == poolRoot || !strings.HasPrefix(target, poolRoot+string(filepath.Separator)) {
		return "", errors.New("review surface target escaped the owned pool")
	}
	return p, nil
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
			if a.Name != m.Reviewer || a.TabID != m.TabID || a.PaneID != m.PaneID || a.Workspace != m.Workspace || a.TerminalID != m.TerminalID {
				return errors.New("review incarnation changed before close")
			}
			if a.Status != "idle" && a.Status != "done" {
				return fmt.Errorf("review status %q is not settled", a.Status)
			}
			if a.Focused == nil || *a.Focused {
				return errors.New("review focus is not explicitly unfocused")
			}
			procs, pErr := PaneProcessInfo(a.PaneID)
			if pErr != nil {
				return pErr
			}
			if len(procs) > 0 && phase != "close" {
				return errors.New("review process remains before dependent mutation")
			}
		}
	}
	if phase != "close" {
		procs, pErr := PaneProcessInfo(m.PaneID)
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
	return nil
}

func (n *NativeReviewRetirementOp) Journal(m ReviewRetirementManifest, phase string) error {
	p := n.JournalPath
	if p == "" {
		p = filepath.Join(n.Root, ".herd", "review", "retirement-phases.jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	rec := struct{ Generation, CandidateSHA, Reviewer, Phase string }{m.Generation, m.CandidateSHA, m.Reviewer, phase}
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
	if n.Pool == nil {
		return false, errors.New("review retirement pool authority is missing")
	}
	slots, err := n.Pool.Slots()
	if err != nil {
		return false, err
	}
	for _, s := range slots {
		if s.LeaseID == m.Nonce {
			return false, nil
		}
	}
	return true, nil
}
func (n *NativeReviewRetirementOp) ReleaseLease(ctx context.Context, m ReviewRetirementManifest) error {
	if n.Pool == nil {
		return errors.New("review retirement pool authority is missing")
	}
	return n.Pool.Release(ctx, m.Nonce)
}

func (n *NativeReviewRetirementOp) RemoveWorktree(m ReviewRetirementManifest) error {
	if m.Surface == "" {
		return nil
	}
	p, err := n.boundSurfacePath(m.Surface, m.Pool)
	if err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return errors.New("review surface is not an owned symlink")
	}
	return os.Remove(p)
}
func (n *NativeReviewRetirementOp) RemoveBranch(m ReviewRetirementManifest) error {
	if m.ReviewRef == "" {
		return errors.New("review retirement ref is missing")
	}
	if _, err := n.git(n.Root, "show-ref", "--verify", "--quiet", m.ReviewRef); err != nil {
		// A completed retry may already have removed this exact owned ref.
		return nil
	}
	_, err := n.git(n.Root, "update-ref", "-d", m.ReviewRef, m.CandidateSHA)
	return err
}
func (n *NativeReviewRetirementOp) RemoveArtifact(m ReviewRetirementManifest) error {
	p, err := n.boundPath(m.PromptArtifact)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	if m.ManifestArtifact != "" {
		mp, err := n.boundPath(m.ManifestArtifact)
		if err != nil {
			return err
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
