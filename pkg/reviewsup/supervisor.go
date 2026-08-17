package reviewsup

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/classify"
	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/router"
)

type EventType string

const (
	EventCompletion       EventType = "completion"
	EventReview           EventType = "review"
	EventVerdict          EventType = "verdict"
	EventSupersede        EventType = "supersede"
	EventEvict            EventType = "evict"
	EventHarvest          EventType = "harvest"
	EventCapacity         EventType = "capacity"
	EventBuilderCallback  EventType = "builder_callback"
	EventBuilderAck       EventType = "builder_ack"
	EventVerdictRetained  EventType = "verdict_retained"
	EventIngested         EventType = "ingested"
	EventAuthorNotified   EventType = "author_notified"
	EventCleanupCandidate EventType = "cleanup_candidate"
	EventClosed           EventType = "closed"
	EventRefutation       EventType = "refutation"
	EventLaunchFailed     EventType = "launch_failed"
	EventRouteBlocked     EventType = "route_blocked"
	EventDispatchBlocked  EventType = "dispatch_blocked"
)

type Verdict string

const (
	VerdictPASS    Verdict = "PASS"
	VerdictFAIL    Verdict = "FAIL"
	VerdictBLOCKED Verdict = "BLOCKED"
)

type RiskTier string

const (
	TierR0 RiskTier = "R0"
	TierR1 RiskTier = "R1"
	TierR2 RiskTier = "R2"
	TierR3 RiskTier = "R3"
)

type Row struct {
	Timestamp     string `json:"ts"`
	Event         string `json:"event"`
	SHA           string `json:"sha"`
	Branch        string `json:"branch,omitempty"`
	PatchID       string `json:"patch_id,omitempty"`
	AuthorModel   string `json:"author_model,omitempty"`
	AuthorFamily  string `json:"author_family,omitempty"`
	Reviewer      string `json:"reviewer,omitempty"`
	ReviewFamily  string `json:"review_family,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Harness       string `json:"harness,omitempty"`
	Tier          string `json:"tier,omitempty"`
	Verdict       string `json:"verdict,omitempty"`
	Reason        string `json:"reason,omitempty"`
	PrevSHA       string `json:"prev_sha,omitempty"`
	Harvested     bool   `json:"harvested,omitempty"`
	Capacity      int    `json:"capacity,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	IngestedAt    string `json:"ingested_at,omitempty"`
	LeaseID       string `json:"lease_id,omitempty"`
	Generation    int64  `json:"generation,omitempty"`
	ReceiptDigest string `json:"receipt_digest,omitempty"`
	// Artifact is the repo-relative durable review-inbox path retained before
	// cleanup-candidate admission (FAC-373).
	Artifact string `json:"artifact,omitempty"`
}

type CandidateState string

const (
	StatePending   CandidateState = "pending"
	StateReviewing CandidateState = "reviewing"
	StatePass      CandidateState = "pass"
	StateFail      CandidateState = "fail"
	StateBlocked   CandidateState = "blocked"
	StateHarvested CandidateState = "harvested"
	StateEvicted   CandidateState = "evicted"
)

// QueueState is the durable supervisor-facing lifecycle. CandidateState is
// retained for compatibility with the older API; QueueState makes the
// operator contract explicit and gives pulse/batch callers exact references.
type QueueState string

const (
	QueueAdmitted         QueueState = "admitted"
	QueueLaunched         QueueState = "launched"
	QueueVerdictRetained  QueueState = "verdict-retained"
	QueueIngested         QueueState = "ingested"
	QueueAuthorNotified   QueueState = "author-notified"
	QueueHarvestReady     QueueState = "harvest-ready"
	QueueCleanupCandidate QueueState = "cleanup-candidate"
	QueueBlocked          QueueState = "blocked"
	QueueClosed           QueueState = "closed"
)

