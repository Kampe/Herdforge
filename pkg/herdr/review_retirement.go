package herdr

// FAC-708: review-lane retirement policy.  This package owns the decision and
// ordering contract; callers provide the live Herdr/git adapters.  In
// particular, candidate discovery is intentionally absent: a destructive
// target must come from a bound launch manifest.

import (
	"bufio"
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
)

type ReviewRetirementManifest struct {
	Repository        string `json:"repository"`
	TaskRef           string `json:"task_ref"`
	TaskID            string `json:"task_id"`
	CandidateSHA      string `json:"candidate_sha"`
	BaseSHA           string `json:"base_sha"`
	Branch            string `json:"branch"`
	Worktree          string `json:"worktree"`
	Pool              string `json:"pool"`
	Slot              string `json:"slot"`
	LeaseGeneration   int64  `json:"lease_generation"`
	Workspace         string `json:"workspace"`
	TabID             string `json:"tab_id"`
	PaneID            string `json:"pane_id"`
	TerminalID        string `json:"terminal_id"`
	SessionGeneration string `json:"session_generation"`
	Reviewer          string `json:"reviewer"`
	ReviewerFamily    string `json:"reviewer_family"`
	ReviewerModel     string `json:"reviewer_model"`
	PromptArtifact    string `json:"prompt_artifact"`
	Generation        string `json:"generation"`
	Nonce             string `json:"nonce"`
	BindingDigest     string `json:"binding_digest"`
	RecordedAt        string `json:"recorded_at"`
}

type ReviewRetirementVerdict struct {
	CandidateSHA string
	Value        string
	Durable      bool
	Terminal     bool
	ReviewerACK  bool
}

type ReviewRetirementEvidence struct {
	Manifest      ReviewRetirementManifest
	Verdict       ReviewRetirementVerdict
	Active        bool
	Focused       bool
	Dirty         bool
	Head          string
	Branch        string
	UniqueCommits bool
	UniqueRefs    bool
	WorktreeRoot  string
	PromptRoot    string
	Repository    string
}

type ReviewRetirementDecision struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}

func blockReviewRetirement(reason string) ReviewRetirementDecision {
	return ReviewRetirementDecision{Reason: "BLOCKED: " + reason}
}

func exactSHA(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func pathUnder(root, path string) bool {
	root, path = filepath.Clean(root), filepath.Clean(path)
	if root == "." || path == "." || filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// EvaluateReviewRetirement is read-only and is shared by dry-run and acting
// callers.  Every refusal is specific so a retained manifest can be retried
// without guessing which newer lane incarnation it belongs to.
func EvaluateReviewRetirement(e ReviewRetirementEvidence) ReviewRetirementDecision {
	m := e.Manifest
	fields := []struct{ name, value string }{
		{"repository", m.Repository}, {"task_ref", m.TaskRef}, {"task_id", m.TaskID},
		{"candidate_sha", m.CandidateSHA}, {"base_sha", m.BaseSHA}, {"branch", m.Branch},
		{"worktree", m.Worktree}, {"pool", m.Pool}, {"slot", m.Slot}, {"workspace", m.Workspace},
		{"tab_id", m.TabID}, {"pane_id", m.PaneID}, {"terminal_id", m.TerminalID},
		{"session_generation", m.SessionGeneration}, {"reviewer", m.Reviewer},
		{"reviewer_family", m.ReviewerFamily}, {"reviewer_model", m.ReviewerModel},
		{"prompt_artifact", m.PromptArtifact}, {"generation", m.Generation},
		{"nonce", m.Nonce}, {"binding_digest", m.BindingDigest}, {"recorded_at", m.RecordedAt},
	}
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			return blockReviewRetirement("manifest is missing " + f.name)
		}
	}
	if !exactSHA(m.CandidateSHA) || !exactSHA(m.BaseSHA) {
		return blockReviewRetirement("manifest contains a non-exact candidate or base SHA")
	}
	if e.Repository != "" && filepath.Clean(e.Repository) != filepath.Clean(m.Repository) {
		return blockReviewRetirement("repository identity differs from the bound manifest")
	}
	if !e.Verdict.Durable || !e.Verdict.Terminal || !e.Verdict.ReviewerACK ||
		strings.TrimSpace(e.Verdict.CandidateSHA) != m.CandidateSHA {
		return blockReviewRetirement("no durable terminal verdict and reviewer ACK bound to the exact candidate SHA")
	}
	if e.Active {
		return blockReviewRetirement("review lane is still active")
	}
	if e.Focused {
		return blockReviewRetirement("review tab is focused")
	}
	if e.Dirty {
		return blockReviewRetirement("review worktree is dirty")
	}
	if strings.TrimSpace(e.Head) != m.CandidateSHA || strings.TrimSpace(e.Branch) != m.Branch {
		return blockReviewRetirement("review worktree branch or HEAD drifted from the manifest")
	}
	if e.UniqueCommits || e.UniqueRefs {
		return blockReviewRetirement("review worktree or branch contains unique unmerged history")
	}
	if !pathUnder(e.WorktreeRoot, m.Worktree) {
		return blockReviewRetirement("worktree is outside the approved review namespace")
	}
	if !pathUnder(e.PromptRoot, m.PromptArtifact) {
		return blockReviewRetirement("prompt artifact is outside the approved review namespace")
	}
	return ReviewRetirementDecision{Eligible: true, Reason: "exact bound manifest and durable terminal verdict verified"}
}

