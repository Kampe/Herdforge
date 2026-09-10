package herdr

// FAC-794: source-lane retirement policy. This package owns the decision,
// validation, journal, and ordering contract for retiring settled native
// source and mender agent terminals after durable exact-candidate READY handoff.
//
// Invariants:
// - Source worktrees, branches, refs, and recovery receipts are NEVER deleted.
// - Tasks are NOT marked Done (landed code is not inferred).
// - Standing lanes, coordinators, canaries, focused, working, and foreign lanes are protected.
// - All lifecycle records use repo-relative paths.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/launch"
)

const (
	SourceRetirementManifestFile = ".herd/source/retirement-manifests.jsonl"
	SourceRetirementPhasesFile   = ".herd/source/retirement-phases.jsonl"
	SourceRetirementReceiptsFile = ".herd/source/retirement-receipts.jsonl"
)

func SourceRetirementRegistryPath(root string) string {
	return filepath.Join(root, SourceRetirementManifestFile)
}

func SourceRetirementPhasesPath(root string) string {
	return filepath.Join(root, SourceRetirementPhasesFile)
}

func SourceRetirementReceiptsPath(root string) string {
	return filepath.Join(root, SourceRetirementReceiptsFile)
}

type SourceRetirementManifest struct {
	Repository        string `json:"repository"`
	TaskRef           string `json:"task_ref"`
	TaskID            string `json:"task_id"`
	CandidateSHA      string `json:"candidate_sha"`
	BaseSHA           string `json:"base_sha"`
	Branch            string `json:"branch"`
	Worktree          string `json:"worktree"`
	Workspace         string `json:"workspace"`
	TabID             string `json:"tab_id"`
	PaneID            string `json:"pane_id"`
	TerminalID        string `json:"terminal_id"`
	SessionID         string `json:"session_id,omitempty"`
	SessionGeneration string `json:"session_generation,omitempty"`
	AgentName         string `json:"agent_name"`
	Role              string `json:"role,omitempty"`
	AgentKind         string `json:"agent_kind,omitempty"`
	Provider          string `json:"provider,omitempty"`
	Model             string `json:"model,omitempty"`
	HandoffArtifact   string `json:"handoff_artifact,omitempty"`
	HandoffDigest     string `json:"handoff_digest,omitempty"`
	ReportArtifact    string `json:"report_artifact,omitempty"`
	ReportDigest      string `json:"report_digest,omitempty"`
	Generation        string `json:"generation"`
	Nonce             string `json:"nonce"`
	BindingDigest     string `json:"binding_digest"`
	RecordedAt        string `json:"recorded_at"`
}

type SourceRetirementHandoff struct {
	Known        bool   `json:"known"`
	Status       string `json:"status"`
	CandidateSHA string `json:"candidate_sha"`
	TaskRef      string `json:"task_ref"`
	AgentName    string `json:"agent_name"`
	ReportDigest string `json:"report_digest"`
	ArtifactPath string `json:"artifact_path,omitempty"`
}

type SourceRetirementEvidence struct {
	Manifest   SourceRetirementManifest
	Launch     launch.Receipt
	Handoff    SourceRetirementHandoff
	Live       SourceRetirementLive
	Worktree   SourceRetirementWorktree
	Repository string
}

type SourceRetirementLive struct {
	Status            string
	Focused           *bool
	ProcessPresent    bool
	TabPresent        bool
	IsStanding        bool
	IsCoordinator     bool
	IsCanary          bool
	Workspace         string
	TabID             string
	PaneID            string
	TerminalID        string
	SessionID         string
	SessionGeneration string
	ActiveDescendants []string
}

type SourceRetirementWorktree struct {
	Known  bool
	Dirty  bool
	Head   string
	Branch string
	Path   string
}

type SourceRetirementDecision struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}

func blockSourceRetirement(reason string) SourceRetirementDecision {
	return SourceRetirementDecision{Reason: "BLOCKED: " + reason}
}

func NewSourceRetirementManifest(now time.Time, m SourceRetirementManifest) SourceRetirementManifest {
	if strings.TrimSpace(m.RecordedAt) == "" {
		m.RecordedAt = now.UTC().Format(time.RFC3339Nano)
	}
	if strings.TrimSpace(m.BindingDigest) == "" {
		m.BindingDigest = SourceRetirementBindingDigest(m)
	}
	return m
}

