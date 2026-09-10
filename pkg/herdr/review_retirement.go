package herdr

// FAC-708: review-lane retirement policy.  This package owns the decision and
// ordering contract; callers provide the live Herdr/git adapters.  In
// particular, candidate discovery is intentionally absent: a destructive
// target must come from a bound launch manifest.

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

	"github.com/Kampe/Herdforge/pkg/reviewack"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// ReviewRefPrefix and ReviewRetirementManifestFile are the canonical names
// shared by review admission, retirement discovery, and manifest validation.
// Keeping these in the policy package prevents callers from drifting into a
// second review namespace or registry file.
const (
	ReviewRefPrefix              = "refs/herd/reviews/"
	ReviewRetirementManifestFile = ".herd/review/retirement-manifests.jsonl"
)

func ReviewRetirementRegistryPath(root string) string {
	return filepath.Join(root, ReviewRetirementManifestFile)
}

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
	SessionID         string `json:"session_id,omitempty"`
	SessionGeneration string `json:"session_generation"`
	Reviewer          string `json:"reviewer"`
	ReviewerFamily    string `json:"reviewer_family"`
	ReviewerModel     string `json:"reviewer_model"`
	PromptArtifact    string `json:"prompt_artifact"`
	PromptDigest      string `json:"prompt_digest,omitempty"`
	Surface           string `json:"surface,omitempty"`
	ReviewRef         string `json:"review_ref,omitempty"`
	ManifestArtifact  string `json:"manifest_artifact,omitempty"`
	Generation        string `json:"generation"`
	Nonce             string `json:"nonce"`
	BindingDigest     string `json:"binding_digest"`
	RecordedAt        string `json:"recorded_at"`
}

type ReviewRetirementVerdict struct {
	Row reviewledger.LedgerRow
	Ack reviewack.Ack
}

type ReviewRetirementEvidence struct {
	Manifest     ReviewRetirementManifest
	Verdict      ReviewRetirementVerdict
	Launch       reviewledger.LedgerRow
	Live         ReviewRetirementLive
	Worktree     ReviewRetirementWorktree
	WorktreeRoot string
	PromptRoot   string
	Repository   string
}

type ReviewRetirementLive struct {
	Status                                                             string
	Focused                                                            *bool
	ProcessPresent                                                     bool
	TabPresent                                                         bool
	Workspace, TabID, PaneID, TerminalID, SessionID, SessionGeneration string
}

