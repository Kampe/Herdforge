package herdr

// FAC-794: native production adapter for source-lane retirement.
//
// Invariants:
// - Source worktree, branch, refs, and recovery receipts are NEVER deleted.
// - Tasks are NOT marked Done (landed code is not inferred).
// - Exact process/foreground identity rechecked immediately before native close.
// - Pane absence and process cleanup are verified by readback.

import (
	"bufio"
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

	"github.com/Kampe/Herdforge/pkg/launch"
)

type NativeSourceRetirementOp struct {
	Root               string
	RepositoryIdentity string
	LaunchReceiptsPath string
	JournalPath        string
	ReceiptsPath       string
	StandingLanes      map[string]bool
}

type sourceRetirementPhaseRecord struct {
	Generation    string `json:"generation"`
	CandidateSHA  string `json:"candidate_sha"`
	AgentName     string `json:"agent_name"`
	BindingDigest string `json:"binding_digest"`
	Worktree      string `json:"worktree"`
	Branch        string `json:"branch"`
	Phase         string `json:"phase"`
	RecordedAt    string `json:"recorded_at"`
}

type SourceRetirementReceiptRecord struct {
	Generation    string                   `json:"generation"`
	CandidateSHA  string                   `json:"candidate_sha"`
	AgentName     string                   `json:"agent_name"`
	BindingDigest string                   `json:"binding_digest"`
	TaskRef       string                   `json:"task_ref"`
	Decision      SourceRetirementDecision `json:"decision"`
	RetiredAt     string                   `json:"retired_at"`
}

func (n *NativeSourceRetirementOp) Observe(m SourceRetirementManifest) (SourceRetirementEvidence, error) {
	if n == nil || strings.TrimSpace(n.Root) == "" || strings.TrimSpace(n.RepositoryIdentity) == "" {
		return SourceRetirementEvidence{}, errors.New("native source retirement authority is incomplete")
	}
	if err := ValidateSourceRetirementManifest(m); err != nil {
		return SourceRetirementEvidence{}, err
	}

	// 1. Launch receipt lookup
	launchReceipt := n.findLaunchReceipt(m)

	// 2. Durable handoff / report verification
	handoff := n.observeHandoff(m)

	// 3. Live agent and pane state
	live, err := n.observeLive(m)
	if err != nil {
		return SourceRetirementEvidence{}, err
	}

	// 4. Source worktree status & HEAD check
	wt, wtErr := n.observeWorktree(m)
	if wtErr != nil {
		return SourceRetirementEvidence{}, wtErr
	}

	return SourceRetirementEvidence{
		Manifest:   m,
		Launch:     launchReceipt,
		Handoff:    handoff,
		Live:       live,
		Worktree:   wt,
		Repository: n.RepositoryIdentity,
	}, nil
}

func (n *NativeSourceRetirementOp) launchReceiptsPath() string {
	if n.LaunchReceiptsPath != "" {
		return n.LaunchReceiptsPath
	}
	return launch.ReceiptPathFor(n.Root)
}

func (n *NativeSourceRetirementOp) phasePath() string {
	if n.JournalPath != "" {
		return n.JournalPath
	}
	return filepath.Join(n.Root, SourceRetirementPhasesFile)
}

func (n *NativeSourceRetirementOp) receiptsPath() string {
	if n.ReceiptsPath != "" {
		return n.ReceiptsPath
	}
	return filepath.Join(n.Root, SourceRetirementReceiptsFile)
}

