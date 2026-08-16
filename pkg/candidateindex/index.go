package candidateindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

var sha40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// CandidateSource identifies where a candidate record originated.
type CandidateSource string

const (
	SourceProviderTask CandidateSource = "provider_task"
	SourceReviewMail   CandidateSource = "review_mail"
	SourceReviewLedger CandidateSource = "review_ledger"
	SourceReviewInbox  CandidateSource = "review_inbox"
	SourceWorktree     CandidateSource = "worktree"
)

// CandidateState represents the evaluated state of a candidate in the review index.
type CandidateState string

const (
	StateEligible CandidateState = "eligible"
	StateBlocked  CandidateState = "blocked"
	StateConsumed CandidateState = "consumed"
	StatePending  CandidateState = "pending"
)

// BlockedReason represents structured reasons why a candidate is blocked from admission.
type BlockedReason string

const (
	BlockedMissingCandidateSHA  BlockedReason = "missing_candidate_sha"
	BlockedMalformedSHA         BlockedReason = "malformed_sha"
	BlockedMissingTaskRef       BlockedReason = "missing_task_ref"
	BlockedStaleCandidate       BlockedReason = "stale_candidate"
	BlockedUnpublishedCandidate BlockedReason = "unpublished_candidate"
	BlockedMalformedEvidence    BlockedReason = "malformed_evidence"
	BlockedVetoVerdict          BlockedReason = "veto_verdict"
	BlockedNoPassingVerdict     BlockedReason = "no_passing_verdict"
	BlockedAlreadyConsumed      BlockedReason = "already_consumed"
	BlockedMissingWorktree      BlockedReason = "missing_worktree"
	BlockedMissingReceipt       BlockedReason = "missing_receipt"
)

// Candidate represents an unintegrated or active review candidate.
type Candidate struct {
	Ref             string            `json:"ref"`
	TaskID          string            `json:"task_id,omitempty"`
	Title           string            `json:"title,omitempty"`
	Priority        provider.Priority `json:"priority"`
	CandidateSHA    string            `json:"candidate_sha"`
	BaseSHA         string            `json:"base_sha,omitempty"`
	Sources         []CandidateSource `json:"sources"`
	State           CandidateState    `json:"state"`
	BlockedReasons  []BlockedReason   `json:"blocked_reasons,omitempty"`
	BlockedEvidence []string          `json:"blocked_evidence,omitempty"`
	Verdict         string            `json:"verdict,omitempty"`
	Reviewer        string            `json:"reviewer,omitempty"`
	ReviewerFamily  string            `json:"reviewer_family,omitempty"`
	AuthorFamily    string            `json:"author_family,omitempty"`
	LeaseGeneration int64             `json:"lease_generation,omitempty"`
	WorktreePath    string            `json:"worktree_path,omitempty"`
	UpdatedAt       time.Time         `json:"updated_at,omitempty"`
}

// IndexOptions configures candidate discovery and indexing.
type IndexOptions struct {
	RepoRoot     string
	Config       *config.Config
	TaskProvider provider.TaskProvider
	MailPath     string
	LedgerPath   string
	QueuePath    string
	InboxDir     string
	WorktreesDir string
	Now          func() time.Time
}

// CandidateIndex builds and serves a deterministic read-only view of review candidates.
type CandidateIndex struct {
	opts IndexOptions
}

type candidateKey struct {
	ref string
	sha string
}

type candidateEvidence struct {
	observedAt time.Time
	source     CandidateSource
	sequence   int64
	ordinal    int
}

// New creates a new CandidateIndex.
func New(opts IndexOptions) *CandidateIndex {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MailPath == "" && opts.RepoRoot != "" {
		opts.MailPath = filepath.Join(opts.RepoRoot, mail.DefaultMailFile)
	}
	if opts.LedgerPath == "" && opts.RepoRoot != "" {
		opts.LedgerPath = filepath.Join(opts.RepoRoot, ".herd", "review", "ledger.jsonl")
	}
	if opts.QueuePath == "" && opts.RepoRoot != "" {
		opts.QueuePath = filepath.Join(opts.RepoRoot, ".herd", "review", "queue.jsonl")
	}
	if opts.InboxDir == "" && opts.RepoRoot != "" {
		opts.InboxDir = filepath.Join(opts.RepoRoot, ".herd", "review", "inbox")
	}
	if opts.WorktreesDir == "" && opts.RepoRoot != "" {
		opts.WorktreesDir = filepath.Join(opts.RepoRoot, ".herd", "worktrees")
	}
	return &CandidateIndex{opts: opts}
}