func SourceRetirementBindingDigest(m SourceRetirementManifest) string {
	m.BindingDigest, m.RecordedAt = "", ""
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ValidateSourceRetirementManifest(m SourceRetirementManifest) error {
	if !exactSHA(m.CandidateSHA) || !exactSHA(m.BaseSHA) {
		return errors.New("source retirement manifest requires full-length candidate and base SHAs")
	}
	fields := []struct{ name, value string }{
		{"repository", m.Repository}, {"task_ref", m.TaskRef}, {"task_id", m.TaskID}, {"branch", m.Branch},
		{"worktree", m.Worktree}, {"workspace", m.Workspace}, {"tab_id", m.TabID}, {"pane_id", m.PaneID},
		{"terminal_id", m.TerminalID}, {"session_id", m.SessionID}, {"agent_name", m.AgentName},
		{"generation", m.Generation}, {"nonce", m.Nonce}, {"recorded_at", m.RecordedAt},
	}
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("source retirement manifest missing %s", f.name)
		}
	}
	if strings.TrimSpace(m.ReportDigest) == "" && strings.TrimSpace(m.HandoffDigest) == "" {
		return errors.New("source retirement manifest missing report or handoff digest")
	}
	if m.BindingDigest != SourceRetirementBindingDigest(m) {
		return errors.New("source retirement manifest binding digest is invalid")
	}
	if filepath.IsAbs(m.Worktree) || strings.Contains(filepath.Clean(m.Worktree), ".."+string(filepath.Separator)) {
		return errors.New("source retirement manifest worktree must be repository-relative")
	}
	if filepath.Clean(m.Worktree) == "." {
		return errors.New("source retirement manifest worktree cannot be repository root")
	}
	if m.HandoffArtifact != "" && (filepath.IsAbs(m.HandoffArtifact) || filepath.Clean(m.HandoffArtifact) == "." || strings.HasPrefix(filepath.Clean(m.HandoffArtifact), ".."+string(filepath.Separator))) {
		return errors.New("source retirement manifest handoff artifact must be repository-relative")
	}
	if m.ReportArtifact != "" && (filepath.IsAbs(m.ReportArtifact) || filepath.Clean(m.ReportArtifact) == "." || strings.HasPrefix(filepath.Clean(m.ReportArtifact), ".."+string(filepath.Separator))) {
		return errors.New("source retirement manifest report artifact must be repository-relative")
	}
	return nil
}

func isAncestor(dir, ancestor, descendant string) bool {
	ancestor = strings.TrimSpace(ancestor)
	descendant = strings.TrimSpace(descendant)
	dir = strings.TrimSpace(dir)
	if ancestor == "" || descendant == "" {
		return false
	}
	if ancestor == descendant {
		return true
	}
	if dir == "" {
		return false
	}
	ok, err := gitroot.IsAncestorContext(context.Background(), dir, ancestor, descendant)
	return err == nil && ok
}

func isSourceRole(role string) bool {
	r := strings.ToLower(strings.TrimSpace(role))
	switch r {
	case "worker", "forge-smith", "recovery", "mender":
		return true
	}
	return false
}