func (n *NativeSourceRetirementOp) findLaunchReceipt(m SourceRetirementManifest) launch.Receipt {
	path := n.launchReceiptsPath()
	f, err := os.Open(path)
	if err != nil {
		return launch.Receipt{}
	}
	defer f.Close()

	var matching []launch.Receipt
	s := bufio.NewScanner(f)
	for s.Scan() {
		var r launch.Receipt
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			continue
		}
		if r.TaskRef == m.TaskRef && r.Name == m.AgentName && r.Worktree == m.Worktree {
			if r.CandidateSHA != "" && m.CandidateSHA != "" && r.CandidateSHA != m.CandidateSHA {
				continue
			}
			if r.Branch != "" && m.Branch != "" && r.Branch != m.Branch {
				continue
			}
			if r.HerdrSession != "" && m.SessionID != "" && r.HerdrSession != m.SessionID {
				continue
			}
			matching = append(matching, r)
		}
	}
	if len(matching) == 1 {
		return matching[0]
	}
	if len(matching) > 1 {
		// Prefer the latest accepted receipt matching exact candidate and session
		for i := len(matching) - 1; i >= 0; i-- {
			if matching[i].Accepted && matching[i].CandidateSHA == m.CandidateSHA {
				return matching[i]
			}
		}
		return matching[len(matching)-1]
	}
	return launch.Receipt{}
}

func (n *NativeSourceRetirementOp) observeHandoff(m SourceRetirementManifest) SourceRetirementHandoff {
	h := SourceRetirementHandoff{
		CandidateSHA: m.CandidateSHA,
		TaskRef:      m.TaskRef,
		AgentName:    m.AgentName,
		ReportDigest: m.ReportDigest,
		Status:       "UNKNOWN",
	}

	var data []byte
	var pathRel string
	if m.ReportArtifact != "" {
		p := filepath.Join(n.Root, filepath.Clean(m.ReportArtifact))
		if b, err := os.ReadFile(p); err == nil {
			data = b
			pathRel = m.ReportArtifact
		}
	}
	if len(data) == 0 && m.HandoffArtifact != "" {
		p := filepath.Join(n.Root, filepath.Clean(m.HandoffArtifact))
		if b, err := os.ReadFile(p); err == nil {
			data = b
			pathRel = m.HandoffArtifact
		}
	}

	if len(data) > 0 {
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		expectedDigest := m.ReportDigest
		if expectedDigest == "" {
			expectedDigest = m.HandoffDigest
		}
		if expectedDigest == "" || expectedDigest == digest {
			if parsed, err := ParseStructuredHandoffReport(data); err == nil {
				h.Known = true
				h.Status = parsed.Status
				h.CandidateSHA = parsed.CandidateSHA
				h.ReportDigest = digest
				h.ArtifactPath = pathRel
				if parsed.TaskRef != "" {
					h.TaskRef = parsed.TaskRef
				}
				if parsed.AgentName != "" {
					h.AgentName = parsed.AgentName
				}
				return h
			}
		}
	}

	return h
}

func isShellProcessName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "zsh", "bash", "sh", "fish", "-zsh", "-bash", "-sh", "-fish":
		return true
	}
	return false
}

func isIdleHarnessOrShell(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if isShellProcessName(n) {
		return true
	}
	switch n {
	case "claude", "codex", "opencode", "grok", "agy", "herdr-term":
		return true
	}
	return false
}

func collectActiveDescendants(procs []PaneProcess) []string {
	var activeDescendants []string
	for _, p := range procs {
		if !isIdleHarnessOrShell(p.Name) {
			activeDescendants = append(activeDescendants, p.Name)
		} else if isShellProcessName(p.Name) && len(p.Argv) > 1 {
			activeDescendants = append(activeDescendants, p.Name+" "+strings.Join(p.Argv[1:], " "))
		}
	}
	if len(procs) > 1 && len(activeDescendants) == 0 {
		for _, p := range procs[1:] {
			activeDescendants = append(activeDescendants, p.Name)
		}
	}
	return activeDescendants
}