type QueueEntry struct {
	SHA       string     `json:"sha"`
	Branch    string     `json:"branch,omitempty"`
	State     QueueState `json:"state"`
	Reviewer  string     `json:"reviewer,omitempty"`
	Attempts  int        `json:"attempts,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type Candidate struct {
	SHA           string
	Branch        string
	PatchID       string
	AuthorModel   string
	AuthorFamily  string
	Tier          RiskTier
	State         CandidateState
	Verdict       Verdict
	VerdictReason string
	Reviewer      string
	ReviewFamily  string
	Provider      string
	Harness       string
	BlockedReason string
	Attempts      int
	IngestedAt    time.Time
	UpdatedAt     time.Time
	LeaseID       string
	LeaseExpiry   time.Time
	Generation    int64
	// ReceiptDigest is the FAC-122 verification receipt digest that
	// admitted this candidate. LaunchReview re-checks it; empty digest
	// never enters review.
	ReceiptDigest    string
	// Artifact is the durable .herd/review/inbox path proven before a PASS
	// may become cleanup-candidate (FAC-373).
	Artifact         string
	VerdictRetained  bool
	Ingested         bool
	HarvestReady     bool
	CleanupCandidate bool
	DispatchBlocked  bool
	DispatchReason   string
}

func queueState(c *Candidate) QueueState {
	if c == nil {
		return QueueClosed
	}
	if c.State == StateEvicted {
		return QueueClosed
	}
	if c.CleanupCandidate {
		return QueueCleanupCandidate
	}
	if c.DispatchBlocked {
		return QueueBlocked
	}
	if c.HarvestReady {
		return QueueHarvestReady
	}
	if c.Ingested {
		return QueueIngested
	}
	if c.VerdictRetained {
		return QueueVerdictRetained
	}
	switch c.State {
	case StatePending:
		if c.Verdict == VerdictFAIL {
			return QueueAuthorNotified
		}
		return QueueAdmitted
	case StateReviewing:
		return QueueLaunched
	case StatePass:
		return QueueVerdictRetained
	case StateFail, StateBlocked:
		return QueueAuthorNotified
	case StateHarvested:
		return QueueHarvestReady
	case StateEvicted:
		return QueueClosed
	default:
		return QueueAdmitted
	}
}

// QueueSnapshot returns a deterministic exact-SHA view of every active review
// candidate. It is intentionally read-only and independent of fleet-feedback
// census, so a supervisor can batch-dispatch independent stacks while one
// blocked stack remains unresolved.
func (sv *ReviewSupervisor) QueueSnapshot() []QueueEntry {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	out := make([]QueueEntry, 0, len(sv.cands))
	for _, c := range sv.cands {
		if c == nil {
			continue
		}
		reason := c.DispatchReason
		if reason == "" {
			reason = c.BlockedReason
		}
		out = append(out, QueueEntry{SHA: c.SHA, Branch: c.Branch, State: queueState(c), Reviewer: c.Reviewer, Attempts: c.Attempts, Reason: reason, UpdatedAt: c.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SHA < out[j].SHA })
	return out
}

// WatchdogAlert identifies an in-review candidate that has outlived the
// supervisor's bounded dispatch window. Review progress must not depend on a
// fleet-feedback census; callers can use this read-only signal to re-dispatch
// a reviewer or page the coordinator.
type WatchdogAlert struct {
	SHA      string
	Reviewer string
	Age      time.Duration
	Reason   string
}

type ModelFamily string

const (
	FamilyLazer  ModelFamily = "lazer"
	FamilyAnt    ModelFamily = "anthropic"
	FamilyGoogle ModelFamily = "google"
	FamilyOpenAI ModelFamily = "openai"
	FamilyGrok   ModelFamily = "grok"
	FamilyKimi   ModelFamily = "kimi"
	FamilyCodex  ModelFamily = "codex"
	FamilyOther  ModelFamily = "other"
)

func lookupFamily(model string) ModelFamily {
	l := strings.ToLower(model)
	for prefix, f := range map[string]ModelFamily{
		"lazer": FamilyLazer, "deepseek": FamilyLazer,
		"claude": FamilyAnt, "sonnet": FamilyAnt, "opus": FamilyAnt, "haiku": FamilyAnt, "anthropic": FamilyAnt,
		"gemini": FamilyGoogle, "google": FamilyGoogle, "agy": FamilyGoogle,
		"gpt": FamilyOpenAI, "o1": FamilyOpenAI, "o3": FamilyOpenAI, "openai": FamilyOpenAI,
		"grok": FamilyGrok, "xai": FamilyGrok,
		"kimi": FamilyKimi, "moonshot": FamilyKimi,
		"codex": FamilyCodex,
	} {
		if strings.Contains(l, prefix) {
			return f
		}
	}
	return FamilyOther
}

func CrossFamilyOK(authorFamily, reviewFamily ModelFamily) bool {
	// Fail closed: an unrecognized model family can never satisfy the
	// cross-family requirement for R1-R3.
	if authorFamily == FamilyOther || reviewFamily == FamilyOther {
		return false
	}
	return authorFamily != reviewFamily
}

func RequireCrossFamily(tier RiskTier) bool {
	return tier == TierR1 || tier == TierR2 || tier == TierR3
}

// ReceiptAdmit is the FAC-144 review-spawn gate. Production composes it to
// ReceiptAdmission.RequireCurrentPassing (via daemon.CompletionGate). A nil
// admit refuses LaunchReview fail-closed — CheckCompletion is never enough.
type ReceiptAdmit func(ctx context.Context, worktreeDir, digest string) error

type Config struct {
	// RepoRoot is the git repository root used for durable review-inbox
	// retention (.herd/review/inbox). PASS cannot become cleanup-candidate
	// without a retained artifact under this root (FAC-373).
	RepoRoot          string
	LedgerPath        string
	QueuePath         string
	MaxPendingReviews int
	StaleDuration     time.Duration
	RetryLimit        int
	DispatchTimeout   time.Duration
	LeaseDuration     time.Duration
	Now               func() time.Time
	Orders            *control.CoordinatorOrders
	// AdmitReceipt is required for LaunchReview. When nil, review spawn is
	// refused so a miscomposed supervisor cannot bypass verification.
	AdmitReceipt ReceiptAdmit
	// WorktreeFor resolves the candidate worktree for AdmitReceipt. When
	// nil, AdmitReceipt is called with dir "".
	WorktreeFor func(candidateSHA string) (string, error)
}

func DefaultConfig(ledgerDir string) Config {
	return Config{
		RepoRoot:          ledgerDir,
		LedgerPath:        filepath.Join(ledgerDir, "supervisor-ledger.jsonl"),
		QueuePath:         filepath.Join(ledgerDir, "harvest-evidence-queue.jsonl"),
		MaxPendingReviews: 3,
		StaleDuration:     24 * time.Hour,
		RetryLimit:        3,
		DispatchTimeout:   30 * time.Second,
		LeaseDuration:     30 * time.Minute,
		Now:               time.Now,
	}
}

type ReviewSupervisor struct {
	mu sync.RWMutex

	cfg      Config
	cands    map[string]*Candidate
	shaIdx   map[string]string
	leaseGen map[string]int64
	evrows   []Row

	pendingCount int
}

func New(cfg Config) *ReviewSupervisor {
	dir := filepath.Dir(cfg.LedgerPath)
	os.MkdirAll(dir, 0755)

	sv := &ReviewSupervisor{
		cfg:      cfg,
		cands:    make(map[string]*Candidate),
		shaIdx:   make(map[string]string),
		leaseGen: make(map[string]int64),
	}
	return sv
}

func (sv *ReviewSupervisor) nowISO() string { return sv.cfg.Now().UTC().Format(time.RFC3339) }

func (sv *ReviewSupervisor) now() time.Time { return sv.cfg.Now().UTC() }

func (sv *ReviewSupervisor) appendRow(r *Row) error {
	r.Timestamp = sv.nowISO()
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal row: %w", err)
	}
	f, err := os.OpenFile(sv.cfg.LedgerPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write ledger: %w", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		return fmt.Errorf("write ledger newline: %w", err)
	}
	return nil
}

func (sv *ReviewSupervisor) appendQueue(r *Row) error {
	r.Timestamp = sv.nowISO()
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal queue row: %w", err)
	}
	f, err := os.OpenFile(sv.cfg.QueuePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open queue: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write queue: %w", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		return fmt.Errorf("write queue newline: %w", err)
	}
	return nil
}

func readRows(path string) ([]Row, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read rows open: %w", err)
	}
	defer f.Close()
	var rows []Row
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			// Fail closed: a corrupted evidence file must surface as a hard
			// error, never silently drop replayable events.
			return nil, fmt.Errorf("read rows %s: malformed JSON at line %d: %w", path, lineNo, err)
		}
		rows = append(rows, r)
	}
	return rows, sc.Err()
}

type CompletionCallback struct {
	SHA           string
	Branch        string
	PatchID       string
	AuthorModel   string
	Tier          RiskTier
	Files         []string
	LeaseID       string
	Generation    int64
	DirtyWorktree bool
	// ReceiptDigest is the FAC-122 VerifyAndPersist digest for this SHA.
	// Ingest refuses an empty digest so CheckCompletion-only callbacks
	// cannot enter the review queue (FAC-144).
	ReceiptDigest string
}

// DispatchRequest describes one independent candidate admission attempt. Each
// request gets its own timeout and retry budget; a slow route cannot hold up a
// different candidate.
type DispatchRequest struct {
	SHA         string
	Reviewers   []ReviewerEntry
	MaxAttempts int
	Timeout     time.Duration
	Launch      func(context.Context, string, ReviewerEntry) error
}

// DispatchResult is the durable outcome of one request. SHA is always the
// exact candidate identity that was dispatched or blocked.
type DispatchResult struct {
	SHA      string
	State    QueueState
	Reviewer string
	Attempts int
	Reason   string
}

func (sv *ReviewSupervisor) Ingest(cb CompletionCallback) (accepted bool, staleSHA string, err error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	if cb.SHA == "" {
		return false, "", fmt.Errorf("reviewsup: empty SHA in completion callback")
	}
	if strings.TrimSpace(cb.ReceiptDigest) == "" {
		return false, "", fmt.Errorf("reviewsup: completion callback for %s rejected: missing verification receipt digest", cb.SHA)
	}
	if !strings.HasPrefix(cb.ReceiptDigest, "sha256:") {
		return false, "", fmt.Errorf("reviewsup: completion callback for %s rejected: receipt digest must be sha256:…", cb.SHA)
	}

	// Dirty-worktree evidence can never be reviewed: the recorded SHA does
	// not describe the actual tree state. Fail closed.
	if cb.DirtyWorktree {
		return false, "", fmt.Errorf("reviewsup: completion callback for %s rejected: dirty worktree", cb.SHA)
	}

	if _, ok := sv.cands[cb.SHA]; ok {
		return false, "", nil
	}

	// Lease-generation verification: a callback carrying a lease must present
	// a strictly newer generation than any previously accepted callback for
	// that lease. Generation 0 means "unspecified" and skips ordering checks.
	if cb.LeaseID != "" && cb.Generation > 0 {
		if last, ok := sv.leaseGen[cb.LeaseID]; ok && cb.Generation <= last {
			return false, "", fmt.Errorf("reviewsup: stale lease generation for %s (lease=%s gen=%d last=%d)", cb.SHA, cb.LeaseID, cb.Generation, last)
		}
	}

	if cb.PatchID != "" {
		if prevSHA, ok := sv.shaIdx[cb.PatchID]; ok {
			if prevCand, ok := sv.cands[prevSHA]; ok {
				// Exact stale-SHA validation: when both commits carry
				// generation info, a superseding commit must be newer.
				if cb.Generation > 0 && prevCand.Generation > 0 && cb.Generation <= prevCand.Generation {
					return false, "", fmt.Errorf("reviewsup: stale SHA %s for patch %s (gen %d <= %d)", cb.SHA, cb.PatchID, cb.Generation, prevCand.Generation)
				}
				pState := prevCand.State
				pWasPending := (pState == StatePending || pState == StateReviewing || pState == StatePass)
				prevCand.State = StateEvicted
				prevCand.UpdatedAt = sv.now()
				if pWasPending {
					sv.pendingCount--
				}
				if err := sv.appendRow(&Row{
					Event:   string(EventSupersede),
					SHA:     cb.SHA,
					PrevSHA: prevSHA,
					PatchID: cb.PatchID,
					Reason:  "newer commit supersedes",
				}); err != nil {
					prevCand.State = pState
					if pWasPending {
						sv.pendingCount++
					}
					return false, "", fmt.Errorf("reviewsup: append supersede row: %w", err)
				}
				staleSHA = prevSHA
			}
		}
	}

	cand := &Candidate{
		SHA:           cb.SHA,
		Branch:        cb.Branch,
		PatchID:       cb.PatchID,
		AuthorModel:   cb.AuthorModel,
		AuthorFamily:  string(lookupFamily(cb.AuthorModel)),
		Tier:          cb.Tier,
		State:         StatePending,
		IngestedAt:    sv.now(),
		UpdatedAt:     sv.now(),
		LeaseID:       cb.LeaseID,
		Generation:    cb.Generation,
		ReceiptDigest: cb.ReceiptDigest,
	}

	if err := sv.appendRow(&Row{
		Event:         string(EventCompletion),
		SHA:           cb.SHA,
		Branch:        cb.Branch,
		PatchID:       cb.PatchID,
		AuthorModel:   cb.AuthorModel,
		AuthorFamily:  cand.AuthorFamily,
		Tier:          string(cb.Tier),
		IngestedAt:    sv.nowISO(),
		LeaseID:       cb.LeaseID,
		Generation:    cb.Generation,
		ReceiptDigest: cb.ReceiptDigest,
	}); err != nil {
		return false, "", fmt.Errorf("reviewsup: append completion row: %w", err)
	}

	sv.cands[cb.SHA] = cand
	if cb.PatchID != "" {
		sv.shaIdx[cb.PatchID] = cb.SHA
	}
	if cb.LeaseID != "" && cb.Generation > 0 {
		sv.leaseGen[cb.LeaseID] = cb.Generation
	}
	sv.pendingCount++

	return true, staleSHA, nil
}

type ReviewerEntry struct {
	Name          string
	Model         string
	Provider      string
	Harness       string
	Preferred     bool
	Unavailable   bool
	Refused       bool
	RefusalReason string
}

func (sv *ReviewSupervisor) SelectReviewer(candidateSHA string, pool []ReviewerEntry) (*ReviewerEntry, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	cand, ok := sv.cands[candidateSHA]
	if !ok {
		return nil, fmt.Errorf("reviewsup: unknown candidate %s", candidateSHA)
	}

	if cand.State != StatePending {
		return nil, fmt.Errorf("reviewsup: candidate %s is not pending (state=%s)", candidateSHA, cand.State)
	}

	authorFamily := lookupFamily(cand.AuthorModel)
	needsCross := RequireCrossFamily(cand.Tier)

	// An empty pool represents backpressure, not an exhausted route space.
	if len(pool) == 0 {
		return nil, nil
	}
	ordered := append([]ReviewerEntry(nil), pool...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Preferred != ordered[j].Preferred {
			return ordered[i].Preferred
		}
		li := strings.ToLower(ordered[i].Provider + "|" + ordered[i].Model + "|" + ordered[i].Name)
		lj := strings.ToLower(ordered[j].Provider + "|" + ordered[j].Model + "|" + ordered[j].Name)
		return li < lj
	})
	var reasons []string
	for _, r := range ordered {
		if r.Unavailable || r.Refused {
			reason := r.RefusalReason
			if reason == "" {
				reason = "route unavailable"
			}
			reasons = append(reasons, r.Name+": "+reason)
			continue
		}
		rFamily := lookupFamily(r.Model)
		if needsCross && !CrossFamilyOK(authorFamily, rFamily) {
			reasons = append(reasons, r.Name+": same-family or unknown-family route")
			continue
		}
		allowed, reason := router.ReviewRouteAdmission(r.Provider, r.Model, classify.Tier(cand.Tier))
		if !allowed {
			reasons = append(reasons, r.Name+": "+reason)
			continue
		}
		selected := r
		if selected.Provider == "" {
			selected.Provider = router.ReviewProviderForModel(r.Model)
		}
		if surface, ok := router.SurfaceFor(selected.Provider); ok {
			selected.Harness = surface.Harness
		}
		return &selected, nil
	}

	if err := sv.blockRouteLocked(cand, strings.Join(reasons, "; ")); err != nil {
		return nil, err
	}
	return nil, nil
}

// blockRouteLocked records one terminal route decision. It is idempotent so a
// retrying supervisor cannot wedge a pin with repeated refused attempts.
func (sv *ReviewSupervisor) blockRouteLocked(cand *Candidate, reason string) error {
	if cand.State == StateBlocked {
		return nil
	}
	if reason == "" {
		reason = "no compatible review route"
	}
	if cand.State == StatePending || cand.State == StateReviewing || cand.State == StatePass {
		sv.pendingCount--
	}
	cand.State = StateBlocked
	cand.Verdict = VerdictBLOCKED
	cand.BlockedReason = reason
	cand.UpdatedAt = sv.now()
	if err := sv.appendRow(&Row{Event: string(EventRouteBlocked), SHA: cand.SHA, Tier: string(cand.Tier), Verdict: string(VerdictBLOCKED), Reason: reason}); err != nil {
		return fmt.Errorf("reviewsup: persist blocked route for %s: %w", cand.SHA, err)
	}
	return nil
}

func (sv *ReviewSupervisor) LaunchReview(candidateSHA, reviewer, reviewModel string) error {
	return sv.LaunchReviewContext(context.Background(), candidateSHA, reviewer, reviewModel)
}

// LaunchReviewContext is the cancellable form used by bounded dispatch. The
// legacy LaunchReview API remains a background-context wrapper.
func (sv *ReviewSupervisor) LaunchReviewContext(ctx context.Context, candidateSHA, reviewer, reviewModel string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	sv.mu.Lock()
	cand, ok := sv.cands[candidateSHA]
	if !ok {
		sv.mu.Unlock()
		return fmt.Errorf("reviewsup: unknown candidate %s", candidateSHA)
	}
	if cand.State == StateReviewing && strings.TrimSpace(cand.Reviewer) == strings.TrimSpace(reviewer) {
		// Idempotent exact-SHA + reviewer admission. A supervisor retry must
		// reuse the existing reviewer heartbeat/pane rather than launch a
		// duplicate reviewer for the same candidate identity.
		sv.mu.Unlock()
		return nil
	}
	if cand.State != StatePending {
		sv.mu.Unlock()
		return fmt.Errorf("reviewsup: candidate %s is not pending (state=%s)", candidateSHA, cand.State)
	}
	if allowed, reason := router.ReviewRouteAdmission(router.ReviewProviderForModel(reviewModel), reviewModel, classify.Tier(cand.Tier)); !allowed {
		blockErr := sv.blockRouteLocked(cand, reason)
		sv.mu.Unlock()
		if blockErr != nil {
			return blockErr
		}
		return fmt.Errorf("reviewsup: review route refused for %s: %s", candidateSHA, reason)
	}
	if strings.TrimSpace(cand.ReceiptDigest) == "" {
		sv.mu.Unlock()
		return fmt.Errorf("reviewsup: candidate %s has no verification receipt digest", candidateSHA)
	}
	// FAC-144: RequireCurrentPassing (or equivalent) before review spawn.
	// A nil admit is a miscomposition, not a free pass.
	if sv.cfg.AdmitReceipt == nil {
		sv.mu.Unlock()
		return fmt.Errorf("reviewsup: receipt admission is not configured — refusing to spawn review for %s", candidateSHA)
	}
	receiptDigest := cand.ReceiptDigest
	authorModel := cand.AuthorModel
	tier := cand.Tier
	admitReceipt := sv.cfg.AdmitReceipt
	worktreeFor := sv.cfg.WorktreeFor
	dir := ""
	sv.mu.Unlock()
	if worktreeFor != nil {
		var err error
		dir, err = worktreeFor(candidateSHA)
		if err != nil {
			return fmt.Errorf("reviewsup: resolve worktree for %s: %w", candidateSHA, err)
		}
	}
	if err := admitReceipt(ctx, dir, receiptDigest); err != nil {
		return fmt.Errorf("reviewsup: receipt admission refused for %s: %w", candidateSHA, err)
	}

	reviewFamily := lookupFamily(reviewModel)
	authorFamily := lookupFamily(authorModel)
	needsCross := RequireCrossFamily(tier)

	if needsCross && !CrossFamilyOK(authorFamily, reviewFamily) {
		return fmt.Errorf("reviewsup: candidate %s requires cross-family review (author=%s, reviewer=%s)", candidateSHA, authorFamily, reviewFamily)
	}

	sv.mu.Lock()
	defer sv.mu.Unlock()
	// The candidate may have been superseded while the receipt admission was
	// running. Never attach a reviewer lease to a stale SHA.
	cand, ok = sv.cands[candidateSHA]
	if !ok || cand.State != StatePending || cand.ReceiptDigest != receiptDigest {
		return fmt.Errorf("reviewsup: candidate %s changed while admission was in flight", candidateSHA)
	}
	cand.State = StateReviewing
	cand.Reviewer = reviewer
	cand.ReviewFamily = string(reviewFamily)
	cand.Provider = router.ReviewProviderForModel(reviewModel)
	if surface, ok := router.SurfaceFor(cand.Provider); ok {
		cand.Harness = surface.Harness
	}
	cand.UpdatedAt = sv.now()
	cand.Attempts++
	if err := sv.appendRow(&Row{
		Event:         string(EventReview),
		SHA:           candidateSHA,
		Reviewer:      reviewer,
		ReviewFamily:  string(reviewFamily),
		Provider:      cand.Provider,
		Harness:       cand.Harness,
		Tier:          string(cand.Tier),
		Attempts:      cand.Attempts,
		ReceiptDigest: cand.ReceiptDigest,
	}); err != nil {
		cand.State = StatePending
		cand.Reviewer = ""
		cand.ReviewFamily = ""
		cand.Provider = ""
		cand.Harness = ""
		cand.Attempts--
		cand.UpdatedAt = sv.now()
		return fmt.Errorf("reviewsup: append review row: %w", err)
	}
	if sv.cfg.Orders != nil {
		if _, err := sv.cfg.Orders.ReviewCorrection(ctx, fmt.Sprintf("review candidate %s by %s", candidateSHA, reviewer)); err != nil {
			cand.State = StatePending
			cand.Reviewer = ""
			cand.ReviewFamily = ""
			cand.Provider = ""
			cand.Harness = ""
			cand.Attempts--
			cand.UpdatedAt = sv.now()
			return fmt.Errorf("reviewsup: durable review correction order: %w", err)
		}
	}

	return nil
}

// CompensateLaunch reverts a reviewing candidate after a bounded native
// launch failure. The card must not remain in-review without a live reviewer.
func (sv *ReviewSupervisor) CompensateLaunch(candidateSHA, reason string) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	cand, ok := sv.cands[candidateSHA]
	if !ok {
		return fmt.Errorf("reviewsup: unknown candidate %s", candidateSHA)
	}
	if cand.State != StateReviewing {
		return fmt.Errorf("reviewsup: candidate %s is not reviewing (state=%s)", candidateSHA, cand.State)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "LAUNCH_FAILED"
	}
	prevReviewer := cand.Reviewer
	prevFamily := cand.ReviewFamily
	cand.State = StatePending
	cand.Reviewer = ""
	cand.ReviewFamily = ""
	cand.UpdatedAt = sv.now()
	if err := sv.appendRow(&Row{
		Event:    string(EventLaunchFailed),
		SHA:      candidateSHA,
		Reviewer: prevReviewer,
		Reason:   reason,
		Attempts: cand.Attempts,
	}); err != nil {
		cand.State = StateReviewing
		cand.Reviewer = prevReviewer
		cand.ReviewFamily = prevFamily
		cand.UpdatedAt = sv.now()
		return fmt.Errorf("reviewsup: append launch_failed row: %w", err)
	}
	return nil
}

// Dispatch admits candidates independently. Each request runs in its own
// bounded route call, and results are sorted by exact SHA for deterministic
// queue reporting. Exhausted routes receive a durable blocked record.
func (sv *ReviewSupervisor) Dispatch(ctx context.Context, requests []DispatchRequest) []DispatchResult {
	if ctx == nil {
		ctx = context.Background()
	}
	results := make(chan DispatchResult, len(requests))
	var wg sync.WaitGroup
	for _, request := range requests {
		req := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- sv.dispatchOne(ctx, req)
		}()
	}
	wg.Wait()
	close(results)
	out := make([]DispatchResult, 0, len(requests))
	for result := range results {
		out = append(out, result)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SHA < out[j].SHA })
	return out
}

func (sv *ReviewSupervisor) currentQueueState(sha string) QueueState {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	if cand, ok := sv.cands[sha]; ok {
		return queueState(cand)
	}
	return QueueAdmitted
}

func (sv *ReviewSupervisor) dispatchOne(parent context.Context, req DispatchRequest) DispatchResult {
	result := DispatchResult{SHA: req.SHA}
	attempts := req.MaxAttempts
	if attempts <= 0 {
		attempts = sv.cfg.RetryLimit
	}
	if attempts <= 0 {
		attempts = 1
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = sv.cfg.DispatchTimeout
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	var lastReason string
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := parent.Err(); err != nil {
			return DispatchResult{
				SHA:      req.SHA,
				State:    sv.currentQueueState(req.SHA),
				Attempts: attempt - 1,
				Reason:   "dispatch canceled: " + err.Error(),
			}
		}
		reviewer, err := sv.SelectReviewer(req.SHA, req.Reviewers)
		if err != nil {
			return DispatchResult{
				SHA:      req.SHA,
				State:    sv.currentQueueState(req.SHA),
				Attempts: attempt - 1,
				Reason:   err.Error(),
			}
		}
		if reviewer == nil {
			lastReason = "no routable reviewer available"
			continue
		}
		launch := req.Launch
		if launch == nil {
			launch = func(ctx context.Context, sha string, entry ReviewerEntry) error {
				return sv.LaunchReviewContext(ctx, sha, entry.Name, entry.Model)
			}
		}
		callCtx, cancel := context.WithTimeout(parent, timeout)
		err = launch(callCtx, req.SHA, *reviewer)
		cancel()
		result.Attempts = attempt
		if err == nil {
			result.State = QueueLaunched
			result.Reviewer = reviewer.Name
			return result
		}
		if parentErr := parent.Err(); parentErr != nil {
			return DispatchResult{
				SHA:      req.SHA,
				State:    sv.currentQueueState(req.SHA),
				Attempts: attempt,
				Reason:   "dispatch canceled: " + parentErr.Error(),
			}
		}
		lastReason = err.Error()
	}
	result.Attempts = attempts
	result.Reason = fmt.Sprintf("no routable review surface after %d attempts: %s", attempts, lastReason)
	if err := sv.markDispatchBlocked(req.SHA, result.Reason, attempts); err != nil {
		result.State = sv.currentQueueState(req.SHA)
		result.Reason += "; durable record failed: " + err.Error()
		return result
	}
	result.State = QueueBlocked
	return result
}

func (sv *ReviewSupervisor) markDispatchBlocked(sha, reason string, attempts int) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	cand, ok := sv.cands[sha]
	if !ok {
		return fmt.Errorf("reviewsup: unknown candidate %s", sha)
	}
	if cand.State != StatePending {
		return fmt.Errorf("reviewsup: candidate %s is not pending (state=%s)", sha, cand.State)
	}
	oldState, oldVerdict := cand.State, cand.Verdict
	oldAttempts, oldUpdated := cand.Attempts, cand.UpdatedAt
	cand.DispatchBlocked = true
	cand.DispatchReason = reason
	cand.State = StateBlocked
	cand.Verdict = VerdictBLOCKED
	cand.Attempts = attempts
	cand.UpdatedAt = sv.now()
	sv.pendingCount--
	if err := sv.appendRow(&Row{Event: string(EventDispatchBlocked), SHA: sha, Reason: reason, Attempts: attempts}); err != nil {
		cand.DispatchBlocked = false
		cand.DispatchReason = ""
		cand.State = oldState
		cand.Verdict = oldVerdict
		cand.Attempts = oldAttempts
		cand.UpdatedAt = oldUpdated
		sv.pendingCount++
		return fmt.Errorf("reviewsup: append dispatch blocked row: %w", err)
	}
	return nil
}

type ReviewVerdict struct {
	SHA      string
	Reviewer string
	Verdict  Verdict
	Reason   string
	// Artifact is the path of the reviewer verdict body. For PASS it may be an
	// ephemeral temp path; SubmitVerdict retains it under the repo review
	// inbox before any cleanup-candidate transition (FAC-373).
	Artifact string
}

func (sv *ReviewSupervisor) SubmitVerdict(v ReviewVerdict) (newState CandidateState, err error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	if v.SHA == "" {
		return "", fmt.Errorf("reviewsup: empty SHA in verdict")
	}

	cand, ok := sv.cands[v.SHA]
	if !ok {
		return "", fmt.Errorf("reviewsup: unknown candidate %s", v.SHA)
	}

	if cand.State != StateReviewing {
		return "", fmt.Errorf("reviewsup: candidate %s is not reviewing (state=%s)", v.SHA, cand.State)
	}

	if v.Reviewer != cand.Reviewer {
		return "", fmt.Errorf("reviewsup: verdict reviewer %q does not match assigned reviewer %q", v.Reviewer, cand.Reviewer)
	}

	// Use the family recorded at LaunchReview time (derived from the actual
	// review model), never re-derive from the reviewer's label — a reviewer
	// name is an arbitrary string and can accidentally collide with a family
	// keyword, letting a same-family review masquerade as cross-family.
	reviewFamily := ModelFamily(cand.ReviewFamily)
	authorFamily := lookupFamily(cand.AuthorModel)
	needsCross := RequireCrossFamily(cand.Tier)
	if needsCross && !CrossFamilyOK(authorFamily, reviewFamily) {
		return "", fmt.Errorf("reviewsup: candidate %s received verdict from same-family reviewer (author=%s, reviewer=%s)", v.SHA, authorFamily, reviewFamily)
	}

	// FAC-373: a PASS is not cleanup-ready until the exact-SHA artifact is
	// durably retained under .herd/review/inbox. Missing or vanished sources
	// become one durable BLOCKED state — never a coordinator-directed PASS.
	var retainedArtifact string
	if v.Verdict == VerdictPASS {
		retained, retainErr := sv.retainPASSArtifact(v)
		if retainErr != nil {
			return sv.blockMissingArtifact(cand, v, retainErr)
		}
		retainedArtifact = retained
	}

	cand.Verdict = v.Verdict
	cand.VerdictReason = v.Reason
	cand.UpdatedAt = sv.now()
	if retainedArtifact != "" {
		cand.Artifact = retainedArtifact
	}

	if err := sv.appendRow(&Row{
		Event:    string(EventVerdict),
		SHA:      v.SHA,
		Reviewer: v.Reviewer,
		Verdict:  string(v.Verdict),
		Reason:   v.Reason,
		Artifact: retainedArtifact,
	}); err != nil {
		cand.Verdict = ""
		cand.VerdictReason = ""
		cand.Artifact = ""
		cand.UpdatedAt = sv.now()
		return "", fmt.Errorf("reviewsup: append verdict row: %w", err)
	}
	if err := sv.appendRow(&Row{
		Event:         string(EventVerdictRetained),
		SHA:           v.SHA,
		Reviewer:      v.Reviewer,
		Verdict:       string(v.Verdict),
		Reason:        v.Reason,
		ReceiptDigest: cand.ReceiptDigest,
		Artifact:      retainedArtifact,
	}); err != nil {
		return "", fmt.Errorf("reviewsup: retain verdict row: %w", err)
	}

	switch v.Verdict {
	case VerdictPASS:
		sv.pendingCount--
		if err := sv.appendQueue(&Row{
			Event:        string(EventHarvest),
			SHA:          v.SHA,
			Reviewer:     v.Reviewer,
			AuthorFamily: cand.AuthorFamily,
			ReviewFamily: cand.ReviewFamily,
			Tier:         string(cand.Tier),
			Attempts:     cand.Attempts,
			Artifact:     retainedArtifact,
		}); err != nil {
			sv.pendingCount++
			cand.Verdict = ""
			cand.VerdictReason = ""
			cand.Artifact = ""
			cand.UpdatedAt = sv.now()
			return "", fmt.Errorf("reviewsup: append harvest row: %w", err)
		}
		cand.VerdictRetained = true
		cand.Ingested = true
		cand.HarvestReady = true
		if err := sv.appendRow(&Row{Event: string(EventIngested), SHA: v.SHA, Reviewer: v.Reviewer, ReceiptDigest: cand.ReceiptDigest, Artifact: retainedArtifact}); err != nil {
			return "", fmt.Errorf("reviewsup: retain ingested event: %w", err)
		}
		// Cleanup is admitted only after durable retain + ingest proof.
		if err := sv.appendRow(&Row{Event: string(EventCleanupCandidate), SHA: v.SHA, Reviewer: v.Reviewer, Artifact: retainedArtifact, Reason: "exact-SHA artifact retained under review inbox; coordinator may close exact tab after harvest"}); err != nil {
			return "", fmt.Errorf("reviewsup: append cleanup candidate: %w", err)
		}
		cand.State = StateHarvested
		cand.CleanupCandidate = true

	case VerdictFAIL:
		effective := VerdictFAIL
		if cand.Attempts >= sv.cfg.RetryLimit {
			effective = VerdictBLOCKED
		}
		sv.pendingCount--
		// Durable handoff of findings to the owning builder. Written before
		// any state transition so a crash cannot lose the repair packet.
		if err := sv.appendRow(&Row{
			Event:    string(EventBuilderCallback),
			SHA:      v.SHA,
			Reviewer: v.Reviewer,
			Verdict:  string(effective),
			Reason:   v.Reason,
			Attempts: cand.Attempts,
		}); err != nil {
			sv.pendingCount++
			cand.Verdict = ""
			cand.VerdictReason = ""
			cand.UpdatedAt = sv.now()
			return "", fmt.Errorf("reviewsup: append builder callback row: %w", err)
		}
		if err := sv.appendRow(&Row{Event: string(EventAuthorNotified), SHA: v.SHA, Reviewer: v.Reviewer, Verdict: string(effective), Reason: v.Reason}); err != nil {
			return "", fmt.Errorf("reviewsup: append author notification: %w", err)
		}
		if effective == VerdictBLOCKED {
			cand.State = StateBlocked
			cand.Verdict = VerdictBLOCKED
		} else {
			cand.State = StatePending
			cand.Reviewer = ""
			cand.ReviewFamily = ""
			cand.Verdict = ""
		}

	case VerdictBLOCKED:
		sv.pendingCount--
		// Durable handoff of findings to the owning builder (terminal).
		if err := sv.appendRow(&Row{
			Event:    string(EventBuilderCallback),
			SHA:      v.SHA,
			Reviewer: v.Reviewer,
			Verdict:  string(VerdictBLOCKED),
			Reason:   v.Reason,
			Attempts: cand.Attempts,
		}); err != nil {
			sv.pendingCount++
			cand.Verdict = ""
			cand.VerdictReason = ""
			cand.UpdatedAt = sv.now()
			return "", fmt.Errorf("reviewsup: append builder callback row: %w", err)
		}
		if err := sv.appendRow(&Row{Event: string(EventAuthorNotified), SHA: v.SHA, Reviewer: v.Reviewer, Verdict: string(VerdictBLOCKED), Reason: v.Reason}); err != nil {
			return "", fmt.Errorf("reviewsup: append author notification: %w", err)
		}
		cand.State = StateBlocked
	}

	return cand.State, nil
}

// retainPASSArtifact copies the reviewer verdict into the durable review
// inbox and re-stats it. Ephemeral temp paths (chainseer-herd-review, /tmp)
// are never cleanup authority.
func (sv *ReviewSupervisor) retainPASSArtifact(v ReviewVerdict) (string, error) {
	root := strings.TrimSpace(sv.cfg.RepoRoot)
	if root == "" {
		return "", fmt.Errorf("RepoRoot required for durable PASS retention")
	}
	source := strings.TrimSpace(v.Artifact)
	if source == "" {
		return "", fmt.Errorf("missing PASS verdict artifact for %s", v.SHA)
	}
	// Allow either an absolute ephemeral path or a repo-relative path.
	if !filepath.IsAbs(source) {
		source = filepath.Join(root, source)
	}
	retained, err := reviewingest.RetainArtifact(root, source, v.SHA, v.Reviewer)
	if err != nil {
		return "", err
	}
	if !reviewingest.IsInboxPath(retained) {
		return "", fmt.Errorf("retain produced non-inbox path %q", retained)
	}
	dst := filepath.Join(root, filepath.FromSlash(retained))
	info, err := os.Stat(dst)
	if err != nil {
		return "", fmt.Errorf("retained artifact vanished before cleanup gate: %w", err)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("retained artifact is empty")
	}
	return retained, nil
}

// blockMissingArtifact records one durable BLOCKED state when a claimed PASS
// cannot prove a durable inbox artifact. The candidate never becomes
// cleanup-candidate or harvest-ready.
func (sv *ReviewSupervisor) blockMissingArtifact(cand *Candidate, v ReviewVerdict, cause error) (CandidateState, error) {
	reason := fmt.Sprintf("PASS refused: durable review-inbox artifact required before cleanup (%v)", cause)
	cand.Verdict = VerdictBLOCKED
	cand.VerdictReason = reason
	cand.UpdatedAt = sv.now()
	cand.VerdictRetained = false
	cand.Ingested = false
	cand.HarvestReady = false
	cand.CleanupCandidate = false
	cand.Artifact = ""

	if err := sv.appendRow(&Row{
		Event:    string(EventVerdict),
		SHA:      v.SHA,
		Reviewer: v.Reviewer,
		Verdict:  string(VerdictBLOCKED),
		Reason:   reason,
	}); err != nil {
		cand.Verdict = ""
		cand.VerdictReason = ""
		cand.UpdatedAt = sv.now()
		return "", fmt.Errorf("reviewsup: append missing-artifact BLOCKED: %w", err)
	}
	sv.pendingCount--
	if err := sv.appendRow(&Row{
		Event:    string(EventBuilderCallback),
		SHA:      v.SHA,
		Reviewer: v.Reviewer,
		Verdict:  string(VerdictBLOCKED),
		Reason:   reason,
		Attempts: cand.Attempts,
	}); err != nil {
		sv.pendingCount++
		cand.Verdict = ""
		cand.VerdictReason = ""
		cand.UpdatedAt = sv.now()
		return "", fmt.Errorf("reviewsup: append missing-artifact builder callback: %w", err)
	}
	if err := sv.appendRow(&Row{
		Event:    string(EventAuthorNotified),
		SHA:      v.SHA,
		Reviewer: v.Reviewer,
		Verdict:  string(VerdictBLOCKED),
		Reason:   reason,
	}); err != nil {
		return "", fmt.Errorf("reviewsup: append missing-artifact author notification: %w", err)
	}
	cand.State = StateBlocked
	return StateBlocked, fmt.Errorf("reviewsup: %s", reason)
}

func (sv *ReviewSupervisor) PendingCount() int {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	return sv.pendingCount
}

func (sv *ReviewSupervisor) AtCapacity() bool {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	return sv.pendingCount >= sv.cfg.MaxPendingReviews
}

func (sv *ReviewSupervisor) AvailableCapacity() int {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	free := sv.cfg.MaxPendingReviews - sv.pendingCount
	if free < 0 {
		return 0
	}
	return free
}

// Watchdog reports review pins that have had no state transition for at least
// timeout. When reviewerLive is supplied, an alert is emitted only when the
// assigned reviewer is not live; with no liveness provider, stale pins are
// still returned with an explicit liveness-unknown reason so a caller cannot
// mistake an unobserved fleet for an empty queue.
func (sv *ReviewSupervisor) Watchdog(timeout time.Duration, reviewerLive func(string) bool) []WatchdogAlert {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	now := sv.now()
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	alerts := make([]WatchdogAlert, 0)
	for _, cand := range sv.cands {
		if cand == nil || cand.State != StateReviewing {
			continue
		}
		age := now.Sub(cand.UpdatedAt)
		if age < timeout {
			continue
		}
		if reviewerLive != nil && cand.Reviewer != "" && reviewerLive(cand.Reviewer) {
			continue
		}
		reason := "review dispatch overdue"
		if reviewerLive == nil {
			reason = "reviewer liveness unknown"
		} else if cand.Reviewer == "" || !reviewerLive(cand.Reviewer) {
			reason = "reviewer not live"
		}
		alerts = append(alerts, WatchdogAlert{SHA: cand.SHA, Reviewer: cand.Reviewer, Age: age, Reason: reason})
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].SHA < alerts[j].SHA })
	return alerts
}

type HarvestCandidate struct {
	SHA          string
	AuthorFamily string
	ReviewFamily string
	Tier         RiskTier
	Attempts     int
	Findings     string
	HarvestedAt  time.Time
}

func (sv *ReviewSupervisor) ReadyForHarvest(max int) ([]HarvestCandidate, error) {
	sv.mu.RLock()
	defer sv.mu.RUnlock()

	qrows, err := readRows(sv.cfg.QueuePath)
	if err != nil {
		return nil, err
	}

	type qe struct {
		row   Row
		order int
	}
	seen := make(map[string]qe)
	for i, r := range qrows {
		if r.Event == string(EventHarvest) && !r.Harvested {
			seen[r.SHA] = qe{row: r, order: i}
		}
	}

	var result []HarvestCandidate
	for sha, eq := range seen {
		cand, ok := sv.cands[sha]
		if !ok || cand.State != StateHarvested {
			continue
		}
		result = append(result, HarvestCandidate{
			SHA:          sha,
			AuthorFamily: eq.row.AuthorFamily,
			ReviewFamily: eq.row.ReviewFamily,
			Tier:         RiskTier(eq.row.Tier),
			Attempts:     eq.row.Attempts,
			Findings:     cand.VerdictReason,
			HarvestedAt:  sv.cfg.Now(),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].SHA < result[j].SHA
	})

	if max > 0 && len(result) > max {
		result = result[:max]
	}

	return result, nil
}

func (sv *ReviewSupervisor) MarkHarvested(sha string) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	if cand, ok := sv.cands[sha]; ok {
		cand.State = StateHarvested
		cand.UpdatedAt = sv.now()
	}

	qrows, err := readRows(sv.cfg.QueuePath)
	if err != nil {
		return err
	}
	var updated bool
	for i, r := range qrows {
		if r.SHA == sha && r.Event == string(EventHarvest) {
			qrows[i].Harvested = true
			updated = true
		}
	}
	if !updated {
		return nil
	}
	f, err := os.Create(sv.cfg.QueuePath)
	if err != nil {
		return fmt.Errorf("rewrite queue: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range qrows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	if sv.cfg.Orders != nil {
		if _, err := sv.cfg.Orders.Callback(context.Background(), fmt.Sprintf("builder callback acknowledgement for %s", sha)); err != nil {
			return fmt.Errorf("reviewsup: durable callback order: %w", err)
		}
	}
	return nil
}

// MarkClosed records the final coordinator-owned cleanup transition after the
// exact Herdr reviewer tab and its worktree have been closed. Reviewers never
// close their own panes; this method makes that handoff durable and queryable.
func (sv *ReviewSupervisor) MarkClosed(sha string) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	cand, ok := sv.cands[sha]
	if !ok {
		return fmt.Errorf("reviewsup: unknown candidate %s", sha)
	}
	// Reconstruct replays the durable verdict before it can observe the queue
	// row's harvested bit, so a retained PASS may temporarily have StatePass.
	// The cleanup-candidate event is the authoritative admission proof across a
	// restart; requiring StateHarvested here made a valid pane impossible to
	// close after supervisor recovery.
	if cand.State != StateHarvested && !(cand.State == StatePass && cand.CleanupCandidate) && cand.State != StateEvicted {
		return fmt.Errorf("reviewsup: candidate %s is not cleanup-ready (state=%s)", sha, cand.State)
	}
	if err := sv.appendRow(&Row{Event: string(EventClosed), SHA: sha, Reason: "coordinator confirmed exact-tab and worktree cleanup"}); err != nil {
		return fmt.Errorf("reviewsup: append closed event: %w", err)
	}
	cand.State = StateEvicted
	cand.CleanupCandidate = false
	cand.UpdatedAt = sv.now()
	return nil
}

// BuilderHandoff is a durable repair packet returned to the owning builder
// after a FAIL or BLOCKED verdict.
type BuilderHandoff struct {
	SHA      string
	Reviewer string
	Verdict  Verdict
	Findings string
	Attempts int
}

// ReadyForBuilder returns undelivered builder handoffs for candidates still
// owned by the builder flow (pending repair or terminally blocked).
func (sv *ReviewSupervisor) ReadyForBuilder(max int) ([]BuilderHandoff, error) {
	sv.mu.RLock()
	defer sv.mu.RUnlock()

	rows, err := readRows(sv.cfg.LedgerPath)
	if err != nil {
		return nil, err
	}

	// Latest un-acked callback per SHA wins; an ack clears all earlier ones.
	pending := make(map[string]Row)
	var order []string
	for _, r := range rows {
		switch EventType(r.Event) {
		case EventBuilderCallback:
			if _, seen := pending[r.SHA]; !seen {
				order = append(order, r.SHA)
			}
			pending[r.SHA] = r
		case EventBuilderAck:
			delete(pending, r.SHA)
		}
	}

	var result []BuilderHandoff
	for _, sha := range order {
		r, ok := pending[sha]
		if !ok {
			continue
		}
		cand, ok := sv.cands[sha]
		if !ok {
			continue
		}
		if cand.State != StatePending && cand.State != StateBlocked {
			continue
		}
		result = append(result, BuilderHandoff{
			SHA:      sha,
			Reviewer: r.Reviewer,
			Verdict:  Verdict(r.Verdict),
			Findings: r.Reason,
			Attempts: r.Attempts,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].SHA < result[j].SHA
	})

	if max > 0 && len(result) > max {
		result = result[:max]
	}

	return result, nil
}

// MarkBuilderDelivered durably acks a builder handoff for the given SHA.
func (sv *ReviewSupervisor) MarkBuilderDelivered(sha string) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	if _, ok := sv.cands[sha]; !ok {
		return fmt.Errorf("reviewsup: unknown candidate %s", sha)
	}
	if err := sv.appendRow(&Row{
		Event: string(EventBuilderAck),
		SHA:   sha,
	}); err != nil {
		return fmt.Errorf("reviewsup: append builder ack row: %w", err)
	}
	if sv.cfg.Orders != nil {
		if _, err := sv.cfg.Orders.Callback(context.Background(), fmt.Sprintf("builder callback acknowledgement for %s", sha)); err != nil {
			return fmt.Errorf("reviewsup: durable callback order: %w", err)
		}
	}
	return nil
}

func (sv *ReviewSupervisor) EvictStale() (int, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	cutoff := sv.now().Add(-sv.cfg.StaleDuration)
	var evicted int
	for sha, cand := range sv.cands {
		if cand.State == StateEvicted || cand.State == StateBlocked || cand.State == StateHarvested {
			continue
		}
		if cand.IngestedAt.Before(cutoff) {
			pWasPending := (cand.State == StatePending || cand.State == StateReviewing)
			oldState := cand.State
			if pWasPending {
				sv.pendingCount--
			}
			cand.State = StateEvicted
			cand.UpdatedAt = sv.now()
			if err := sv.appendRow(&Row{
				Event:  string(EventEvict),
				SHA:    sha,
				Reason: "stale",
			}); err != nil {
				cand.State = oldState
				if pWasPending {
					sv.pendingCount++
				}
				return evicted, fmt.Errorf("reviewsup: append evict row: %w", err)
			}
			evicted++
		}
	}
	return evicted, nil
}

func (sv *ReviewSupervisor) Reconstruct() (int, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	rows, err := readRows(sv.cfg.LedgerPath)
	if err != nil {
		return 0, fmt.Errorf("reviewsup: read ledger: %w", err)
	}

	sv.cands = make(map[string]*Candidate)
	sv.shaIdx = make(map[string]string)
	sv.leaseGen = make(map[string]int64)
	sv.pendingCount = 0
	sv.evrows = nil

	// Track terminal FAIL/BLOCKED verdicts and builder handoff rows by ledger
	// position so missing handoffs (written by older versions) can be
	// backfilled after replay.
	lastVerdictRow := make(map[string]Row)
	lastVerdictAt := make(map[string]int)
	lastCallbackAt := make(map[string]int)
	lastAckAt := make(map[string]int)

	for i, r := range rows {
		sv.evrows = append(sv.evrows, r)

		switch EventType(r.Event) {
		case EventCompletion:
			tier := TierR0
			if r.Tier != "" {
				tier = RiskTier(r.Tier)
			}
			ingestedAt := sv.cfg.Now()
			if r.IngestedAt != "" {
				if t, err := time.Parse(time.RFC3339, r.IngestedAt); err == nil {
					ingestedAt = t
				}
			}
			cand := &Candidate{
				SHA:           r.SHA,
				Branch:        r.Branch,
				PatchID:       r.PatchID,
				AuthorModel:   r.AuthorModel,
				AuthorFamily:  r.AuthorFamily,
				Tier:          tier,
				State:         StatePending,
				IngestedAt:    ingestedAt,
				UpdatedAt:     sv.cfg.Now(),
				LeaseID:       r.LeaseID,
				Generation:    r.Generation,
				ReceiptDigest: r.ReceiptDigest,
			}
			sv.cands[r.SHA] = cand
			if r.PatchID != "" {
				sv.shaIdx[r.PatchID] = r.SHA
			}
			if r.LeaseID != "" && r.Generation > 0 {
				if cur, ok := sv.leaseGen[r.LeaseID]; !ok || r.Generation > cur {
					sv.leaseGen[r.LeaseID] = r.Generation
				}
			}
			sv.pendingCount++

		case EventReview:
			if cand, ok := sv.cands[r.SHA]; ok {
				if cand.State == StatePending {
					cand.State = StateReviewing
					cand.Reviewer = r.Reviewer
					cand.ReviewFamily = r.ReviewFamily
					cand.Provider = r.Provider
					cand.Harness = r.Harness
					cand.Attempts = r.Attempts
				}
			}

		case EventLaunchFailed:
			if cand, ok := sv.cands[r.SHA]; ok {
				cand.State = StatePending
				cand.Reviewer = ""
				cand.ReviewFamily = ""
				cand.VerdictReason = r.Reason
			}

		case EventVerdict:
			if cand, ok := sv.cands[r.SHA]; ok {
				cand.Verdict = Verdict(r.Verdict)
				cand.VerdictReason = r.Reason
				switch Verdict(r.Verdict) {
				case VerdictPASS:
					cand.State = StatePass
					sv.pendingCount--
				case VerdictFAIL:
					lastVerdictAt[r.SHA] = i
					lastVerdictRow[r.SHA] = r
					sv.pendingCount--
					if cand.Attempts >= sv.cfg.RetryLimit {
						cand.State = StateBlocked
						cand.Verdict = VerdictBLOCKED
					} else {
						cand.State = StatePending
						cand.Reviewer = ""
						cand.ReviewFamily = ""
					}
				case VerdictBLOCKED:
					lastVerdictAt[r.SHA] = i
					lastVerdictRow[r.SHA] = r
					cand.State = StateBlocked
					sv.pendingCount--
				}
			}

		case EventVerdictRetained:
			if cand, ok := sv.cands[r.SHA]; ok {
				cand.VerdictRetained = true
				cand.Verdict = Verdict(r.Verdict)
				cand.VerdictReason = r.Reason
				if r.Artifact != "" {
					cand.Artifact = r.Artifact
				}
			}

		case EventIngested:
			if cand, ok := sv.cands[r.SHA]; ok {
				cand.Ingested = true
				if r.Artifact != "" {
					cand.Artifact = r.Artifact
				}
			}

		case EventCleanupCandidate:
			if cand, ok := sv.cands[r.SHA]; ok {
				// FAC-373: cleanup-candidate is only authoritative when the
				// retained inbox artifact path is still on the row (and was
				// recorded with a real retain). Legacy rows without Artifact
				// still project cleanup so MarkClosed can recover them, but
				// new PASS paths always carry the path.
				cand.CleanupCandidate = true
				if r.Artifact != "" {
					cand.Artifact = r.Artifact
				}
			}

		case EventDispatchBlocked:
			if cand, ok := sv.cands[r.SHA]; ok {
				if cand.State != StatePending {
					break
				}
				sv.pendingCount--
				cand.DispatchBlocked = true
				cand.DispatchReason = r.Reason
				cand.State = StateBlocked
				cand.Verdict = VerdictBLOCKED
				cand.Attempts = r.Attempts
			}

		case EventClosed:
			if cand, ok := sv.cands[r.SHA]; ok {
				cand.State = StateEvicted
				cand.CleanupCandidate = false
			}

		case EventSupersede:
			if cand, ok := sv.cands[r.PrevSHA]; ok {
				pWasPending := (cand.State == StatePending || cand.State == StateReviewing || cand.State == StatePass)
				cand.State = StateEvicted
				if pWasPending {
					sv.pendingCount--
				}
			}

		case EventEvict:
			if cand, ok := sv.cands[r.SHA]; ok {
				pWasPending := (cand.State == StatePending || cand.State == StateReviewing)
				cand.State = StateEvicted
				if pWasPending {
					sv.pendingCount--
				}
			}

		case EventBuilderCallback:
			lastCallbackAt[r.SHA] = i

		case EventBuilderAck:
			lastAckAt[r.SHA] = i

		case EventRouteBlocked:
			if cand, ok := sv.cands[r.SHA]; ok {
				if cand.State == StatePending || cand.State == StateReviewing || cand.State == StatePass {
					sv.pendingCount--
				}
				cand.State = StateBlocked
				cand.Verdict = VerdictBLOCKED
				cand.BlockedReason = r.Reason
			}

		case EventCapacity:
		}
	}

	qrows, err := readRows(sv.cfg.QueuePath)
	if err != nil {
		// Fail closed: an unreadable evidence queue hides harvest state.
		return 0, fmt.Errorf("reviewsup: read queue: %w", err)
	}
	for _, r := range qrows {
		if r.Event == string(EventHarvest) && r.Harvested {
			if cand, ok := sv.cands[r.SHA]; ok {
				cand.State = StateHarvested
			}
		}
	}

	// Terminal-event backfill: candidates stuck at a FAIL/BLOCKED terminal
	// verdict without a durable builder handoff get one appended now.
	for sha, vIdx := range lastVerdictAt {
		cand, ok := sv.cands[sha]
		if !ok {
			continue
		}
		if cand.State != StateBlocked && !(cand.State == StatePending && cand.Verdict == VerdictFAIL) {
			continue
		}
		if cbIdx, ok := lastCallbackAt[sha]; ok && cbIdx > vIdx {
			continue
		}
		if ackIdx, ok := lastAckAt[sha]; ok && ackIdx > vIdx {
			continue
		}
		vr := lastVerdictRow[sha]
		bv := VerdictFAIL
		if cand.Verdict == VerdictBLOCKED || vr.Verdict == string(VerdictBLOCKED) {
			bv = VerdictBLOCKED
		}
		if err := sv.appendRow(&Row{
			Event:    string(EventBuilderCallback),
			SHA:      sha,
			Reviewer: vr.Reviewer,
			Verdict:  string(bv),
			Reason:   vr.Reason,
			Attempts: cand.Attempts,
		}); err != nil {
			return 0, fmt.Errorf("reviewsup: backfill builder callback for %s: %w", sha, err)
		}
	}

	return len(sv.cands), nil
}

type Status struct {
	Config         Config
	CandidateCount int
	PendingCount   int
	AtCapacity     bool
	AvailableCap   int
	Candidates     []*Candidate
}

func (sv *ReviewSupervisor) Status() *Status {
	sv.mu.RLock()
	defer sv.mu.RUnlock()

	s := &Status{
		Config:         sv.cfg,
		CandidateCount: len(sv.cands),
		PendingCount:   sv.pendingCount,
		AtCapacity:     sv.pendingCount >= sv.cfg.MaxPendingReviews,
		AvailableCap: func() int {
			free := sv.cfg.MaxPendingReviews - sv.pendingCount
			if free < 0 {
				return 0
			}
			return free
		}(),
	}
	for _, c := range sv.cands {
		s.Candidates = append(s.Candidates, c)
	}
	sort.Slice(s.Candidates, func(i, j int) bool {
		return s.Candidates[i].SHA < s.Candidates[j].SHA
	})
	return s
}

func (sv *ReviewSupervisor) Candidate(sha string) *Candidate {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	c, ok := sv.cands[sha]
	if !ok {
		return nil
	}
	cp := *c
	return &cp
}