// EvaluateSourceRetirement evaluates whether a settled source/mender lane
// is eligible for automatic terminal retirement.
func EvaluateSourceRetirement(e SourceRetirementEvidence) SourceRetirementDecision {
	m := e.Manifest
	if err := ValidateSourceRetirementManifest(m); err != nil {
		return blockSourceRetirement("manifest validation failed: " + err.Error())
	}
	if e.Repository == "" || filepath.Clean(e.Repository) != filepath.Clean(m.Repository) {
		return blockSourceRetirement("repository identity differs from the bound source manifest")
	}
	if e.Live.IsStanding {
		return blockSourceRetirement("standing source lane is protected from source retirement")
	}
	if e.Live.IsCoordinator || isCoordinatorName(m.AgentName) {
		return blockSourceRetirement("coordinator lane is protected from source retirement policy")
	}
	if e.Live.IsCanary || isCanaryName(m.AgentName) {
		return blockSourceRetirement("canary lane is protected from source retirement policy")
	}

	// Launch provenance check - must be authentic, accepted, and match the manifest
	if !e.Launch.Accepted || strings.TrimSpace(e.Launch.TaskRef) == "" || strings.TrimSpace(e.Launch.Name) == "" {
		return blockSourceRetirement("launch provenance was not accepted or is missing")
	}
	if e.Launch.TaskRef != m.TaskRef {
		wtDir := e.Worktree.Path
		if wtDir == "" {
			wtDir = m.Worktree
		}
		tcMatched := false
		repoRoot := filepath.Dir(wtDir)
		if rootCandidate, _, err := gitroot.ProjectRoot(context.Background(), wtDir); err == nil && rootCandidate != "" {
			repoRoot = rootCandidate
		}
		if tc, err := readVerifiedSourceTaskContext(repoRoot, wtDir); err == nil {
			if strings.EqualFold(tc.TaskRef, m.TaskRef) {
				tcMatched = true
			}
		}
		if !tcMatched {
			return blockSourceRetirement("launch provenance task ref mismatch")
		}
	}
	if e.Launch.Name != m.AgentName {
		return blockSourceRetirement("launch provenance agent name mismatch")
	}
	if strings.TrimSpace(e.Launch.Worktree) == "" || e.Launch.Worktree != m.Worktree {
		// Also allow launch receipt where CWD matches the worktree
		cwdMatched := false
		if strings.TrimSpace(e.Launch.CWD) != "" {
			cwdClean := filepath.Clean(e.Launch.CWD)
			if filepath.IsAbs(cwdClean) {
				wtAbs := e.Worktree.Path
				if wtAbs == "" {
					wtAbs = m.Worktree
				}
				if filepath.Clean(wtAbs) == cwdClean {
					cwdMatched = true
				} else if rel, err := filepath.Rel(filepath.Dir(cwdClean), cwdClean); err == nil && rel == filepath.Base(m.Worktree) {
					// relative match
				}
			}
			if !cwdMatched && e.Worktree.Path != "" {
				if filepath.Clean(e.Launch.CWD) == filepath.Clean(e.Worktree.Path) {
					cwdMatched = true
				}
			}
			if !cwdMatched {
				// Try resolving relative to repo dir if known
				repoDir := filepath.Dir(e.Worktree.Path)
				if rel, err := filepath.Rel(repoDir, cwdClean); err == nil && !strings.HasPrefix(rel, "..") && rel == m.Worktree {
					cwdMatched = true
				}
			}
		}
		if !cwdMatched {
			return blockSourceRetirement("launch provenance worktree mismatch")
		}
	}
	if strings.TrimSpace(e.Launch.Branch) == "" || e.Launch.Branch != m.Branch {
		return blockSourceRetirement("launch provenance branch mismatch")
	}
	if strings.TrimSpace(e.Launch.HerdrSession) == "" || e.Launch.HerdrSession != m.SessionID {
		wtDir := e.Worktree.Path
		if wtDir == "" {
			wtDir = m.Worktree
		}
		tcMatched := false
		repoRoot := filepath.Dir(wtDir)
		if rootCandidate, _, err := gitroot.ProjectRoot(context.Background(), wtDir); err == nil && rootCandidate != "" {
			repoRoot = rootCandidate
		}
		if tc, err := readVerifiedSourceTaskContext(repoRoot, wtDir); err == nil {
			if tc.SessionID != "" && tc.SessionID == m.SessionID {
				tcMatched = true
			}
		}
		if !tcMatched {
			return blockSourceRetirement("launch provenance session mismatch")
		}
	}
	if !isSourceRole(e.Launch.Role) {
		return blockSourceRetirement("launch provenance role is not an authorized source role: " + e.Launch.Role)
	}
	if m.Role != "" && !strings.EqualFold(e.Launch.Role, m.Role) {
		return blockSourceRetirement("launch provenance role mismatch")
	}
	if e.Launch.Repository != "" && !strings.EqualFold(e.Launch.Repository, m.Repository) {
		return blockSourceRetirement("launch provenance repository mismatch")
	}
	wtDir := e.Worktree.Path
	if wtDir == "" {
		wtDir = m.Worktree
	}
	if e.Launch.CandidateSHA != "" {
		if e.Launch.CandidateSHA == m.CandidateSHA {
			// Non-empty launch pin matching final candidate
		} else if e.Launch.CandidateSHA == m.BaseSHA {
			// Non-empty launch pin matching base pin; authenticate that start pin reaches final candidate
			if !isAncestor(wtDir, e.Launch.CandidateSHA, m.CandidateSHA) {
				return blockSourceRetirement("launch provenance candidate SHA is not an ancestor of manifest candidate")
			}
		} else {
			return blockSourceRetirement("launch provenance candidate SHA mismatch")
		}
	}
	if m.BaseSHA != m.CandidateSHA {
		if !isAncestor(wtDir, m.BaseSHA, m.CandidateSHA) {
			return blockSourceRetirement("manifest base SHA is not an ancestor of manifest candidate")
		}
	}
	if e.Launch.TabID != "" && e.Launch.TabID != m.TabID {
		return blockSourceRetirement("launch provenance tab ID mismatch")
	}
	if e.Launch.PaneID != "" && e.Launch.PaneID != m.PaneID {
		return blockSourceRetirement("launch provenance pane ID mismatch")
	}

	// Durable handoff / report check
	if !e.Handoff.Known {
		return blockSourceRetirement("durable handoff is missing or unverified")
	}
	if e.Handoff.CandidateSHA != m.CandidateSHA {
		return blockSourceRetirement("handoff candidate SHA does not match manifest")
	}
	if strings.TrimSpace(e.Handoff.TaskRef) == "" || !strings.EqualFold(e.Handoff.TaskRef, m.TaskRef) {
		return blockSourceRetirement("handoff task ref does not match manifest")
	}
	if strings.TrimSpace(e.Handoff.AgentName) == "" || e.Handoff.AgentName != m.AgentName {
		return blockSourceRetirement("handoff agent name does not match manifest")
	}
	status := strings.ToUpper(strings.TrimSpace(e.Handoff.Status))
	if status != "READY" && status != "COMPLETE" && status != "COMPLETED" && status != "DONE" && status != "PASS" {
		return blockSourceRetirement("handoff status is not ready or complete: " + e.Handoff.Status)
	}
	digest := m.ReportDigest
	if digest == "" {
		digest = m.HandoffDigest
	}
	if e.Handoff.ReportDigest != "" && digest != "" && e.Handoff.ReportDigest != digest {
		return blockSourceRetirement("handoff report digest does not match manifest")
	}

	// Worktree cleanliness and HEAD check
	if !e.Worktree.Known {
		return blockSourceRetirement("source worktree state is unknown")
	}
	if e.Worktree.Dirty {
		return blockSourceRetirement("source worktree is dirty")
	}
	if strings.TrimSpace(e.Worktree.Head) != m.CandidateSHA {
		return blockSourceRetirement("source worktree HEAD drifted from manifest candidate SHA")
	}
	if strings.TrimSpace(e.Worktree.Branch) != m.Branch {
		return blockSourceRetirement("source worktree branch drifted from manifest")
	}

	// Live lane & terminal checks
	liveStatus := strings.ToLower(strings.TrimSpace(e.Live.Status))
	if liveStatus != "idle" && liveStatus != "done" {
		return blockSourceRetirement("source lane is still active: status " + e.Live.Status)
	}
	if e.Live.Focused == nil {
		return blockSourceRetirement("source tab focus is unknown")
	}
	if *e.Live.Focused {
		return blockSourceRetirement("source tab is focused")
	}
	if strings.TrimSpace(m.SessionID) == "" {
		return blockSourceRetirement("legacy launch lacks authenticated session identity")
	}
	if e.Live.SessionID != "" && e.Live.SessionID != m.SessionID && liveStatus != "done" {
		return blockSourceRetirement("live session identity differs from the bound source launch")
	}
	if e.Live.TabID != "" && e.Live.TabID != m.TabID {
		return blockSourceRetirement("live tab ID differs from manifest")
	}
	if e.Live.PaneID != "" && e.Live.PaneID != m.PaneID {
		return blockSourceRetirement("live pane ID differs from manifest")
	}
	if e.Live.TerminalID != "" && e.Live.TerminalID != m.TerminalID {
		return blockSourceRetirement("live terminal ID differs from manifest")
	}
	if e.Live.Workspace != "" && e.Live.Workspace != m.Workspace {
		return blockSourceRetirement("live workspace differs from manifest")
	}
	if len(e.Live.ActiveDescendants) > 0 {
		return blockSourceRetirement("active processes running in pane: " + strings.Join(e.Live.ActiveDescendants, ", "))
	}
	if e.Live.Status == "done" && e.Live.ProcessPresent {
		return blockSourceRetirement("residual processes present in completed pane")
	}

	return SourceRetirementDecision{Eligible: true, Reason: "exact bound source manifest and durable ready handoff verified"}
}