func (n *NativeSourceRetirementOp) observeLive(m SourceRetirementManifest) (SourceRetirementLive, error) {
	agents, err := AgentList()
	if err != nil {
		return SourceRetirementLive{}, err
	}

	isStanding := false
	if n.StandingLanes != nil && n.StandingLanes[m.AgentName] {
		isStanding = true
	}

	for _, a := range agents {
		if a.Name != m.AgentName || a.TabID != m.TabID || a.PaneID != m.PaneID || a.Workspace != m.Workspace || a.TerminalID != m.TerminalID {
			continue
		}
		if m.SessionID != "" && a.Session.Value != "" && a.Session.Value != m.SessionID {
			return SourceRetirementLive{}, errors.New("source session identity differs from the bound launch")
		}

		procs, pErr := paneProcessesForRetirement(a.PaneID)
		if pErr != nil {
			return SourceRetirementLive{}, pErr
		}

		activeDescendants := collectActiveDescendants(procs)

		return SourceRetirementLive{
			Status:            a.Status,
			Focused:           a.Focused,
			ProcessPresent:    len(procs) > 0,
			TabPresent:        true,
			IsStanding:        isStanding,
			IsCoordinator:     isCoordinatorName(a.Name),
			IsCanary:          isCanaryName(a.Name),
			Workspace:         a.Workspace,
			TabID:             a.TabID,
			PaneID:            a.PaneID,
			TerminalID:        a.TerminalID,
			SessionID:         a.Session.Value,
			SessionGeneration: "",
			ActiveDescendants: activeDescendants,
		}, nil
	}

	// Agent not in live roster: check if pane or tab still exists
	focused := false
	procs, pErr := paneProcessesForRetirement(m.PaneID)
	if pErr != nil {
		return SourceRetirementLive{}, pErr
	}

	tabs, tErr := TabList(m.Workspace)
	if tErr != nil {
		return SourceRetirementLive{}, tErr
	}
	tabPresent := false
	for _, tab := range tabs {
		if tab.TabID == m.TabID {
			tabPresent = true
			break
		}
	}

	status := "unknown"
	if len(procs) == 0 && !tabPresent {
		status = "done"
	}

	activeDescendants := collectActiveDescendants(procs)

	return SourceRetirementLive{
		Status:            status,
		Focused:           &focused,
		ProcessPresent:    len(procs) > 0,
		TabPresent:        tabPresent,
		IsStanding:        isStanding,
		IsCoordinator:     isCoordinatorName(m.AgentName),
		IsCanary:          isCanaryName(m.AgentName),
		Workspace:         m.Workspace,
		TabID:             m.TabID,
		PaneID:            m.PaneID,
		TerminalID:        m.TerminalID,
		SessionID:         m.SessionID,
		ActiveDescendants: activeDescendants,
	}, nil
}

func (n *NativeSourceRetirementOp) observeWorktree(m SourceRetirementManifest) (SourceRetirementWorktree, error) {
	dir := filepath.Join(n.Root, filepath.Clean(m.Worktree))
	if _, err := os.Stat(dir); err != nil {
		return SourceRetirementWorktree{}, fmt.Errorf("source worktree path unavailable: %w", err)
	}

	status, err := n.git(dir, "status", "--porcelain")
	if err != nil {
		return SourceRetirementWorktree{}, fmt.Errorf("source worktree git status: %w", err)
	}

	head, err := n.git(dir, "rev-parse", "HEAD")
	if err != nil {
		return SourceRetirementWorktree{}, fmt.Errorf("source worktree git rev-parse HEAD: %w", err)
	}

	branch, err := n.git(dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return SourceRetirementWorktree{}, fmt.Errorf("source worktree git rev-parse branch: %w", err)
	}

	return SourceRetirementWorktree{
		Known:  true,
		Dirty:  strings.TrimSpace(status) != "",
		Head:   strings.TrimSpace(head),
		Branch: strings.TrimSpace(branch),
	}, nil
}

func (n *NativeSourceRetirementOp) git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (n *NativeSourceRetirementOp) Revalidate(m SourceRetirementManifest, phase string) error {
	if n == nil || strings.TrimSpace(n.Root) == "" {
		return errors.New("native source retirement authority is incomplete")
	}
	if err := ValidateSourceRetirementManifest(m); err != nil {
		return err
	}

	live, err := n.observeLive(m)
	if err != nil {
		return err
	}
	if live.IsStanding {
		return errors.New("standing lane is protected from source retirement")
	}
	if live.IsCoordinator {
		return errors.New("coordinator lane is protected from source retirement")
	}
	if live.IsCanary {
		return errors.New("canary lane is protected from source retirement")
	}
	if live.Focused != nil && *live.Focused {
		return errors.New("source tab is focused")
	}
	if live.Status != "idle" && live.Status != "done" {
		return fmt.Errorf("source lane is still active: status %s", live.Status)
	}
	if len(live.ActiveDescendants) > 0 {
		return fmt.Errorf("active processes running in pane: %s", strings.Join(live.ActiveDescendants, ", "))
	}

	wt, wtErr := n.observeWorktree(m)
	if wtErr != nil {
		return wtErr
	}
	if !wt.Known || wt.Dirty || wt.Head != m.CandidateSHA || wt.Branch != m.Branch {
		return fmt.Errorf("worktree changed before retirement: head=%s want=%s dirty=%t branch=%s want-branch=%s", wt.Head, m.CandidateSHA, wt.Dirty, wt.Branch, m.Branch)
	}
	return nil
}

