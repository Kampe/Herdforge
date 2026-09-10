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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

	// Launch provenance check
	if e.Launch.Role != "" || e.Launch.TaskRef != "" || e.Launch.Name != "" || e.Launch.Accepted {
		if !e.Launch.Accepted {
			return blockSourceRetirement("launch provenance was not accepted")
		}
		if e.Launch.TaskRef != "" && e.Launch.TaskRef != m.TaskRef {
			return blockSourceRetirement("launch provenance task ref mismatch")
		}
		if e.Launch.Branch != "" && e.Launch.Branch != m.Branch {
			return blockSourceRetirement("launch provenance branch mismatch")
		}
		if e.Launch.CandidateSHA != "" && e.Launch.CandidateSHA != m.CandidateSHA {
			return blockSourceRetirement("launch provenance candidate SHA mismatch")
		}
	}

	// Durable handoff / report check
	if !e.Handoff.Known {
		return blockSourceRetirement("durable handoff is missing or unverified")
	}
	if e.Handoff.CandidateSHA != m.CandidateSHA {
		return blockSourceRetirement("handoff candidate SHA does not match manifest")
	}
	if e.Handoff.TaskRef != "" && e.Handoff.TaskRef != m.TaskRef {
		return blockSourceRetirement("handoff task ref does not match manifest")
	}
	if e.Handoff.AgentName != "" && e.Handoff.AgentName != m.AgentName {
		return blockSourceRetirement("handoff agent name does not match manifest")
	}
	status := strings.ToLower(strings.TrimSpace(e.Handoff.Status))
	if status != "ready" && status != "complete" && status != "completed" && status != "done" && status != "pass" {
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
				return r, fmt.Errorf("read source retirement phase %s: %w", m.Generation, err)
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
	if r.Failed > 0 {
		return r, fmt.Errorf("source retirement: %d observation failures", r.Failed)
	}
	if dryRun || r.Blocked > 0 {
		return r, nil
	}

	var opErrs []string
	for _, c := range r.Candidates {
		if c.Completed {
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
	if len(opErrs) > 0 {
		return r, errors.New(strings.Join(opErrs, "; "))
	}
	return r, nil
}