func NewReviewRetirementManifest(now time.Time, m ReviewRetirementManifest) ReviewRetirementManifest {
	if strings.TrimSpace(m.RecordedAt) == "" {
		m.RecordedAt = now.UTC().Format(time.RFC3339Nano)
	}
	if strings.TrimSpace(m.BindingDigest) == "" {
		m.BindingDigest = ReviewRetirementBindingDigest(m)
	}
	return m
}

func ReviewRetirementBindingDigest(m ReviewRetirementManifest) string {
	m.BindingDigest, m.RecordedAt = "", ""
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ValidateReviewRetirementManifest(m ReviewRetirementManifest) error {
	if !exactSHA(m.CandidateSHA) || !exactSHA(m.BaseSHA) {
		return errors.New("review retirement manifest requires full-length candidate and base SHAs")
	}
	fields := []struct{ name, value string }{
		{"repository", m.Repository}, {"task_ref", m.TaskRef}, {"task_id", m.TaskID}, {"branch", m.Branch},
		{"worktree", m.Worktree}, {"pool", m.Pool}, {"slot", m.Slot}, {"workspace", m.Workspace},
		{"tab_id", m.TabID}, {"pane_id", m.PaneID}, {"terminal_id", m.TerminalID}, {"session_generation", m.SessionGeneration},
		{"reviewer", m.Reviewer}, {"reviewer_family", m.ReviewerFamily}, {"reviewer_model", m.ReviewerModel},
		{"prompt_artifact", m.PromptArtifact}, {"generation", m.Generation}, {"nonce", m.Nonce}, {"recorded_at", m.RecordedAt},
	}
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("review retirement manifest missing %s", f.name)
		}
	}
	if m.LeaseGeneration <= 0 {
		return errors.New("review retirement manifest lease_generation must be positive")
	}
	if m.BindingDigest != ReviewRetirementBindingDigest(m) {
		return errors.New("review retirement manifest binding digest is invalid")
	}
	if filepath.IsAbs(m.Worktree) || filepath.IsAbs(m.PromptArtifact) || strings.Contains(filepath.Clean(m.Worktree), ".."+string(filepath.Separator)) || strings.Contains(filepath.Clean(m.PromptArtifact), ".."+string(filepath.Separator)) {
		return errors.New("review retirement manifest paths must be repository-relative")
	}
	return nil
}

type ReviewRetirementRegistry struct{ Path string }