func (n *NativeSourceRetirementOp) Journal(m SourceRetirementManifest, phase string) error {
	p := n.phasePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	rec := sourceRetirementPhaseRecord{
		Generation:    m.Generation,
		CandidateSHA:  m.CandidateSHA,
		AgentName:     m.AgentName,
		BindingDigest: m.BindingDigest,
		Worktree:      m.Worktree,
		Branch:        m.Branch,
		Phase:         phase,
		RecordedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
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

func (n *NativeSourceRetirementOp) Completed(m SourceRetirementManifest) (bool, error) {
	p := n.phasePath()
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		var rec sourceRetirementPhaseRecord
		if err := json.Unmarshal(s.Bytes(), &rec); err != nil {
			return false, fmt.Errorf("corrupt source retirement phase record: %w", err)
		}
		if rec.Phase == "complete" && rec.Generation == m.Generation && rec.CandidateSHA == m.CandidateSHA && rec.AgentName == m.AgentName && rec.BindingDigest == m.BindingDigest {
			return true, nil
		}
	}
	return false, s.Err()
}

func (n *NativeSourceRetirementOp) Close(m SourceRetirementManifest) error {
	agents, err := AgentList()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.Name == m.AgentName && a.TabID == m.TabID && a.PaneID == m.PaneID && a.TerminalID == m.TerminalID && a.Workspace == m.Workspace {
			if m.SessionID != "" && a.Session.Value != "" && a.Session.Value != m.SessionID {
				return errors.New("cannot close: live agent session identity changed")
			}
			return CloseSettledSourceTab(a)
		}
	}
	tabs, err := TabList(m.Workspace)
	if err != nil {
		return err
	}
	for _, tab := range tabs {
		if tab.TabID == m.TabID {
			return fmt.Errorf("tab %s present in workspace %s but does not match original agent identity %s", m.TabID, m.Workspace, m.AgentName)
		}
	}
	return nil
}

func (n *NativeSourceRetirementOp) VerifyAbsence(m SourceRetirementManifest) error {
	tabs, err := TabList(m.Workspace)
	if err != nil {
		return fmt.Errorf("readback tabs in workspace %s: %w", m.Workspace, err)
	}
	for _, tab := range tabs {
		if tab.TabID == m.TabID {
			return fmt.Errorf("tab %s still present after retirement close", m.TabID)
		}
	}
	procs, err := paneProcessesForRetirement(m.PaneID)
	if err != nil {
		return fmt.Errorf("readback processes in pane %s: %w", m.PaneID, err)
	}
	if len(procs) > 0 {
		return fmt.Errorf("pane %s still has %d residual process(es)", m.PaneID, len(procs))
	}
	return nil
}

func (n *NativeSourceRetirementOp) Receipt(m SourceRetirementManifest, d SourceRetirementDecision) error {
	p := n.receiptsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	rec := SourceRetirementReceiptRecord{
		Generation:    m.Generation,
		CandidateSHA:  m.CandidateSHA,
		AgentName:     m.AgentName,
		BindingDigest: m.BindingDigest,
		TaskRef:       m.TaskRef,
		Decision:      d,
		RetiredAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
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

var _ SourceRetirementOp = (*NativeSourceRetirementOp)(nil)
var _ SourceRetirementPhaseReader = (*NativeSourceRetirementOp)(nil)