func isCoordinatorName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(n, "orchestrator") || strings.Contains(n, "coordinator")
}

func isCanaryName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(n, "canary")
}

type SourceRetirementRegistry struct{ Path string }

func (r SourceRetirementRegistry) Record(m SourceRetirementManifest) error {
	if err := ValidateSourceRetirementManifest(m); err != nil {
		return err
	}
	if strings.TrimSpace(r.Path) == "" {
		return errors.New("source retirement registry path is required")
	}
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (r SourceRetirementRegistry) Latest() ([]SourceRetirementManifest, error) {
	f, err := os.Open(r.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []SourceRetirementManifest
	s := bufio.NewScanner(f)
	for s.Scan() {
		var m SourceRetirementManifest
		if err := json.Unmarshal(s.Bytes(), &m); err != nil {
			return nil, fmt.Errorf("decode source retirement registry: %w", err)
		}
		out = append(out, m)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type SourceRetirementOp interface {
	Observe(SourceRetirementManifest) (SourceRetirementEvidence, error)
	Revalidate(SourceRetirementManifest, string) error
	Journal(SourceRetirementManifest, string) error
	Close(SourceRetirementManifest) error
	VerifyAbsence(SourceRetirementManifest) error
	Receipt(SourceRetirementManifest, SourceRetirementDecision) error
}

type SourceRetirementPhaseReader interface {
	Completed(SourceRetirementManifest) (bool, error)
}

type SourceRetirementCandidate struct {
	Manifest  SourceRetirementManifest `json:"manifest"`
	Decision  SourceRetirementDecision `json:"decision"`
	Completed bool                     `json:"completed,omitempty"`
}

type SourceRetirementReport struct {
	DryRun     bool                        `json:"dry_run"`
	Candidates []SourceRetirementCandidate `json:"candidates"`
	Retired    int                         `json:"retired"`
	Blocked    int                         `json:"blocked"`
	Failed     int                         `json:"failed"`
}

func RetireSourceLanes(op SourceRetirementOp, manifests []SourceRetirementManifest, dryRun bool) (SourceRetirementReport, error) {
	return RetireSourceLanesContext(context.Background(), op, manifests, dryRun)
}

func RetireSourceLanesContext(ctx context.Context, op SourceRetirementOp, manifests []SourceRetirementManifest, dryRun bool) (SourceRetirementReport, error) {
	if op == nil {
		return SourceRetirementReport{}, errors.New("source retirement operation authority is required")
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Generation < manifests[j].Generation })
	r := SourceRetirementReport{DryRun: dryRun}
	for _, m := range manifests {
		if err := ValidateSourceRetirementManifest(m); err != nil {
			r.Failed++
			r.Candidates = append(r.Candidates, SourceRetirementCandidate{Manifest: m, Decision: blockSourceRetirement("manifest validation failed: " + err.Error())})
			continue
		}
		if reader, ok := op.(SourceRetirementPhaseReader); ok {
			complete, err := reader.Completed(m)
			if err != nil {
				r.Failed++
				r.Candidates = append(r.Candidates, SourceRetirementCandidate{Manifest: m, Decision: blockSourceRetirement("read source retirement phase: " + err.Error())})
				continue
			}
			if complete {
				r.Candidates = append(r.Candidates, SourceRetirementCandidate{Manifest: m, Completed: true, Decision: SourceRetirementDecision{Eligible: true, Reason: "source lane already completed for exact manifest identity"}})
				continue
			}
		}
		e, err := op.Observe(m)
		if err != nil {
			r.Failed++
			r.Candidates = append(r.Candidates, SourceRetirementCandidate{Manifest: m, Decision: blockSourceRetirement("observation failed: " + err.Error())})
			continue
		}
		d := EvaluateSourceRetirement(e)
		r.Candidates = append(r.Candidates, SourceRetirementCandidate{Manifest: m, Decision: d})
		if !d.Eligible {
			r.Blocked++
		}
	}
	if dryRun {
		if r.Failed > 0 {
			return r, fmt.Errorf("source retirement: %d observation failures", r.Failed)
		}
		return r, nil
	}

	var opErrs []string
	for _, c := range r.Candidates {
		if c.Completed || !c.Decision.Eligible {
			continue
		}
		m := c.Manifest
		if err := op.Revalidate(m, "close"); err != nil {
			r.Failed++
			opErrs = append(opErrs, fmt.Sprintf("revalidate before close %s: %v", m.TabID, err))
			continue
		}
		if err := op.Journal(m, "close-intent"); err != nil {
			r.Failed++
			opErrs = append(opErrs, fmt.Sprintf("journal close phase %s: %v", m.Generation, err))
			continue
		}
		if err := op.Close(m); err != nil {
			r.Failed++
			opErrs = append(opErrs, fmt.Sprintf("close %s: %v", m.TabID, err))
			continue
		}
		if err := op.VerifyAbsence(m); err != nil {
			r.Failed++
			opErrs = append(opErrs, fmt.Sprintf("verify absence %s: %v", m.TabID, err))
			continue
		}
		if err := op.Journal(m, "close-done"); err != nil {
			r.Failed++
			opErrs = append(opErrs, fmt.Sprintf("journal close completion %s: %v", m.Generation, err))
			continue
		}
		if err := op.Receipt(m, c.Decision); err != nil {
			r.Failed++
			opErrs = append(opErrs, fmt.Sprintf("write retirement receipt %s: %v", m.Generation, err))
			continue
		}
		if err := op.Journal(m, "complete"); err != nil {
			r.Failed++
			opErrs = append(opErrs, fmt.Sprintf("journal complete phase %s: %v", m.Generation, err))
			continue
		}
		r.Retired++
	}
	if len(opErrs) > 0 || r.Failed > 0 {
		if len(opErrs) > 0 {
			return r, errors.New(strings.Join(opErrs, "; "))
		}
		return r, fmt.Errorf("source retirement: %d failure(s)", r.Failed)
	}
	return r, nil
}

// StructuredHandoffReport represents the parsed structured fields from a durable handoff report.
type StructuredHandoffReport struct {
	TaskRef      string `json:"task_ref"`
	CandidateSHA string `json:"candidate_sha"`
	Status       string `json:"status"`
	AgentName    string `json:"agent_name,omitempty"`
	Branch       string `json:"branch,omitempty"`
}

// ParseStructuredHandoffReport parses and strictly validates structured key-value lines from a report.
func ParseStructuredHandoffReport(data []byte) (StructuredHandoffReport, error) {
	var r StructuredHandoffReport
	s := bufio.NewScanner(bytes.NewReader(data))
	inCodeBlock := false
	seen := make(map[string]string)

	for s.Scan() {
		rawLine := s.Text()
		trimmed := strings.TrimSpace(rawLine)
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock || trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, ">") {
			continue
		}
		line := trimmed
		if strings.HasPrefix(line, "#") {
			headTrim := strings.TrimLeft(line, "# ")
			if strings.Contains(headTrim, ":") {
				line = headTrim
			} else {
				continue
			}
		}
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		line = strings.TrimSpace(line)
		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:colonIdx]))
		key = strings.Trim(key, "`*_- ")
		val := strings.TrimSpace(line[colonIdx+1:])
		val = strings.Trim(val, "`\"'")

		var canonicalKey string
		switch key {
		case "task", "task_ref", "task-ref", "task_id":
			canonicalKey = "task"
		case "candidate", "candidate_sha", "sha", "commit", "candidate-sha":
			canonicalKey = "candidate"
		case "status", "verdict", "result":
			canonicalKey = "status"
		case "agent", "agent_name", "agent-name", "builder", "mender", "reviewer":
			canonicalKey = "agent"
		case "branch":
			canonicalKey = "branch"
		default:
			continue
		}

		if prevVal, ok := seen[canonicalKey]; ok {
			if !strings.EqualFold(prevVal, val) {
				return StructuredHandoffReport{}, fmt.Errorf("conflicting duplicate %s field in handoff report: %q vs %q", canonicalKey, prevVal, val)
			}
		} else {
			seen[canonicalKey] = val
		}

		switch canonicalKey {
		case "task":
			if r.TaskRef == "" {
				r.TaskRef = val
			}
		case "candidate":
			if r.CandidateSHA == "" {
				r.CandidateSHA = val
			}
		case "status":
			if r.Status == "" {
				r.Status = strings.ToUpper(val)
			}
		case "agent":
			if r.AgentName == "" {
				r.AgentName = val
			}
		case "branch":
			if r.Branch == "" {
				r.Branch = val
			}
		}
	}
	if err := s.Err(); err != nil {
		return StructuredHandoffReport{}, fmt.Errorf("scan handoff report: %w", err)
	}

	if strings.TrimSpace(r.TaskRef) == "" {
		return StructuredHandoffReport{}, errors.New("report lacks structured task field")
	}
	if strings.TrimSpace(r.AgentName) == "" {
		return StructuredHandoffReport{}, errors.New("report lacks structured agent field")
	}
	if strings.TrimSpace(r.Status) == "" {
		return StructuredHandoffReport{}, errors.New("report lacks structured status/verdict field")
	}
	switch r.Status {
	case "READY", "COMPLETE", "COMPLETED", "DONE", "PASS":
		// Accepted ready status
	case "NOT READY", "NOT_READY", "INCOMPLETE", "FAIL", "FAILED", "BLOCKED", "WORKING", "STARTING", "REJECTED":
		return StructuredHandoffReport{}, fmt.Errorf("report status %q is not ready", r.Status)
	default:
		return StructuredHandoffReport{}, fmt.Errorf("unrecognized report status %q", r.Status)
	}

	if !exactSHA(r.CandidateSHA) {
		return StructuredHandoffReport{}, fmt.Errorf("report candidate SHA %q is missing or invalid", r.CandidateSHA)
	}

	return r, nil
}