type ReviewRetirementWorktree struct {
	Known      bool
	Dirty      bool
	Head       string
	Branch     string
	UniqueRefs bool
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
		{"reviewer", m.Reviewer},
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
	if e.Repository == "" || filepath.Clean(e.Repository) != filepath.Clean(m.Repository) {
		return blockReviewRetirement("repository identity differs from the bound manifest")
	}
	if e.Launch.Event != string(reviewledger.EventRecord) || e.Launch.SHA != m.CandidateSHA || e.Launch.Reviewer != m.Reviewer || e.Launch.Lease != m.Nonce || e.Launch.Branch != m.TaskRef {
		return blockReviewRetirement("verified launch provenance does not bind the exact candidate, reviewer, branch, and lease")
	}
	if e.Verdict.Row.Event != string(reviewledger.EventVerdict) || e.Verdict.Row.SHA != m.CandidateSHA || e.Verdict.Row.CandidateSHA != m.CandidateSHA || e.Verdict.Row.Reviewer != m.Reviewer ||
		(e.Verdict.Row.Verdict != string(reviewledger.VerdictPASS) && e.Verdict.Row.Verdict != string(reviewledger.VerdictFAIL) && e.Verdict.Row.Verdict != string(reviewledger.VerdictBLOCKED)) {
		return blockReviewRetirement("canonical ledger has no terminal PASS, FAIL, or BLOCKED verdict for the exact reviewer and candidate")
	}
	if e.Verdict.Ack.SHA != m.CandidateSHA || e.Verdict.Ack.Reviewer != m.Reviewer || e.Verdict.Ack.LaunchIdentity != m.Reviewer || e.Verdict.Ack.ArtifactDigest == "" || e.Verdict.Ack.ArtifactDigest != e.Verdict.Row.ArtifactDigest {
		return blockReviewRetirement("canonical reviewer acknowledgment is missing or mismatched")
	}
	if e.Live.Status != "idle" && e.Live.Status != "done" {
		return blockReviewRetirement("review lane is still active or its status is unknown")
	}
	if e.Live.Focused == nil {
		return blockReviewRetirement("review tab focus is unknown")
	}
	if strings.TrimSpace(m.SessionID) == "" {
		return blockReviewRetirement("legacy launch lacks authenticated session identity; native retirement unsupported")
	}
	if e.Live.SessionID != m.SessionID && e.Live.Status != "done" {
		return blockReviewRetirement("live session identity differs from the bound launch")
	}
	if *e.Live.Focused {
		return blockReviewRetirement("review tab is focused")
	}
	if !e.Worktree.Known {
		return blockReviewRetirement("review worktree state is unknown")
	}
	if e.Worktree.Dirty {
		return blockReviewRetirement("review worktree is dirty")
	}
	if strings.TrimSpace(e.Worktree.Head) != m.CandidateSHA || strings.TrimSpace(e.Worktree.Branch) != m.Branch {
		return blockReviewRetirement("review worktree branch or HEAD drifted from the manifest")
	}
	if e.Worktree.UniqueRefs {
		return blockReviewRetirement("review ref contains unique unmerged history")
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
		{"tab_id", m.TabID}, {"pane_id", m.PaneID}, {"terminal_id", m.TerminalID},
		{"session_id", m.SessionID},
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
	for name, p := range map[string]string{"surface": m.Surface} {
		if p != "" && (filepath.IsAbs(p) || filepath.Clean(p) == "." || strings.HasPrefix(filepath.Clean(p), ".."+string(filepath.Separator))) {
			return fmt.Errorf("review retirement manifest %s must be repository-relative", name)
		}
	}
	if m.ManifestArtifact != "" && (filepath.IsAbs(m.ManifestArtifact) || filepath.Clean(m.ManifestArtifact) == "." || strings.HasPrefix(filepath.Clean(m.ManifestArtifact), ".."+string(filepath.Separator))) {
		return errors.New("review retirement manifest artifact must be repository-relative")
	}
	if m.ReviewRef != "" && (!strings.HasPrefix(m.ReviewRef, ReviewRefPrefix) || strings.Contains(m.ReviewRef, "..")) {
		return errors.New("review retirement manifest review_ref is outside the owned review namespace")
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
	Revalidate(ReviewRetirementManifest, string) error
	Journal(ReviewRetirementManifest, string) error
	Close(ReviewRetirementManifest) error
	LeaseReleased(ReviewRetirementManifest) (bool, error)
	ReleaseLease(context.Context, ReviewRetirementManifest) error
	RemoveWorktree(ReviewRetirementManifest) error
	RemoveBranch(ReviewRetirementManifest) error
	RemoveArtifact(ReviewRetirementManifest) error
	Receipt(ReviewRetirementManifest, ReviewRetirementDecision) error
}

// ReviewRetirementPhaseReader supplies identity-bound durable completion
// receipts so replay never inspects a recycled incarnation.
type ReviewRetirementPhaseReader interface {
	Completed(ReviewRetirementManifest) (bool, error)
}

type ReviewRetirementCandidate struct {
	Manifest  ReviewRetirementManifest
	Decision  ReviewRetirementDecision `json:"decision"`
	Completed bool                     `json:"completed,omitempty"`
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
	return RetireReviewLanesContext(context.Background(), op, manifests, dryRun)
}

// RetireReviewLanesContext applies one bounded retirement transaction using
// the caller's deadline for pool and Herdr operations.
func RetireReviewLanesContext(ctx context.Context, op ReviewRetirementOp, manifests []ReviewRetirementManifest, dryRun bool) (ReviewRetirementReport, error) {
	if op == nil {
		return ReviewRetirementReport{}, errors.New("review retirement operation authority is required")
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Generation < manifests[j].Generation })
	r := ReviewRetirementReport{DryRun: dryRun}
	for _, m := range manifests {
		if err := ValidateReviewRetirementManifest(m); err != nil {
			r.Failed++
			r.Candidates = append(r.Candidates, ReviewRetirementCandidate{Manifest: m, Decision: blockReviewRetirement("manifest validation failed: " + err.Error())})
			continue
		}
		if reader, ok := op.(ReviewRetirementPhaseReader); ok {
			complete, err := reader.Completed(m)
			if err != nil {
				return r, fmt.Errorf("read retirement phase %s: %w", m.Generation, err)
			}
			if complete {
				r.Candidates = append(r.Candidates, ReviewRetirementCandidate{Manifest: m, Completed: true, Decision: ReviewRetirementDecision{Eligible: true, Reason: "already completed for exact manifest identity"}})
				continue
			}
		}
		e, err := op.Observe(m)
		if err != nil {
			decision := blockReviewRetirement("observation failed: " + err.Error())
			if errors.Is(err, errRetirementSuperseded) {
				decision = blockReviewRetirement("retained superseded manifest: old proof no longer authorizes cleanup")
				r.Blocked++
			} else {
				r.Failed++
			}
			r.Candidates = append(r.Candidates, ReviewRetirementCandidate{Manifest: m, Decision: decision})
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
	eligibleCount := 0
	unsafeBlocked := false
	for _, c := range r.Candidates {
		if c.Decision.Eligible && !c.Completed {
			eligibleCount++
		} else if !c.Completed && c.Decision.Reason != "BLOCKED: retained superseded manifest: old proof no longer authorizes cleanup" {
			unsafeBlocked = true
		}
	}
	if dryRun || unsafeBlocked || eligibleCount == 0 {
		return r, nil
	}
	for _, c := range r.Candidates {
		if c.Completed || !c.Decision.Eligible {
			continue
		}
		m := c.Manifest
		if err := op.Revalidate(m, "close"); err != nil {
			r.Failed++
			return r, fmt.Errorf("revalidate before close %s: %w", m.TabID, err)
		}
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
			if err := op.Revalidate(m, "lease-release"); err != nil {
				r.Failed++
				return r, fmt.Errorf("revalidate before lease release %s: %w", m.Slot, err)
			}
			if err := op.ReleaseLease(ctx, m); err != nil {
				r.Failed++
				return r, fmt.Errorf("release lease %s: %w", m.Slot, err)
			}
		}
		if err := op.Revalidate(m, "worktree"); err != nil {
			r.Failed++
			return r, fmt.Errorf("revalidate before worktree removal %s: %w", m.Worktree, err)
		}
		if err := op.Journal(m, "worktree-intent"); err != nil {
			r.Failed++
			return r, fmt.Errorf("journal worktree phase %s: %w", m.Generation, err)
		}
		if err := op.RemoveWorktree(m); err != nil {
			r.Failed++
			return r, fmt.Errorf("remove worktree %s: %w", m.Worktree, err)
		}
		if err := op.Journal(m, "worktree-done"); err != nil {
			r.Failed++
			return r, fmt.Errorf("journal worktree completion %s: %w", m.Generation, err)
		}
		if err := op.Revalidate(m, "branch"); err != nil {
			r.Failed++
			return r, fmt.Errorf("revalidate before branch removal %s: %w", m.Branch, err)
		}
		if err := op.Journal(m, "ref-intent"); err != nil {
			r.Failed++
			return r, fmt.Errorf("journal ref phase %s: %w", m.Generation, err)
		}
		if err := op.RemoveBranch(m); err != nil {
			r.Failed++
			return r, fmt.Errorf("remove branch %s: %w", m.Branch, err)
		}
		if err := op.Journal(m, "ref-done"); err != nil {
			r.Failed++
			return r, fmt.Errorf("journal ref completion %s: %w", m.Generation, err)
		}
		if err := op.Revalidate(m, "artifact"); err != nil {
			r.Failed++
			return r, fmt.Errorf("revalidate before artifact removal %s: %w", m.PromptArtifact, err)
		}
		if err := op.Journal(m, "artifacts-intent"); err != nil {
			r.Failed++
			return r, fmt.Errorf("journal artifact phase %s: %w", m.Generation, err)
		}
		if err := op.RemoveArtifact(m); err != nil {
			r.Failed++
			return r, fmt.Errorf("remove owned artifacts %s: %w", m.PromptArtifact, err)
		}
		if err := op.Journal(m, "artifacts-done"); err != nil {
			r.Failed++
			return r, fmt.Errorf("journal artifacts completion %s: %w", m.Generation, err)
		}
		if err := op.Receipt(m, c.Decision); err != nil {
			r.Failed++
			return r, fmt.Errorf("write retirement receipt %s: %w", m.Generation, err)
		}
		r.Retired++
	}
	return r, nil
}
