package worktree

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/reviewroot"
)

// ReviewPoolRecoveryRequest is the immutable operator authorization for one
// exact review-pool transaction. Every path is repository-relative so the
// authorization and journal remain portable; live root/host identity is
// independently re-read through ReviewPoolRecoveryProbes.
type ReviewPoolRecoveryRequest struct {
	Version       int                      `json:"version"`
	TransactionID string                   `json:"transaction_id"`
	Repository    string                   `json:"repository"`
	ProjectID     string                   `json:"project_id"`
	Host          string                   `json:"host"`
	PoolRoot      string                   `json:"pool_root"`
	StateRevision string                   `json:"state_revision"`
	TaskRef       string                   `json:"task_ref"`
	TaskID        string                   `json:"task_id"`
	TaskRevision  string                   `json:"task_revision"`
	TaskStatus    string                   `json:"task_status"`
	Slots         []ReviewPoolRecoverySlot `json:"slots"`
}

type ReviewPoolRecoverySlot struct {
	Name         string                       `json:"name"`
	Path         string                       `json:"path"`
	LeaseID      string                       `json:"lease_id"`
	Purpose      string                       `json:"purpose"`
	Head         string                       `json:"head"`
	Base         string                       `json:"base"`
	CandidateSHA string                       `json:"candidate_sha"`
	Evidence     []ReviewPoolRecoveryEvidence `json:"evidence"`
	Nested       []ReviewPoolRecoveryNested   `json:"nested,omitempty"`
}

// ReviewPoolRecoveryNested authorizes removal of one recursively registered
// Git worktree beneath a contaminated slot. Authority is copied and verified
// in the recovery archive before the registered worktree is removed.
type ReviewPoolRecoveryNested struct {
	Path         string                       `json:"path"`
	Head         string                       `json:"head"`
	CandidateSHA string                       `json:"candidate_sha,omitempty"`
	Authority    []ReviewPoolRecoveryEvidence `json:"authority,omitempty"`
}

type ReviewPoolRecoveryEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ReviewPoolRecoveryProbes are mandatory live authorities. Unknown is an
// error, never equivalent to absent/dead/unchanged.
type ReviewPoolRecoveryProbes struct {
	Hostname        func(context.Context) (string, error)
	Repository      func(context.Context) (string, error)
	ProjectID       func(context.Context) (string, error)
	HolderLive      func(context.Context, string) (bool, error)
	OpenFiles       func(context.Context, string) ([]string, error)
	TaskEvidence    func(context.Context, string, string) (revision, status string, err error)
	VerdictEvidence func(context.Context, string) (ReviewPoolVerdictObservation, error)
}

// ReviewPoolVerdictObservation is the canonical parser's live readback of a
// retained review artifact, including its transport/ingest location.
type ReviewPoolVerdictObservation struct {
	TaskRef        string
	CandidateSHA   string
	Verdict        string
	Reviewer       string
	ReviewerFamily string
	BuilderFamily  string
	State          string
}

type ReviewPoolRecoveryResult struct {
	TransactionID string   `json:"transaction_id"`
	Recovered     []string `json:"recovered"`
	StateRevision string   `json:"state_revision"`
	Idempotent    bool     `json:"idempotent"`
	DryRun        bool     `json:"dry_run"`
}

type reviewPoolRecoveryJournal struct {
	At             time.Time `json:"at"`
	TransactionID  string    `json:"transaction_id"`
	RequestDigest  string    `json:"request_digest"`
	Phase          string    `json:"phase"`
	ResultRevision string    `json:"result_revision,omitempty"`
	Recovered      []string  `json:"recovered,omitempty"`
}

type recoveryValidation struct {
	state          poolState
	requestDigest  string
	journal        []reviewPoolRecoveryJournal
	started        bool
	stateRecovered bool
	completed      *reviewPoolRecoveryJournal
	slots          []recoverySlotValidation
}

type recoverySlotValidation struct {
	req  ReviewPoolRecoverySlot
	live PoolSlot
	path string
}

func (p *Pool) recoveryJournalPath() string {
	return filepath.Join(p.RepoRoot, ".herd", "recovery", "review-pool.jsonl")
}