type signedSourceTaskContext struct {
	ProviderType      string    `json:"provider_type"`
	ProjectID         string    `json:"project_id"`
	ProviderWorkspace string    `json:"provider_workspace,omitempty"`
	ProviderProfile   string    `json:"provider_profile,omitempty"`
	Repository        string    `json:"repository"`
	Role              string    `json:"role"`
	TaskRef           string    `json:"task_ref"`
	TaskID            string    `json:"task_id"`
	Branch            string    `json:"branch"`
	BaseSHA           string    `json:"base_sha"`
	CandidateSHA      string    `json:"candidate_sha,omitempty"`
	AuthorityScope    string    `json:"authority_scope,omitempty"`
	AnchorRef         string    `json:"anchor_ref,omitempty"`
	HerdrWorkspace    string    `json:"herdr_workspace,omitempty"`
	LeaseID           string    `json:"lease_id"`
	LeaseGeneration   int64     `json:"lease_generation"`
	LeaseTaskRef      string    `json:"lease_task_ref"`
	SessionID         string    `json:"session_id"`
	AgentSessionID    string    `json:"agent_session_id,omitempty"`
	AllowedOps        []string  `json:"allowed_ops"`
	ExpiresAt         time.Time `json:"expires_at"`
	Signature         string    `json:"signature,omitempty"`
}