func (r ReviewRetirementRegistry) Record(m ReviewRetirementManifest) error {
	if err := ValidateReviewRetirementManifest(m); err != nil {
		return err
	}
	if strings.TrimSpace(r.Path) == "" {
		return errors.New("review retirement registry path is required")
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

func (r ReviewRetirementRegistry) Latest() ([]ReviewRetirementManifest, error) {
	f, err := os.Open(r.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []ReviewRetirementManifest
	s := bufio.NewScanner(f)
	for s.Scan() {
		var m ReviewRetirementManifest
		if err := json.Unmarshal(s.Bytes(), &m); err != nil {
			return nil, fmt.Errorf("decode review retirement registry: %w", err)
		}
		out = append(out, m)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type ReviewRetirementOp interface {
	Observe(ReviewRetirementManifest) (ReviewRetirementEvidence, error)
	Close(ReviewRetirementManifest) error
	LeaseReleased(ReviewRetirementManifest) (bool, error)
	ReleaseLease(ReviewRetirementManifest) error
	RemoveWorktree(ReviewRetirementManifest) error
	RemoveBranch(ReviewRetirementManifest) error
	RemoveArtifact(ReviewRetirementManifest) error
	Receipt(ReviewRetirementManifest, ReviewRetirementDecision) error
}

type ReviewRetirementCandidate struct {
	Manifest ReviewRetirementManifest
	Decision ReviewRetirementDecision `json:"decision"`
}
type ReviewRetirementReport struct {
	DryRun     bool                        `json:"dry_run"`
	Candidates []ReviewRetirementCandidate `json:"candidates"`
	Retired    int                         `json:"retired"`
	Blocked    int                         `json:"blocked"`
	Failed     int                         `json:"failed"`
}

// RetireReviewLanes preflights the complete selected set before its first
// mutation. A later unsafe lane therefore cannot be discovered after an
// earlier lane has already been removed. The operation order is close,
// lease-release, worktree/ref, then owned artifacts and receipt.
func RetireReviewLanes(op ReviewRetirementOp, manifests []ReviewRetirementManifest, dryRun bool) (ReviewRetirementReport, error) {
	if op == nil {
		return ReviewRetirementReport{}, errors.New("review retirement operation authority is required")
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Generation < manifests[j].Generation })
	r := ReviewRetirementReport{DryRun: dryRun}
	for _, m := range manifests {
		e, err := op.Observe(m)
		if err != nil {
			r.Failed++
			r.Candidates = append(r.Candidates, ReviewRetirementCandidate{Manifest: m, Decision: blockReviewRetirement("observation failed: " + err.Error())})
			continue
		}
		d := EvaluateReviewRetirement(e)
		r.Candidates = append(r.Candidates, ReviewRetirementCandidate{Manifest: m, Decision: d})
		if !d.Eligible {
			r.Blocked++
		}
	}
	if r.Failed > 0 {
		return r, fmt.Errorf("review retirement: %d observation failures", r.Failed)
	}
	if dryRun || r.Blocked > 0 {
		return r, nil
	}
	for _, c := range r.Candidates {
		m := c.Manifest
		if err := op.Close(m); err != nil {
			r.Failed++
			return r, fmt.Errorf("close %s: %w", m.TabID, err)
		}
		released, err := op.LeaseReleased(m)
		if err != nil {
			r.Failed++
			return r, fmt.Errorf("lease readback %s: %w", m.Slot, err)
		}
		if !released {
			if err := op.ReleaseLease(m); err != nil {
				r.Failed++
				return r, fmt.Errorf("release lease %s: %w", m.Slot, err)
			}
		}
		if err := op.RemoveWorktree(m); err != nil {
			r.Failed++
			return r, fmt.Errorf("remove worktree %s: %w", m.Worktree, err)
		}
		if err := op.RemoveBranch(m); err != nil {
			r.Failed++
			return r, fmt.Errorf("remove branch %s: %w", m.Branch, err)
		}
		if err := op.RemoveArtifact(m); err != nil {
			r.Failed++
			return r, fmt.Errorf("remove owned artifacts %s: %w", m.PromptArtifact, err)
		}
		if err := op.Receipt(m, c.Decision); err != nil {
			r.Failed++
			return r, fmt.Errorf("write retirement receipt %s: %w", m.Generation, err)
		}
		r.Retired++
	}
	return r, nil
}