// PriorityRank maps task priority to numeric rank for comparison.
func PriorityRank(p provider.Priority) int {
	switch p {
	case provider.PriorityUrgent:
		return 4
	case provider.PriorityHigh:
		return 3
	case provider.PriorityMedium:
		return 2
	case provider.PriorityLow:
		return 1
	default:
		return 0
	}
}

// SortCandidates sorts a slice of Candidates deterministically: Priority DESC, Ref ASC, CandidateSHA ASC.
func SortCandidates(cands []*Candidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		pi := PriorityRank(cands[i].Priority)
		pj := PriorityRank(cands[j].Priority)
		if pi != pj {
			return pi > pj
		}
		cmp := provider.CompareRefs(cands[i].Ref, cands[j].Ref)
		if cmp != 0 {
			return cmp < 0
		}
		return cands[i].CandidateSHA < cands[j].CandidateSHA
	})
}

// BuildIndex scans provider tasks, review mailboxes, review ledger, verdict inbox, and worktree HEADs,
// deduplicates records by (TaskRef, CandidateSHA), validates exact SHA and evidence integrity,
// and returns deterministically sorted candidates.
func (idx *CandidateIndex) BuildIndex(ctx context.Context) ([]*Candidate, error) {
	merged := make(map[candidateKey]*Candidate)
	sourceMap := make(map[candidateKey]map[CandidateSource]bool)
	evidence := make(map[candidateKey]candidateEvidence)
	type callbackBlock struct {
		sequence int64
		lease    int64
	}
	callbackBlocks := make(map[candidateKey]callbackBlock)
	var evidenceOrdinal int

	addSource := func(k candidateKey, src CandidateSource) {
		if sourceMap[k] == nil {
			sourceMap[k] = make(map[CandidateSource]bool)
		}
		sourceMap[k][src] = true
	}

	getOrCreate := func(ref, sha string, priority provider.Priority) *Candidate {
		k := candidateKey{ref: strings.TrimSpace(ref), sha: strings.TrimSpace(sha)}
		c, ok := merged[k]
		if !ok {
			c = &Candidate{
				Ref:          k.ref,
				CandidateSHA: k.sha,
				Priority:     priority,
				State:        StatePending,
			}
			merged[k] = c
		} else if c.Priority == "" && priority != "" {
			c.Priority = priority
		} else if PriorityRank(priority) > PriorityRank(c.Priority) {
			c.Priority = priority
		}
		return c
	}
	observe := func(ref, sha string, source CandidateSource, observedAt time.Time, sequence int64) {
		key := candidateKey{ref: strings.TrimSpace(ref), sha: strings.TrimSpace(sha)}
		evidenceOrdinal++
		candidate := candidateEvidence{observedAt: observedAt, source: source, sequence: sequence, ordinal: evidenceOrdinal}
		if previous, ok := evidence[key]; !ok || newerEvidence(candidate, previous) {
			evidence[key] = candidate
		}
	}

	// 1. Scan Provider Tasks in "in-progress" status
	if idx.opts.TaskProvider != nil && idx.opts.Config != nil && idx.opts.Config.TaskProvider.ProjectID != "" {
		tasks, err := idx.opts.TaskProvider.ListTasks(ctx, idx.opts.Config.TaskProvider.ProjectID, "in-progress")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("list in-progress tasks: %w", err)
		}
		for _, t := range tasks {
			ref := t.Ref
			if ref == "" {
				continue
			}
			k := candidateKey{ref: ref, sha: ""}
			c := getOrCreate(ref, "", t.Priority)
			c.TaskID = t.ID
			c.Title = t.Title
			c.UpdatedAt = t.UpdatedAt
			addSource(k, SourceProviderTask)
			observe(ref, "", SourceProviderTask, t.UpdatedAt, 0)
		}
	}

	// 2. Scan Review Mail / Callbacks
	if idx.opts.MailPath != "" {
		if mb := mail.NewMailbox(idx.opts.MailPath); mb != nil {
			envelopes, err := mb.ReadInbox("coordinator")
			if err == nil {
				for _, env := range envelopes {
					var cb mail.Callback
					if err := json.Unmarshal([]byte(env.Body), &cb); err == nil && cb.Ref != "" {
						c := getOrCreate(cb.Ref, cb.SHA, provider.PriorityMedium)
						key := candidateKey{ref: strings.TrimSpace(cb.Ref), sha: strings.TrimSpace(cb.SHA)}
						if cb.LeaseGeneration > c.LeaseGeneration {
							c.LeaseGeneration = cb.LeaseGeneration
						}
						if cb.Kind == mail.CallbackBlocked {
							c.State = StateBlocked
							c.BlockedReasons = append(c.BlockedReasons, BlockedVetoVerdict)
							c.BlockedEvidence = append(c.BlockedEvidence, fmt.Sprintf("callback blocked: %s", cb.Detail))
							callbackBlocks[key] = callbackBlock{sequence: env.Sequence, lease: cb.LeaseGeneration}
						} else if cb.Kind == mail.CallbackComplete {
							if block, ok := callbackBlocks[key]; ok && env.Sequence > block.sequence && cb.LeaseGeneration == block.lease {
								c.State = StatePending
								c.BlockedReasons = removeBlockedReason(c.BlockedReasons, BlockedVetoVerdict)
								c.BlockedEvidence = removeCallbackBlockedEvidence(c.BlockedEvidence)
								delete(callbackBlocks, key)
							}
						}
						addSource(key, SourceReviewMail)
						observe(cb.Ref, cb.SHA, SourceReviewMail, env.Timestamp, env.Sequence)
					}
				}
			}
		}
	}

	// 3. Scan Review Ledger & Queue
	if idx.opts.LedgerPath != "" {
		ledger := &reviewledger.Ledger{
			Path:      idx.opts.LedgerPath,
			QueuePath: idx.opts.QueuePath,
		}
		rows, err := ledger.AllRows()
		if err == nil {
			qrows, _ := ledger.QueueRows()
			consumed := make(map[string]bool)
			for _, qr := range qrows {
				if qr.Event == string(reviewledger.EventConsumed) {
					consumed[qr.SHA] = true
				}
			}

			for rowIndex, r := range rows {
				ref := r.Task
				sha := r.SHA
				if sha == "" {
					sha = r.CandidateSHA
				}
				if ref == "" && sha == "" {
					continue
				}
				c := getOrCreate(ref, sha, provider.PriorityMedium)
				if r.Reviewer != "" {
					c.Reviewer = r.Reviewer
				}
				if r.ReviewerFamily != "" {
					c.ReviewerFamily = r.ReviewerFamily
				}
				if r.BuilderFamily != "" {
					c.AuthorFamily = r.BuilderFamily
				}
				if r.Verdict != "" {
					c.Verdict = r.Verdict
				}
				if consumed[sha] {
					c.State = StateConsumed
				}
				addSource(candidateKey{ref: ref, sha: sha}, SourceReviewLedger)
				observedAt, _ := time.Parse(time.RFC3339Nano, r.Timestamp)
				observe(ref, sha, SourceReviewLedger, observedAt, int64(rowIndex))
			}
		}
	}

	// 4. Scan Review Inbox for raw verdict artifacts
	if idx.opts.InboxDir != "" {
		if entries, err := os.ReadDir(idx.opts.InboxDir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
					continue
				}
				p := filepath.Join(idx.opts.InboxDir, entry.Name())
				data, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				art := reviewingest.Parse(string(data))
				var observedAt time.Time
				if info, statErr := entry.Info(); statErr == nil {
					observedAt = info.ModTime()
				}
				if art.MalformedHeaderRegion || len(art.ConflictingHeaders) > 0 {
					c := getOrCreate(entry.Name(), art.SHA, provider.PriorityMedium)
					c.State = StateBlocked
					c.BlockedReasons = append(c.BlockedReasons, BlockedMalformedEvidence)
					c.BlockedEvidence = append(c.BlockedEvidence, fmt.Sprintf("inbox artifact %s has malformed/conflicting headers", entry.Name()))
					addSource(candidateKey{ref: entry.Name(), sha: art.SHA}, SourceReviewInbox)
					observe(entry.Name(), art.SHA, SourceReviewInbox, observedAt, 0)
					continue
				}
				if art.SHA != "" {
					c := getOrCreate(entry.Name(), art.SHA, provider.PriorityMedium)
					if art.Verdict != "" {
						c.Verdict = art.Verdict
					}
					if art.Reviewer != "" {
						c.Reviewer = art.Reviewer
					}
					if art.ReviewerFamily != "" {
						c.ReviewerFamily = art.ReviewerFamily
					}
					if art.BuilderFamily != "" {
						c.AuthorFamily = art.BuilderFamily
					}
					if valErr := art.Validate(nil, func(s string) bool { return true }); valErr != nil {
						c.State = StateBlocked
						c.BlockedReasons = append(c.BlockedReasons, BlockedMalformedEvidence)
						c.BlockedEvidence = append(c.BlockedEvidence, fmt.Sprintf("verdict validation failed: %v", valErr))
					}
					addSource(candidateKey{ref: entry.Name(), sha: art.SHA}, SourceReviewInbox)
					observe(entry.Name(), art.SHA, SourceReviewInbox, observedAt, 0)
				}
			}
		}
	}

	// 5. Coalesce by ref to link candidate SHAs across sources
	byRef := make(map[string][]*Candidate)
	for _, c := range merged {
		byRef[c.Ref] = append(byRef[c.Ref], c)
	}

	var candidates []*Candidate
	for ref, list := range byRef {
		var resolvedTitle, resolvedTaskID string
		var bestPriority provider.Priority
		srcSet := make(map[CandidateSource]bool)
		var worktreePath string

		// A callback can name an anchor or an otherwise stale candidate. The
		// task worktree is the live checkout used for review, so always add
		// its exact HEAD as evidence before coalescing. Previously this was
		// consulted only when no callback/ledger SHA existed, allowing a stale
		// callback to hide the current worktree candidate.
		if idx.opts.WorktreesDir != "" && ref != "" {
			wt := filepath.Join(idx.opts.WorktreesDir, strings.ToLower(hsync.NormalizeRef(ref)))
			if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
				worktreePath = wt
				if out, gerr := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output(); gerr == nil {
					head := strings.TrimSpace(string(out))
					if sha40Re.MatchString(head) {
						worktreeCandidate := getOrCreate(ref, head, provider.PriorityMedium)
						addSource(candidateKey{ref: ref, sha: head}, SourceWorktree)
						observedAt := time.Time{}
						if out, showErr := exec.Command("git", "-C", wt, "show", "-s", "--format=%cI", head).Output(); showErr == nil {
							observedAt, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
						}
						observe(ref, head, SourceWorktree, observedAt, 0)
						list = append(list, worktreeCandidate)
					}
				}
			}
		}

		selected := authoritativeCandidate(list, evidence)
		resolvedSHA := selected.CandidateSHA
		resolvedVerdict := selected.Verdict
		resolvedReviewer := selected.Reviewer
		resolvedReviewerFamily := selected.ReviewerFamily
		resolvedAuthorFamily := selected.AuthorFamily
		resolvedLease := selected.LeaseGeneration
		selectedBlockedReasons := append([]BlockedReason(nil), selected.BlockedReasons...)
		selectedBlockedEvidence := append([]string(nil), selected.BlockedEvidence...)
		hasBlockedState := selected.State == StateBlocked
		for _, item := range list {
			if item.Title != "" && resolvedTitle == "" {
				resolvedTitle = item.Title
			}
			if item.TaskID != "" && resolvedTaskID == "" {
				resolvedTaskID = item.TaskID
			}
			if PriorityRank(item.Priority) > PriorityRank(bestPriority) {
				bestPriority = item.Priority
			}
			k := candidateKey{ref: item.Ref, sha: item.CandidateSHA}
			for s := range sourceMap[k] {
				srcSet[s] = true
			}
		}

		// Evidence fields below are intentionally taken only from the
		// authoritative SHA. Mixing a verdict or block reason from an older
		// SHA with the newest candidate would break exact SHA fencing.

		c := &Candidate{
			Ref:             ref,
			CandidateSHA:    resolvedSHA,
			TaskID:          resolvedTaskID,
			Title:           resolvedTitle,
			Priority:        bestPriority,
			Verdict:         resolvedVerdict,
			Reviewer:        resolvedReviewer,
			ReviewerFamily:  resolvedReviewerFamily,
			AuthorFamily:    resolvedAuthorFamily,
			LeaseGeneration: resolvedLease,
			State:           StatePending,
			WorktreePath:    worktreePath,
			BlockedReasons:  selectedBlockedReasons,
			BlockedEvidence: selectedBlockedEvidence,
		}
		if hasBlockedState {
			c.State = StateBlocked
		}

		var srcs []CandidateSource
		for s := range srcSet {
			srcs = append(srcs, s)
		}
		sort.Slice(srcs, func(i, j int) bool { return srcs[i] < srcs[j] })
		c.Sources = srcs

		// Validate SHA format
		if c.CandidateSHA == "" {
			c.State = StateBlocked
			c.BlockedReasons = append(c.BlockedReasons, BlockedMissingCandidateSHA)
			c.BlockedEvidence = append(c.BlockedEvidence, fmt.Sprintf("task %s has no candidate SHA", ref))
		} else if !sha40Re.MatchString(c.CandidateSHA) {
			c.State = StateBlocked
			c.BlockedReasons = append(c.BlockedReasons, BlockedMalformedSHA)
			c.BlockedEvidence = append(c.BlockedEvidence, fmt.Sprintf("candidate SHA %q is not 40 hex chars", c.CandidateSHA))
		}

		if c.State != StateBlocked && c.State != StateConsumed {
			if c.Verdict == string(reviewledger.VerdictFAIL) || c.Verdict == string(reviewledger.VerdictBLOCKED) {
				c.State = StateBlocked
				c.BlockedReasons = append(c.BlockedReasons, BlockedVetoVerdict)
			} else if c.Verdict == string(reviewledger.VerdictPASS) {
				c.State = StateEligible
			}
		}

		c.BlockedReasons = dedupReasons(c.BlockedReasons)
		candidates = append(candidates, c)
	}

	SortCandidates(candidates)
	return candidates, nil
}