// readVerifiedSourceTaskContext reads and cryptographically verifies TASK-CONTEXT.json
// against the repository's published receipt key (.herd/receipt.pub). If the receipt is
// unsigned, tampered, or the key is missing/corrupt, it fails closed with an error.
func readVerifiedSourceTaskContext(repoRoot, worktreeAbs string) (signedSourceTaskContext, error) {
	var tc signedSourceTaskContext
	data, err := os.ReadFile(filepath.Join(worktreeAbs, "TASK-CONTEXT.json"))
	if err != nil {
		return tc, fmt.Errorf("read TASK-CONTEXT.json: %w", err)
	}
	if err := json.Unmarshal(data, &tc); err != nil {
		return tc, fmt.Errorf("unmarshal TASK-CONTEXT.json: %w", err)
	}
	if strings.TrimSpace(tc.Signature) == "" {
		return tc, errors.New("TASK-CONTEXT.json is unsigned (FAC-145: missing authority signature)")
	}

	pubData, err := os.ReadFile(filepath.Join(repoRoot, ".herd", "receipt.pub"))
	if err != nil {
		return tc, fmt.Errorf("no receipt verification key at %s (FAC-145): %w", filepath.Join(repoRoot, ".herd", "receipt.pub"), err)
	}
	rawPub, err := hex.DecodeString(strings.TrimSpace(string(pubData)))
	if err != nil || len(rawPub) != ed25519.PublicKeySize {
		return tc, errors.New("receipt verification key is corrupt")
	}

	sigBytes, err := hex.DecodeString(strings.TrimSpace(tc.Signature))
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return tc, errors.New("TASK-CONTEXT.json carries malformed signature")
	}

	tcCopy := tc
	tcCopy.Signature = ""
	canonical, err := json.Marshal(tcCopy)
	if err != nil {
		return tc, fmt.Errorf("canonicalize TASK-CONTEXT: %w", err)
	}

	if !ed25519.Verify(ed25519.PublicKey(rawPub), canonical, sigBytes) {
		return tc, errors.New("TASK-CONTEXT.json failed cryptographic signature verification")
	}

	return tc, nil
}