// StateRevision returns the digest of the exact durable pool state bytes.
func (p *Pool) StateRevision() (string, error) {
	b, err := os.ReadFile(p.statePath())
	if err != nil {
		return "", fmt.Errorf("worktree pool recovery: read state revision: %w", err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

// RecoverExact validates and, when apply is true, recovers only the explicitly
// authorized slots. It holds the ordinary pool lock for the entire validation
// and mutation boundary, giving concurrent attempts exactly one winner.
func (p *Pool) RecoverExact(ctx context.Context, req ReviewPoolRecoveryRequest, probes ReviewPoolRecoveryProbes, apply bool) (*ReviewPoolRecoveryResult, error) {
	if p == nil {
		return nil, errors.New("worktree pool recovery: pool is required")
	}
	var result *ReviewPoolRecoveryResult
	err := p.withLock(func() error {
		v, err := p.validateRecovery(ctx, req, probes)
		if err != nil {
			return err
		}
		if v.completed != nil {
			if err := p.verifyCompletedRecovery(ctx, req, v, probes); err != nil {
				return err
			}
			result = &ReviewPoolRecoveryResult{
				TransactionID: req.TransactionID, Recovered: append([]string(nil), v.completed.Recovered...),
				StateRevision: v.completed.ResultRevision, Idempotent: true,
			}
			return nil
		}
		if !apply {
			result = &ReviewPoolRecoveryResult{TransactionID: req.TransactionID, DryRun: true}
			for _, s := range v.slots {
				result.Recovered = append(result.Recovered, s.req.Name)
			}
			return nil
		}
		if v.stateRecovered {
			for _, slot := range v.slots {
				ref := SalvageRefFor("review/" + req.TaskRef + "/" + slot.req.CandidateSHA)
				if err := verifyGitRef(ctx, p.RepoRoot, ref, slot.req.CandidateSHA); err != nil {
					return fmt.Errorf("worktree pool recovery: partial state lacks candidate preservation: %w", err)
				}
			}
			resultRevision, err := p.StateRevision()
			if err != nil {
				return err
			}
			recovered := make([]string, 0, len(v.slots))
			for _, slot := range v.slots {
				recovered = append(recovered, slot.req.Name)
			}
			sort.Strings(recovered)
			entry := reviewPoolRecoveryJournal{At: time.Now().UTC(), TransactionID: req.TransactionID, RequestDigest: v.requestDigest, Phase: "complete", ResultRevision: resultRevision, Recovered: recovered}
			if err := appendRecoveryJournal(p.recoveryJournalPath(), entry); err != nil {
				return err
			}
			result = &ReviewPoolRecoveryResult{TransactionID: req.TransactionID, Recovered: recovered, StateRevision: resultRevision, Idempotent: true}
			return nil
		}
		if !v.started {
			if err := appendRecoveryJournal(p.recoveryJournalPath(), reviewPoolRecoveryJournal{
				At: time.Now().UTC(), TransactionID: req.TransactionID, RequestDigest: v.requestDigest, Phase: "started",
			}); err != nil {
				return err
			}
		}

		wm := NewWorktreeManager(p.RepoRoot)
		for _, slot := range v.slots {
			// External holder/process/task planes do not participate in the pool
			// file lock. Re-read the complete slot evidence at the last boundary
			// before its first mutation.
			if err := p.validateSlotLive(ctx, req, slot.req, slot.path, probes, v.started); err != nil {
				return fmt.Errorf("worktree pool recovery: final slot revalidation: %w", err)
			}
			if err := p.recoverNested(ctx, req, slot, wm, probes); err != nil {
				return err
			}
			preserveRef := SalvageRefFor("review/" + req.TaskRef + "/" + slot.req.CandidateSHA)
			if err := wm.ensureSalvageRef(ctx, preserveRef, slot.req.CandidateSHA); err != nil {
				return fmt.Errorf("worktree pool recovery: preserve candidate for %s: %w", slot.req.Name, err)
			}
			if err := verifyGitRef(ctx, p.RepoRoot, preserveRef, slot.req.CandidateSHA); err != nil {
				return err
			}
			current, err := gitRevision(ctx, slot.path, "HEAD")
			if err != nil {
				return err
			}
			// Retry after a crash between reset and journal/state publication is
			// safe only at one of the two exact authorized commits.
			if current != slot.req.Base {
				if current != slot.req.Head {
					return fmt.Errorf("worktree pool recovery: partial write left %s at ambiguous HEAD %s", slot.req.Name, current)
				}
				out, err := exec.CommandContext(ctx, "git", "-C", slot.path, "reset", "--hard", slot.req.Base).CombinedOutput()
				if err != nil {
					return fmt.Errorf("worktree pool recovery: reset %s: %v (%s)", slot.req.Name, err, strings.TrimSpace(string(out)))
				}
			}
			clean, err := gitClean(ctx, p.RepoRoot, slot.path)
			if err != nil || !clean {
				return fmt.Errorf("worktree pool recovery: %s not clean after exact reset: clean=%v err=%v", slot.req.Name, clean, err)
			}
			if err := p.revalidateEvidence(ctx, req, slot.req, probes); err != nil {
				return err
			}
		}
		finalTaskRevision, finalTaskStatus, err := probes.TaskEvidence(ctx, req.TaskRef, req.TaskID)
		if err != nil || finalTaskRevision != req.TaskRevision || finalTaskStatus != req.TaskStatus {
			return fmt.Errorf("worktree pool recovery: task/evidence drift at commit boundary: revision=%q status=%q err=%v", finalTaskRevision, finalTaskStatus, err)
		}

		state := v.state
		for i := range state.Slots {
			for _, slot := range v.slots {
				if state.Slots[i].Name == slot.req.Name {
					state.Slots[i].Purpose = ""
					state.Slots[i].LeaseID = ""
					state.Slots[i].LeasedAt = time.Time{}
					state.Slots[i].Base = slot.req.Base
				}
			}
		}
		if err := p.writeState(state); err != nil {
			return err
		}
		resultRevision, err := p.StateRevision()
		if err != nil {
			return err
		}
		recovered := make([]string, 0, len(v.slots))
		for _, slot := range v.slots {
			recovered = append(recovered, slot.req.Name)
		}
		sort.Strings(recovered)
		entry := reviewPoolRecoveryJournal{
			At: time.Now().UTC(), TransactionID: req.TransactionID, RequestDigest: v.requestDigest,
			Phase: "complete", ResultRevision: resultRevision, Recovered: recovered,
		}
		if err := appendRecoveryJournal(p.recoveryJournalPath(), entry); err != nil {
			return err
		}
		result = &ReviewPoolRecoveryResult{TransactionID: req.TransactionID, Recovered: recovered, StateRevision: resultRevision}
		return nil
	})
	return result, err
}

func (p *Pool) validateRecovery(ctx context.Context, req ReviewPoolRecoveryRequest, probes ReviewPoolRecoveryProbes) (*recoveryValidation, error) {
	if req.Version != 1 || strings.TrimSpace(req.TransactionID) == "" || strings.TrimSpace(req.Repository) == "" ||
		strings.TrimSpace(req.ProjectID) == "" || strings.TrimSpace(req.Host) == "" || strings.TrimSpace(req.StateRevision) == "" ||
		strings.TrimSpace(req.TaskRef) == "" || strings.TrimSpace(req.TaskID) == "" || strings.TrimSpace(req.TaskRevision) == "" ||
		strings.TrimSpace(req.TaskStatus) == "" || len(req.Slots) == 0 {
		return nil, errors.New("worktree pool recovery: incomplete exact request")
	}
	if !safeRecoveryID(req.TransactionID) {
		return nil, fmt.Errorf("worktree pool recovery: unsafe transaction id %q", req.TransactionID)
	}
	if probes.Hostname == nil || probes.Repository == nil || probes.ProjectID == nil || probes.HolderLive == nil || probes.OpenFiles == nil || probes.TaskEvidence == nil || probes.VerdictEvidence == nil {
		return nil, errors.New("worktree pool recovery: all live probes are required")
	}
	if err := requirePortableRelative(req.PoolRoot); err != nil {
		return nil, fmt.Errorf("worktree pool recovery: pool root: %w", err)
	}
	wantPool := filepath.Join(p.RepoRoot, filepath.FromSlash(req.PoolRoot))
	same, err := sameExistingPath(wantPool, p.Root)
	if err != nil || !same {
		return nil, fmt.Errorf("worktree pool recovery: wrong pool root: configured=%q request=%q err=%v", p.Root, req.PoolRoot, err)
	}
	if err := compareProbe(ctx, "host", req.Host, probes.Hostname); err != nil {
		return nil, err
	}
	if err := compareProbe(ctx, "repository", req.Repository, probes.Repository); err != nil {
		return nil, err
	}
	if err := compareProbe(ctx, "project", req.ProjectID, probes.ProjectID); err != nil {
		return nil, err
	}
	taskRevision, taskStatus, err := probes.TaskEvidence(ctx, req.TaskRef, req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("worktree pool recovery: task evidence unknown: %w", err)
	}
	if taskRevision != req.TaskRevision || taskStatus != req.TaskStatus {
		return nil, fmt.Errorf("worktree pool recovery: task/evidence drift: revision=%q status=%q", taskRevision, taskStatus)
	}

	digest, err := recoveryRequestDigest(req)
	if err != nil {
		return nil, err
	}
	journal, err := readRecoveryJournal(p.recoveryJournalPath())
	if err != nil {
		return nil, err
	}
	v := &recoveryValidation{requestDigest: digest, journal: journal}
	for i := range journal {
		entry := &journal[i]
		if entry.TransactionID != req.TransactionID {
			continue
		}
		if entry.RequestDigest != digest {
			return nil, fmt.Errorf("worktree pool recovery: transaction %s is ambiguous: request digest changed", req.TransactionID)
		}
		if entry.Phase == "started" {
			if v.started || v.completed != nil {
				return nil, fmt.Errorf("worktree pool recovery: transaction %s has ambiguous journal ordering", req.TransactionID)
			}
			v.started = true
		}
		if entry.Phase == "complete" {
			if !v.started || v.completed != nil {
				return nil, fmt.Errorf("worktree pool recovery: transaction %s has ambiguous completion journal", req.TransactionID)
			}
			v.completed = entry
		}
	}

	state, err := p.readState()
	if err != nil {
		return nil, err
	}
	v.state = state
	stateRevision, err := p.StateRevision()
	if err != nil {
		return nil, err
	}
	if v.completed == nil && stateRevision != req.StateRevision {
		if v.started && recoveryStateIsFinal(state, req.Slots) {
			v.stateRecovered = true
		} else {
			return nil, fmt.Errorf("worktree pool recovery: state revision drift/partial write: got %s want %s", stateRevision, req.StateRevision)
		}
	}
	if v.completed != nil && stateRevision != v.completed.ResultRevision {
		return nil, fmt.Errorf("worktree pool recovery: completed state revision drift: got %s want %s", stateRevision, v.completed.ResultRevision)
	}

	byName := make(map[string]PoolSlot, len(state.Slots))
	for _, slot := range state.Slots {
		if _, dup := byName[slot.Name]; dup {
			return nil, fmt.Errorf("worktree pool recovery: ambiguous duplicate pool slot %s", slot.Name)
		}
		byName[slot.Name] = slot
	}
	seenNames, seenPaths := map[string]bool{}, map[string]bool{}
	for _, requested := range req.Slots {
		if seenNames[requested.Name] || seenPaths[requested.Path] {
			return nil, fmt.Errorf("worktree pool recovery: duplicate slot name/path in request")
		}
		seenNames[requested.Name], seenPaths[requested.Path] = true, true
		if err := validateRecoverySlotRequest(requested); err != nil {
			return nil, err
		}
		live, ok := byName[requested.Name]
		if !ok {
			return nil, fmt.Errorf("worktree pool recovery: requested slot %s is absent", requested.Name)
		}
		if !v.started && v.completed == nil && (live.LeaseID != requested.LeaseID || live.Purpose != requested.Purpose) {
			return nil, fmt.Errorf("worktree pool recovery: lease/path drift for %s", requested.Name)
		}
		if v.completed == nil && !v.stateRecovered && (live.LeaseID != requested.LeaseID || live.Purpose != requested.Purpose) {
			return nil, fmt.Errorf("worktree pool recovery: lease drift for %s", requested.Name)
		}
		path := filepath.Join(p.RepoRoot, filepath.FromSlash(requested.Path))
		same, err := sameExistingPath(path, live.Path)
		if err != nil || !same {
			return nil, fmt.Errorf("worktree pool recovery: slot %s path/root mismatch: %v", requested.Name, err)
		}
		if err := p.validateSlotLive(ctx, req, requested, path, probes, v.started || v.completed != nil || v.stateRecovered); err != nil {
			return nil, err
		}
		v.slots = append(v.slots, recoverySlotValidation{req: requested, live: live, path: path})
	}
	sort.Slice(v.slots, func(i, j int) bool { return v.slots[i].req.Name < v.slots[j].req.Name })
	return v, nil
}

func recoveryStateIsFinal(state poolState, requested []ReviewPoolRecoverySlot) bool {
	byName := make(map[string]PoolSlot, len(state.Slots))
	for _, slot := range state.Slots {
		byName[slot.Name] = slot
	}
	for _, want := range requested {
		got, ok := byName[want.Name]
		if !ok || got.LeaseID != "" || got.Purpose != "" || !got.LeasedAt.IsZero() || got.Base != want.Base {
			return false
		}
	}
	return true
}

func validateRecoverySlotRequest(s ReviewPoolRecoverySlot) error {
	if strings.TrimSpace(s.Name) == "" || strings.TrimSpace(s.Path) == "" || strings.TrimSpace(s.Head) == "" || strings.TrimSpace(s.Base) == "" ||
		strings.TrimSpace(s.CandidateSHA) == "" {
		return fmt.Errorf("worktree pool recovery: incomplete exact slot %q", s.Name)
	}
	leased := strings.TrimSpace(s.LeaseID) != "" || strings.TrimSpace(s.Purpose) != ""
	if (strings.TrimSpace(s.LeaseID) == "") != (strings.TrimSpace(s.Purpose) == "") {
		return fmt.Errorf("worktree pool recovery: slot %s has ambiguous partial lease identity", s.Name)
	}
	if leased && len(s.Evidence) == 0 {
		return fmt.Errorf("worktree pool recovery: reviewed slot %s has no canonical verdict evidence", s.Name)
	}
	if s.CandidateSHA != s.Head {
		return fmt.Errorf("worktree pool recovery: candidate ambiguity for %s: candidate %s != HEAD %s", s.Name, s.CandidateSHA, s.Head)
	}
	if err := requirePortableRelative(s.Path); err != nil {
		return fmt.Errorf("worktree pool recovery: slot path: %w", err)
	}
	seen := map[string]bool{}
	for _, evidence := range s.Evidence {
		if err := validateEvidenceBinding(evidence); err != nil {
			return err
		}
		path := filepath.ToSlash(evidence.Path)
		if !strings.HasPrefix(path, reviewroot.Rel+"/") {
			return fmt.Errorf("worktree pool recovery: evidence %q is outside canonical .herd/review authority", evidence.Path)
		}
		if seen[evidence.Path] {
			return fmt.Errorf("worktree pool recovery: duplicate evidence path %q", evidence.Path)
		}
		seen[evidence.Path] = true
	}
	return nil
}

func (p *Pool) validateSlotLive(ctx context.Context, req ReviewPoolRecoveryRequest, slot ReviewPoolRecoverySlot, path string, probes ReviewPoolRecoveryProbes, retry bool) error {
	live, err := probes.HolderLive(ctx, slot.Purpose)
	if err != nil {
		return fmt.Errorf("worktree pool recovery: holder state unknown for %s: %w", slot.Name, err)
	}
	if live {
		return fmt.Errorf("worktree pool recovery: live holder for %s", slot.Name)
	}
	open, err := probes.OpenFiles(ctx, path)
	if err != nil {
		return fmt.Errorf("worktree pool recovery: open files/cwd state unknown for %s: %w", slot.Name, err)
	}
	if len(open) > 0 {
		return fmt.Errorf("worktree pool recovery: open files or process cwd in %s: %s", slot.Name, strings.Join(open, ", "))
	}
	clean, err := gitClean(ctx, p.RepoRoot, path)
	if err != nil {
		return fmt.Errorf("worktree pool recovery: inspect source %s: %w", slot.Name, err)
	}
	if !clean {
		return fmt.Errorf("worktree pool recovery: dirty source slot %s", slot.Name)
	}
	head, err := gitRevision(ctx, path, "HEAD")
	if err != nil {
		return err
	}
	if head != slot.Head && !(retry && head == slot.Base) {
		return fmt.Errorf("worktree pool recovery: HEAD drift for %s: got %s want %s", slot.Name, head, slot.Head)
	}
	for _, sha := range []string{slot.Head, slot.Base, slot.CandidateSHA} {
		if err := verifyCommit(ctx, p.RepoRoot, sha); err != nil {
			return err
		}
	}
	if err := p.revalidateEvidence(ctx, req, slot, probes); err != nil {
		return err
	}
	return p.validateNested(ctx, req.TransactionID, req.TaskRef, slot, path, probes, retry)
}

func (p *Pool) validateNested(ctx context.Context, transactionID, taskRef string, slot ReviewPoolRecoverySlot, slotPath string, probes ReviewPoolRecoveryProbes, retry bool) error {
	registered, err := registeredDescendants(ctx, p.RepoRoot, slotPath)
	if err != nil {
		return err
	}
	registeredSet := make(map[string]bool, len(registered))
	for _, path := range registered {
		registeredSet[path] = true
	}
	expectedSet := make(map[string]bool, len(slot.Nested))
	for _, nested := range slot.Nested {
		if err := requirePortableRelative(nested.Path); err != nil {
			return fmt.Errorf("worktree pool recovery: nested path: %w", err)
		}
		path := filepath.Join(p.RepoRoot, filepath.FromSlash(nested.Path))
		if !pathWithin(slotPath, path) {
			return fmt.Errorf("worktree pool recovery: nested path %q escapes exact slot", nested.Path)
		}
		resolved, rerr := filepath.EvalSymlinks(path)
		if rerr != nil {
			if !retry || !errors.Is(rerr, os.ErrNotExist) {
				return fmt.Errorf("worktree pool recovery: nested path %q unresolved: %w", nested.Path, rerr)
			}
			for _, authority := range nested.Authority {
				archived := filepath.Join(p.RepoRoot, ".herd", "recovery", "review-pool", transactionID, "authority", filepath.FromSlash(authority.Path))
				got, err := fileSHA256(archived)
				if err != nil || got != strings.ToLower(authority.SHA256) {
					return fmt.Errorf("worktree pool recovery: removed nested authority %q is not preserved: got=%s err=%v", authority.Path, got, err)
				}
			}
			if nested.CandidateSHA != "" {
				ref := SalvageRefFor("review/" + taskRef + "/nested/" + nested.CandidateSHA)
				if err := verifyGitRef(ctx, p.RepoRoot, ref, nested.CandidateSHA); err != nil {
					return fmt.Errorf("worktree pool recovery: removed nested candidate is not preserved: %w", err)
				}
			}
			continue
		}
		if expectedSet[resolved] {
			return fmt.Errorf("worktree pool recovery: duplicate nested worktree %q", nested.Path)
		}
		expectedSet[resolved] = true
		if !registeredSet[resolved] {
			return fmt.Errorf("worktree pool recovery: nested path %q is not an exact registered Git worktree", nested.Path)
		}
		open, err := probes.OpenFiles(ctx, resolved)
		if err != nil || len(open) > 0 {
			return fmt.Errorf("worktree pool recovery: nested open files/cwd unknown or live for %q: holders=%v err=%v", nested.Path, open, err)
		}
		clean, err := gitClean(ctx, p.RepoRoot, resolved)
		if err != nil || !clean {
			return fmt.Errorf("worktree pool recovery: nested source dirty or unknown for %q: clean=%v err=%v", nested.Path, clean, err)
		}
		head, err := gitRevision(ctx, resolved, "HEAD")
		if err != nil || head != nested.Head {
			return fmt.Errorf("worktree pool recovery: nested HEAD drift for %q: got=%s err=%v", nested.Path, head, err)
		}
		if nested.CandidateSHA != "" && nested.CandidateSHA != nested.Head {
			return fmt.Errorf("worktree pool recovery: nested candidate ambiguity for %q", nested.Path)
		}
		if err := p.validateNestedAuthority(resolved, registered, nested.Authority); err != nil {
			return err
		}
	}
	for path := range registeredSet {
		if !expectedSet[path] {
			return fmt.Errorf("worktree pool recovery: unexpected/ambiguous nested registered worktree %q", path)
		}
	}
	return nil
}

func (p *Pool) validateNestedAuthority(nestedRoot string, allRegistered []string, expected []ReviewPoolRecoveryEvidence) error {
	args := append([]string{"-C", nestedRoot, "ls-files"}, gitroot.IgnoredUntrackedArgs(".herd")...)
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("worktree pool recovery: inventory nested .herd authority: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	actual := map[string]string{}
	for _, rel := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		abs := filepath.Join(nestedRoot, filepath.FromSlash(rel))
		insideChild := false
		for _, child := range allRegistered {
			if !sameWorktreePath(child, nestedRoot) && (sameWorktreePath(child, abs) || pathWithin(child, abs)) {
				insideChild = true
				break
			}
		}
		if insideChild {
			continue
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return fmt.Errorf("worktree pool recovery: inspect nested authority %q: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("worktree pool recovery: nested .herd authority %q is not a regular file", rel)
		}
		canonicalRepo, err := filepath.EvalSymlinks(p.RepoRoot)
		if err != nil {
			return fmt.Errorf("worktree pool recovery: resolve repository for authority inventory: %w", err)
		}
		repoRel, err := filepath.Rel(canonicalRepo, abs)
		if err != nil || strings.HasPrefix(repoRel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("worktree pool recovery: nested authority escaped repository: %q", rel)
		}
		digest, err := fileSHA256(abs)
		if err != nil {
			return err
		}
		actual[filepath.ToSlash(repoRel)] = digest
	}
	want := map[string]string{}
	for _, authority := range expected {
		if err := validateEvidenceBinding(authority); err != nil {
			return err
		}
		if _, dup := want[authority.Path]; dup {
			return fmt.Errorf("worktree pool recovery: duplicate nested authority %q", authority.Path)
		}
		want[authority.Path] = strings.ToLower(authority.SHA256)
	}
	if len(actual) != len(want) {
		return fmt.Errorf("worktree pool recovery: unbound nested .herd authority: actual=%d expected=%d", len(actual), len(want))
	}
	for path, digest := range actual {
		if want[path] != digest {
			return fmt.Errorf("worktree pool recovery: nested .herd authority drift for %q", path)
		}
	}
	return nil
}

func (p *Pool) revalidateEvidence(ctx context.Context, req ReviewPoolRecoveryRequest, slot ReviewPoolRecoverySlot, probes ReviewPoolRecoveryProbes) error {
	for _, evidence := range slot.Evidence {
		path := filepath.Join(p.RepoRoot, filepath.FromSlash(evidence.Path))
		got, err := fileSHA256(path)
		if err != nil {
			return fmt.Errorf("worktree pool recovery: evidence %q unreadable: %w", evidence.Path, err)
		}
		if got != strings.ToLower(evidence.SHA256) {
			return fmt.Errorf("worktree pool recovery: evidence drift for %q: got %s want %s", evidence.Path, got, evidence.SHA256)
		}
		observation, err := probes.VerdictEvidence(ctx, path)
		if err != nil {
			return fmt.Errorf("worktree pool recovery: canonical verdict evidence %q is invalid: %w", evidence.Path, err)
		}
		if !strings.EqualFold(observation.TaskRef, req.TaskRef) || observation.CandidateSHA != slot.CandidateSHA {
			return fmt.Errorf("worktree pool recovery: evidence %q does not exactly bind task %s and candidate %s", evidence.Path, req.TaskRef, slot.CandidateSHA)
		}
		if strings.TrimSpace(observation.Reviewer) == "" || (observation.Verdict != "PASS" && observation.Verdict != "FAIL" && observation.Verdict != "BLOCKED") {
			return fmt.Errorf("worktree pool recovery: evidence %q lacks exact reviewer/verdict identity", evidence.Path)
		}
		if observation.State != "inbox" && observation.State != "ingested" {
			return fmt.Errorf("worktree pool recovery: evidence %q has ambiguous transport/ingest state %q", evidence.Path, observation.State)
		}
		if strings.TrimSpace(observation.ReviewerFamily) == "" || strings.TrimSpace(observation.BuilderFamily) == "" ||
			strings.EqualFold(strings.TrimSpace(observation.ReviewerFamily), strings.TrimSpace(observation.BuilderFamily)) {
			return fmt.Errorf("worktree pool recovery: evidence %q lacks an exact cross-family review binding", evidence.Path)
		}
	}
	return nil
}

func (p *Pool) verifyCompletedRecovery(ctx context.Context, req ReviewPoolRecoveryRequest, v *recoveryValidation, probes ReviewPoolRecoveryProbes) error {
	for _, slot := range v.slots {
		if slot.live.LeaseID != "" || slot.live.Purpose != "" {
			return fmt.Errorf("worktree pool recovery: completed lease state drift for %s", slot.req.Name)
		}
		head, err := gitRevision(ctx, slot.path, "HEAD")
		if err != nil || head != slot.req.Base {
			return fmt.Errorf("worktree pool recovery: completed HEAD drift for %s: got %s err=%v", slot.req.Name, head, err)
		}
		ref := SalvageRefFor("review/" + req.TaskRef + "/" + slot.req.CandidateSHA)
		if err := verifyGitRef(ctx, p.RepoRoot, ref, slot.req.CandidateSHA); err != nil {
			return err
		}
		if err := p.revalidateEvidence(ctx, req, slot.req, probes); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pool) recoverNested(ctx context.Context, req ReviewPoolRecoveryRequest, slot recoverySlotValidation, wm *WorktreeManager, probes ReviewPoolRecoveryProbes) error {
	registered, err := registeredDescendants(ctx, p.RepoRoot, slot.path)
	if err != nil {
		return err
	}
	expected := make(map[string]ReviewPoolRecoveryNested, len(slot.req.Nested))
	for _, nested := range slot.req.Nested {
		if err := requirePortableRelative(nested.Path); err != nil {
			return fmt.Errorf("worktree pool recovery: nested path: %w", err)
		}
		abs := filepath.Join(p.RepoRoot, filepath.FromSlash(nested.Path))
		if !pathWithin(slot.path, abs) || sameWorktreePath(slot.path, abs) {
			return fmt.Errorf("worktree pool recovery: nested path %q escapes exact slot", nested.Path)
		}
		key, err := filepath.EvalSymlinks(abs)
		if err != nil {
			// A started retry may have already removed the exact nested tree.
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("worktree pool recovery: resolve nested path: %w", err)
			}
			key = filepath.Clean(abs)
		}
		if _, dup := expected[key]; dup {
			return fmt.Errorf("worktree pool recovery: duplicate nested worktree %q", nested.Path)
		}
		expected[key] = nested
	}
	// Deepest first, because the live incident contained recursively registered
	// pools whose parents cannot be removed while their children remain.
	sort.Slice(registered, func(i, j int) bool {
		return strings.Count(registered[i], string(os.PathSeparator)) > strings.Count(registered[j], string(os.PathSeparator))
	})
	for _, path := range registered {
		nested, ok := expected[path]
		if !ok {
			return fmt.Errorf("worktree pool recovery: unexpected nested registered worktree %q", path)
		}
		open, err := probes.OpenFiles(ctx, path)
		if err != nil || len(open) != 0 {
			return fmt.Errorf("worktree pool recovery: nested open files/cwd unknown or live for %q: holders=%v err=%v", nested.Path, open, err)
		}
		clean, err := gitClean(ctx, p.RepoRoot, path)
		if err != nil || !clean {
			return fmt.Errorf("worktree pool recovery: nested source dirty or unknown for %q: clean=%v err=%v", nested.Path, clean, err)
		}
		head, err := gitRevision(ctx, path, "HEAD")
		if err != nil || head != nested.Head {
			return fmt.Errorf("worktree pool recovery: nested HEAD drift for %q: got=%s err=%v", nested.Path, head, err)
		}
		if nested.CandidateSHA != "" {
			ref := SalvageRefFor("review/" + req.TaskRef + "/nested/" + nested.CandidateSHA)
			if err := wm.ensureSalvageRef(ctx, ref, nested.CandidateSHA); err != nil {
				return err
			}
		}
		if err := p.preserveNestedAuthority(req.TransactionID, nested); err != nil {
			return err
		}
		if err := wm.RemovePoolWorktreeSafely(ctx, path); err != nil {
			return fmt.Errorf("worktree pool recovery: guarded nested remove %q: %w", nested.Path, err)
		}
	}
	return nil
}

func (p *Pool) preserveNestedAuthority(transactionID string, nested ReviewPoolRecoveryNested) error {
	for _, authority := range nested.Authority {
		if err := validateEvidenceBinding(authority); err != nil {
			return err
		}
		if !strings.Contains(filepath.ToSlash(authority.Path), "/.herd/") && !strings.HasPrefix(filepath.ToSlash(authority.Path), ".herd/") {
			return fmt.Errorf("worktree pool recovery: nested authority path %q is not .herd authority", authority.Path)
		}
		source := filepath.Join(p.RepoRoot, filepath.FromSlash(authority.Path))
		got, err := fileSHA256(source)
		if err != nil || got != strings.ToLower(authority.SHA256) {
			return fmt.Errorf("worktree pool recovery: nested authority drift for %q: got=%s err=%v", authority.Path, got, err)
		}
		destination := filepath.Join(p.RepoRoot, ".herd", "recovery", "review-pool", transactionID, "authority", filepath.FromSlash(authority.Path))
		if err := copyFileDurable(source, destination); err != nil {
			return err
		}
		if copied, err := fileSHA256(destination); err != nil || copied != got {
			return fmt.Errorf("worktree pool recovery: nested authority readback failed for %q: got=%s err=%v", authority.Path, copied, err)
		}
	}
	return nil
}

func registeredDescendants(ctx context.Context, repoRoot, parent string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "list", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("worktree pool recovery: list registered worktrees: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, fmt.Errorf("worktree pool recovery: resolve registered-worktree parent: %w", err)
	}
	var descendants []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		path := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		resolved, rerr := filepath.EvalSymlinks(path)
		if rerr != nil {
			return nil, fmt.Errorf("worktree pool recovery: registered worktree %q unresolved: %w", path, rerr)
		}
		if pathWithin(resolvedParent, resolved) && !sameWorktreePath(resolvedParent, resolved) {
			descendants = append(descendants, resolved)
		}
	}
	return descendants, nil
}

func recoveryRequestDigest(req ReviewPoolRecoveryRequest) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("worktree pool recovery: encode request: %w", err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func readRecoveryJournal(path string) ([]reviewPoolRecoveryJournal, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worktree pool recovery: read journal: %w", err)
	}
	defer f.Close()
	var entries []reviewPoolRecoveryJournal
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry reviewPoolRecoveryJournal
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, fmt.Errorf("worktree pool recovery: partial/corrupt journal: %w", err)
		}
		if entry.TransactionID == "" || entry.RequestDigest == "" || (entry.Phase != "started" && entry.Phase != "complete") {
			return nil, errors.New("worktree pool recovery: malformed journal entry")
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("worktree pool recovery: scan journal: %w", err)
	}
	return entries, nil
}

func appendRecoveryJournal(path string, entry reviewPoolRecoveryJournal) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("worktree pool recovery: create journal dir: %w", err)
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("worktree pool recovery: open journal: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("worktree pool recovery: append journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("worktree pool recovery: sync journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("worktree pool recovery: sync journal directory: %w", err)
	}
	entries, err := readRecoveryJournal(path)
	if err != nil || len(entries) == 0 {
		return fmt.Errorf("worktree pool recovery: journal readback failed: %w", err)
	}
	last := entries[len(entries)-1]
	if last.TransactionID != entry.TransactionID || last.RequestDigest != entry.RequestDigest || last.Phase != entry.Phase {
		return errors.New("worktree pool recovery: journal exact readback mismatch")
	}
	return nil
}

func compareProbe(ctx context.Context, name, want string, probe func(context.Context) (string, error)) error {
	got, err := probe(ctx)
	if err != nil {
		return fmt.Errorf("worktree pool recovery: %s identity unknown: %w", name, err)
	}
	if strings.TrimSpace(got) != strings.TrimSpace(want) {
		return fmt.Errorf("worktree pool recovery: wrong %s: got %q want %q", name, got, want)
	}
	return nil
}

func validateEvidenceBinding(e ReviewPoolRecoveryEvidence) error {
	if err := requirePortableRelative(e.Path); err != nil {
		return fmt.Errorf("worktree pool recovery: evidence path: %w", err)
	}
	if len(e.SHA256) != 64 {
		return fmt.Errorf("worktree pool recovery: evidence %q requires full sha256", e.Path)
	}
	if _, err := hex.DecodeString(e.SHA256); err != nil {
		return fmt.Errorf("worktree pool recovery: evidence %q invalid sha256", e.Path)
	}
	return nil
}

func requirePortableRelative(path string) error {
	path = strings.TrimSpace(path)
	if path == "" || filepath.IsAbs(path) {
		return fmt.Errorf("portable repository-relative path required: %q", path)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path escapes repository: %q", path)
	}
	return nil
}

func safeRecoveryID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func sameExistingPath(a, b string) (bool, error) {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false, err
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false, err
	}
	aa, err := filepath.Abs(ra)
	if err != nil {
		return false, err
	}
	ab, err := filepath.Abs(rb)
	if err != nil {
		return false, err
	}
	return filepath.Clean(aa) == filepath.Clean(ab), nil
}

func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func gitRevision(ctx context.Context, dir, ref string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", ref+"^{commit}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("worktree pool recovery: resolve %s in %s: %v (%s)", ref, dir, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func verifyCommit(ctx context.Context, repoRoot, sha string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "cat-file", "-e", sha+"^{commit}")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("worktree pool recovery: candidate/base %s is unreachable: %v (%s)", sha, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func verifyGitRef(ctx context.Context, repoRoot, ref, want string) error {
	got, err := gitRevision(ctx, repoRoot, ref)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("worktree pool recovery: preservation ref %s readback=%s want=%s", ref, got, want)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFileDurable(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := destination + ".tmp"
	if existing, err := fileSHA256(destination); err == nil {
		sourceDigest, sourceErr := fileSHA256(source)
		if sourceErr == nil && existing == sourceDigest {
			return nil
		}
		return fmt.Errorf("worktree pool recovery: authority destination %q already exists with different content", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("worktree pool recovery: partial authority write exists for %q", destination)
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("worktree pool recovery: sync authority directory: %w", err)
	}
	return nil
}