func dedupReasons(reasons []BlockedReason) []BlockedReason {
	seen := make(map[BlockedReason]bool)
	var out []BlockedReason
	for _, r := range reasons {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

func removeBlockedReason(reasons []BlockedReason, want BlockedReason) []BlockedReason {
	out := reasons[:0]
	for _, reason := range reasons {
		if reason != want {
			out = append(out, reason)
		}
	}
	return out
}

func removeCallbackBlockedEvidence(evidence []string) []string {
	out := evidence[:0]
	for _, item := range evidence {
		if !strings.HasPrefix(item, "callback blocked:") {
			out = append(out, item)
		}
	}
	return out
}

func evidenceSourceRank(source CandidateSource) int {
	switch source {
	case SourceWorktree:
		return 6
	case SourceReviewLedger:
		return 5
	case SourceReviewMail:
		return 4
	case SourceReviewInbox:
		return 2
	case SourceProviderTask:
		return 1
	default:
		return 0
	}
}

func newerEvidence(candidate, previous candidateEvidence) bool {
	if !candidate.observedAt.Equal(previous.observedAt) {
		return candidate.observedAt.After(previous.observedAt)
	}
	if evidenceSourceRank(candidate.source) != evidenceSourceRank(previous.source) {
		return evidenceSourceRank(candidate.source) > evidenceSourceRank(previous.source)
	}
	if candidate.sequence != previous.sequence {
		return candidate.sequence > previous.sequence
	}
	return candidate.ordinal > previous.ordinal
}

func authoritativeCandidate(list []*Candidate, evidence map[candidateKey]candidateEvidence) *Candidate {
	var selected *Candidate
	var selectedEvidence candidateEvidence
	for _, candidate := range list {
		key := candidateKey{ref: candidate.Ref, sha: candidate.CandidateSHA}
		current, ok := evidence[key]
		if !ok {
			current = candidateEvidence{}
		}
		if selected == nil || candidateIsNewer(candidate, current, selected, selectedEvidence) {
			selected = candidate
			selectedEvidence = current
		}
	}
	return selected
}

func candidateIsNewer(candidate *Candidate, current candidateEvidence, selected *Candidate, previous candidateEvidence) bool {
	if (candidate.CandidateSHA != "") != (selected.CandidateSHA != "") {
		return candidate.CandidateSHA != ""
	}
	if newerEvidence(current, previous) {
		return true
	}
	if newerEvidence(previous, current) {
		return false
	}
	return candidate.CandidateSHA < selected.CandidateSHA
}