// EnrollReadySourceManifests discovers accepted source/mender launch receipts that have
// produced authentic durable ready reports, creates their exact SourceRetirementManifest,
// and records them to the registry if persist is true.
func EnrollReadySourceManifests(root string, repositoryIdentity string, persist bool) ([]SourceRetirementManifest, error) {
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	registry := SourceRetirementRegistry{Path: SourceRetirementRegistryPath(root)}
	existing, err := registry.Latest()
	if err != nil {
		return nil, fmt.Errorf("read source retirement registry for enrollment: %w", err)
	}

	receiptsPath := launch.ReceiptPathFor(root)
	receipts, err := launch.ReadReceipts(receiptsPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read launch receipts for source enrollment: %w", err)
	}
	if len(receipts) == 0 {
		return nil, nil
	}
	enrolledKeys := make(map[string]bool, len(existing))
	for _, m := range existing {
		enrolledKeys[m.Generation] = true
		enrolledKeys[m.TaskRef+":"+m.CandidateSHA] = true
		if m.TabID != "" {
			enrolledKeys[m.TabID] = true
		}
	}

	var newlyEnrolled []SourceRetirementManifest
	for _, r := range receipts {
		if !r.Accepted || !isSourceRole(r.Role) {
			continue
		}
		if repositoryIdentity != "" && r.Repository != "" && !strings.EqualFold(r.Repository, repositoryIdentity) {
			continue
		}
		if strings.TrimSpace(r.Name) == "" {
			continue
		}

		// Resolve worktree: check r.Worktree first, then r.CWD
		worktreeRel := strings.TrimSpace(r.Worktree)
		if worktreeRel == "" && strings.TrimSpace(r.CWD) != "" {
			cwdClean := filepath.Clean(r.CWD)
			rootClean := filepath.Clean(root)
			if cwdClean == rootClean {
				// CWD is root, not an isolated worktree
			} else if rel, err := filepath.Rel(rootClean, cwdClean); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
				worktreeRel = rel
			}
		}

		// Verify worktree exists on disk
		if worktreeRel == "" {
			continue
		}
		wtAbs := worktreeRel
		if !filepath.IsAbs(wtAbs) {
			wtAbs = filepath.Join(root, wtAbs)
		}
		if fi, err := os.Stat(wtAbs); err != nil || !fi.IsDir() {
			continue
		}

		// Determine authentic task ref:
		// 1. From cryptographically verified TASK-CONTEXT.json in the worktree if present
		// 2. From launch receipt's TaskRef
		taskRef := strings.TrimSpace(r.TaskRef)
		taskID := "task-" + taskRef
		var tcSessionID string
		var tcRole string
		var tcBranch string
		var tcBaseSHA string
		if tc, err := readVerifiedSourceTaskContext(root, wtAbs); err == nil {
			if strings.TrimSpace(tc.TaskRef) != "" {
				taskRef = strings.TrimSpace(tc.TaskRef)
				taskID = tc.TaskID
				if taskID == "" {
					taskID = "task-" + taskRef
				}
			}
			tcSessionID = strings.TrimSpace(tc.SessionID)
			tcRole = strings.TrimSpace(tc.Role)
			tcBranch = strings.TrimSpace(tc.Branch)
			tcBaseSHA = strings.TrimSpace(tc.BaseSHA)
		}
		if taskRef == "" {
			continue
		}

		role := r.Role
		if role == "" {
			role = tcRole
		}

		// Find durable handoff report
		var reportPathRel string
		var reportData []byte
		var parsedReport StructuredHandoffReport
		candidates := []string{
			filepath.Join(".herd", "reports", taskRef+".md"),
			filepath.Join(".herd", "reports", strings.ToLower(taskRef)+".md"),
			filepath.Join(worktreeRel, "REPORT"),
			filepath.Join(worktreeRel, "REPORT.md"),
		}
		if r.TaskRef != "" && !strings.EqualFold(r.TaskRef, taskRef) {
			candidates = append(candidates,
				filepath.Join(".herd", "reports", r.TaskRef+".md"),
				filepath.Join(".herd", "reports", strings.ToLower(r.TaskRef)+".md"),
			)
		}
		for _, candRel := range candidates {
			candAbs := candRel
			if !filepath.IsAbs(candAbs) {
				candAbs = filepath.Join(root, candRel)
			}
			if b, err := os.ReadFile(candAbs); err == nil {
				if parsed, pErr := ParseStructuredHandoffReport(b); pErr == nil {
					if !strings.EqualFold(parsed.TaskRef, taskRef) && (r.TaskRef == "" || !strings.EqualFold(parsed.TaskRef, r.TaskRef)) {
						continue
					}
					if parsed.AgentName != "" && parsed.AgentName != r.Name {
						continue
					}
					if r.CandidateSHA != "" && parsed.CandidateSHA != r.CandidateSHA {
						if !isAncestor(wtAbs, r.CandidateSHA, parsed.CandidateSHA) {
							continue
						}
					}
					if parsed.Branch != "" && r.Branch != "" && !strings.EqualFold(parsed.Branch, r.Branch) {
						continue
					}
					reportPathRel = candRel
					reportData = b
					parsedReport = parsed
					break
				}
			}
		}
		if len(reportData) == 0 {
			continue
		}

		sum := sha256.Sum256(reportData)
		reportDigest := hex.EncodeToString(sum[:])

		candidateSHA := parsedReport.CandidateSHA
		if len(candidateSHA) != 40 {
			continue
		}

		baseSHA := candidateSHA
		if r.CandidateSHA != "" {
			if r.CandidateSHA == candidateSHA {
				baseSHA = candidateSHA
			} else {
				if !isAncestor(wtAbs, r.CandidateSHA, candidateSHA) {
					continue
				}
				baseSHA = r.CandidateSHA
			}
		} else if tcBaseSHA != "" && len(tcBaseSHA) == 40 && isAncestor(wtAbs, tcBaseSHA, candidateSHA) {
			baseSHA = tcBaseSHA
		} else if baseOut, err := exec.Command("git", "-C", wtAbs, "merge-base", candidateSHA, "HEAD~1").Output(); err == nil && len(strings.TrimSpace(string(baseOut))) == 40 {
			b := strings.TrimSpace(string(baseOut))
			if isAncestor(wtAbs, b, candidateSHA) {
				baseSHA = b
			}
		}

		branch := r.Branch
		if branch == "" {
			branch = tcBranch
		}
		if branch == "" {
			if brOut, err := exec.Command("git", "-C", wtAbs, "symbolic-ref", "--short", "HEAD").Output(); err == nil {
				branch = strings.TrimSpace(string(brOut))
			}
		}
		if branch == "" {
			continue
		}

		workspace := "wK"
		sessionID := r.HerdrSession
		if sessionID == "" {
			sessionID = tcSessionID
		}
		terminalID := "term-" + r.Name
		paneID := r.PaneID
		tabID := r.TabID

		// If launch receipt has tab/pane, we check live AgentList ONLY to verify matching incarnation
		if agents, err := AgentList(); err == nil {
			for _, a := range agents {
				if a.Name == r.Name && ((r.TabID != "" && a.TabID == r.TabID) || (r.PaneID != "" && a.PaneID == r.PaneID)) {
					if a.Workspace != "" {
						workspace = a.Workspace
					}
					if a.TerminalID != "" {
						terminalID = a.TerminalID
					}
					if a.PaneID != "" {
						paneID = a.PaneID
					}
					if a.TabID != "" {
						tabID = a.TabID
					}
					// Live agent session must match bound session if both present
					if sessionID == "" && a.Session.Value != "" && tcSessionID == a.Session.Value {
						sessionID = a.Session.Value
					}
					break
				}
			}
		}
		if sessionID == "" || tabID == "" || paneID == "" {
			// No unauthenticated fabricated sessions allowed
			continue
		}
		generation := "gen-" + taskRef + "-" + candidateSHA[:8]
		if enrolledKeys[generation] || enrolledKeys[taskRef+":"+candidateSHA] || enrolledKeys[tabID] {
			continue
		}

		m := NewSourceRetirementManifest(time.Now(), SourceRetirementManifest{
			Repository:     repositoryIdentity,
			TaskRef:        taskRef,
			TaskID:         taskID,
			CandidateSHA:   candidateSHA,
			BaseSHA:        baseSHA,
			Branch:         branch,
			Worktree:       worktreeRel,
			Workspace:      workspace,
			TabID:          tabID,
			PaneID:         paneID,
			TerminalID:     terminalID,
			SessionID:      sessionID,
			AgentName:      r.Name,
			Role:           role,
			AgentKind:      r.Provider,
			ReportArtifact: reportPathRel,
			ReportDigest:   reportDigest,
			Generation:     generation,
			Nonce:          candidateSHA[:12],
		})
		if err := ValidateSourceRetirementManifest(m); err != nil {
			continue
		}
		if persist {
			if err := registry.Record(m); err != nil {
				return nil, fmt.Errorf("record enrolled source retirement manifest: %w", err)
			}
		}
		enrolledKeys[generation] = true
		newlyEnrolled = append(newlyEnrolled, m)
	}
	return newlyEnrolled, nil
}
