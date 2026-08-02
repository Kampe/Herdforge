package reviewsup

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// --- Event types for the durable ledger ---

type EventType string

const (
	EventCompletion EventType = "completion" // builder finished, candidate ingested
	EventReview     EventType = "review"     // review launched
	EventVerdict    EventType = "verdict"    // reviewer returned PASS/FAIL/BLOCKED
	EventSupersede  EventType = "supersede"  // new commit replaces old one
	EventEvict      EventType = "evict"      // stale candidate evicted
	EventHarvest    EventType = "harvest"    // enqueued for integration
	EventCapacity   EventType = "capacity"   // reviewer capacity changed
)

// Verdict values.
type Verdict string

const (
	VerdictPASS    Verdict = "PASS"
	VerdictFAIL    Verdict = "FAIL"
	VerdictBLOCKED Verdict = "BLOCKED"
)

// RiskTier values.
type RiskTier string

const (
	TierR0 RiskTier = "R0"
	TierR1 RiskTier = "R1"
	TierR2 RiskTier = "R2"
	TierR3 RiskTier = "R3"
)

// --- Durable ledger row ---

type Row struct {
	Timestamp    string   `json:"ts"`
	Event        string   `json:"event"`
	SHA          string   `json:"sha"`
	Branch       string   `json:"branch,omitempty"`
	PatchID      string   `json:"patch_id,omitempty"`
	AuthorModel  string   `json:"author_model,omitempty"`
	AuthorFamily string   `json:"author_family,omitempty"`
	Reviewer     string   `json:"reviewer,omitempty"`
	ReviewFamily string   `json:"review_family,omitempty"`
	Tier         string   `json:"tier,omitempty"`
	Verdict      string   `json:"verdict,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	PrevSHA      string   `json:"prev_sha,omitempty"`
	Harvested    bool     `json:"harvested,omitempty"`
	Capacity     int      `json:"capacity,omitempty"`
	Attempts     int      `json:"attempts,omitempty"`
	IngestedAt   string   `json:"ingested_at,omitempty"`
}

// --- In-memory mutable state reconstructed from ledger ---

type CandidateState string

const (
	StatePending   CandidateState = "pending"    // ingested, awaiting review
	StateReviewing CandidateState = "reviewing"  // review in flight
	StatePass      CandidateState = "pass"       // PASS verdict
	StateFail      CandidateState = "fail"       // FAIL verdict (returned to builder)
	StateBlocked   CandidateState = "blocked"    // BLOCKED with reason preserved
	StateHarvested CandidateState = "harvested"  // enqueued for integration
	StateEvicted   CandidateState = "evicted"    // superseded or expired
)

type Candidate struct {
	SHA          string
	Branch       string
	PatchID      string
	AuthorModel  string
	AuthorFamily string
	Tier         RiskTier
	State        CandidateState
	Verdict      Verdict
	VerdictReason string
	Reviewer     string
	ReviewFamily string
	Attempts     int
	IngestedAt   time.Time
	UpdatedAt    time.Time
}

// --- ModelFamily registry (inline, no import from pkg/review) ---

type ModelFamily string

const (
	FamilyLazer ModelFamily = "lazer"
	FamilyAnt   ModelFamily = "anthropic"
	FamilyGoogle ModelFamily = "google"
	FamilyOpenAI ModelFamily = "openai"
	FamilyGrok  ModelFamily = "grok"
	FamilyKimi  ModelFamily = "kimi"
	FamilyCodex ModelFamily = "codex"
	FamilyOther ModelFamily = "other"
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

// CrossFamilyOK returns true if the two model families are different.
func CrossFamilyOK(authorFamily, reviewFamily ModelFamily) bool {
	return authorFamily != reviewFamily
}

// RequireCrossFamily returns true for R1-R3 tiers; R0 (mechanical) may use same family.
func RequireCrossFamily(tier RiskTier) bool {
	return tier == TierR1 || tier == TierR2 || tier == TierR3
}

// --- Config ---

type Config struct {
	LedgerPath        string
	QueuePath         string
	MaxPendingReviews int           // max concurrent in-flight reviews
	StaleDuration     time.Duration // candidate considered stale after this
	RetryLimit        int           // max review attempts before BLOCKED
	Now               func() time.Time
}

// DefaultConfig returns a reasonable production config.
func DefaultConfig(ledgerDir string) Config {
	return Config{
		LedgerPath:        filepath.Join(ledgerDir, "supervisor-ledger.jsonl"),
		QueuePath:         filepath.Join(ledgerDir, "harvest-evidence-queue.jsonl"),
		MaxPendingReviews: 3,
		StaleDuration:     24 * time.Hour,
		RetryLimit:        3,
		Now:               time.Now,
	}
}

// --- ReviewSupervisor ---

type ReviewSupervisor struct {
	mu sync.RWMutex

	cfg    Config
	cands  map[string]*Candidate // keyed by SHA
	shaIdx map[string]string     // patchID -> latest SHA; tracks supersession
	evrows []Row                 // replay buffer for Evict stale scan

	pendingCount int
}

func New(cfg Config) *ReviewSupervisor {
	dir := filepath.Dir(cfg.LedgerPath)
	os.MkdirAll(dir, 0755)

	sv := &ReviewSupervisor{
		cfg:    cfg,
		cands:  make(map[string]*Candidate),
		shaIdx: make(map[string]string),
	}
	return sv
}

// nowISO returns the current UTC timestamp.
func (sv *ReviewSupervisor) nowISO() string { return sv.cfg.Now().UTC().Format(time.RFC3339) }

// timestamp returns the current time.
func (sv *ReviewSupervisor) now() time.Time { return sv.cfg.Now().UTC() }

// --- Ledger I/O ---

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
		return err
	}
	_, err = f.WriteString("\n")
	return err
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
		return err
	}
	_, err = f.WriteString("\n")
	return err
}

func readRows(path string) ([]Row, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, nil
	}
	defer f.Close()
	var rows []Row
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		rows = append(rows, r)
	}
	return rows, sc.Err()
}

// --- Ingestion ---

// CompletionCallback is what the builder sends when work is done.
type CompletionCallback struct {
	SHA         string
	Branch      string
	PatchID     string
	AuthorModel string
	Tier        RiskTier
	Files       []string // for tier reclassification
}

// Ingest receives a completion callback. Returns (accepted, staleSHA, error).
// If a newer commit supersedes this one, staleSHA is set.
func (sv *ReviewSupervisor) Ingest(cb CompletionCallback) (accepted bool, staleSHA string, err error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	if cb.SHA == "" {
		return false, "", fmt.Errorf("reviewsup: empty SHA in completion callback")
	}

	// Check for existing candidate with same SHA (duplicate callback).
	if _, ok := sv.cands[cb.SHA]; ok {
		return false, "", nil
	}

	// Check supersession: same patchID but newer SHA replaces old.
	if cb.PatchID != "" {
		if prevSHA, ok := sv.shaIdx[cb.PatchID]; ok {
			if prevCand, ok := sv.cands[prevSHA]; ok {
				// Mark old candidate as evicted.
				prevCand.State = StateEvicted
				prevCand.UpdatedAt = sv.now()
				sv.appendRow(&Row{
					Event:   string(EventSupersede),
					SHA:     cb.SHA,
					PrevSHA: prevSHA,
					PatchID: cb.PatchID,
					Reason:  "newer commit supersedes",
				})
				if prevCand.State == StatePending || prevCand.State == StateReviewing || prevCand.State == StatePass {
					sv.pendingCount--
				}
				staleSHA = prevSHA
			}
		}
	}

	cand := &Candidate{
		SHA:          cb.SHA,
		Branch:       cb.Branch,
		PatchID:      cb.PatchID,
		AuthorModel:  cb.AuthorModel,
		AuthorFamily: string(lookupFamily(cb.AuthorModel)),
		Tier:         cb.Tier,
		State:        StatePending,
		IngestedAt:   sv.now(),
		UpdatedAt:    sv.now(),
	}

	sv.cands[cb.SHA] = cand
	if cb.PatchID != "" {
		sv.shaIdx[cb.PatchID] = cb.SHA
	}
	sv.pendingCount++

	sv.appendRow(&Row{
		Event:        string(EventCompletion),
		SHA:          cb.SHA,
		Branch:       cb.Branch,
		PatchID:      cb.PatchID,
		AuthorModel:  cb.AuthorModel,
		AuthorFamily: cand.AuthorFamily,
		Tier:         string(cb.Tier),
		IngestedAt:   sv.nowISO(),
	})

	return true, staleSHA, nil
}

// --- Reviewer selection ---

// ReviewerPool is the set of available reviewer identities, each tagged with its model family.
type ReviewerEntry struct {
	Name  string
	Model string
}

// SelectReviewer picks a cross-family reviewer from the pool for a candidate.
// Returns ("", nil) if no reviewer is available (backpressure).
func (sv *ReviewSupervisor) SelectReviewer(candidateSHA string, pool []ReviewerEntry) (*ReviewerEntry, error) {
	sv.mu.RLock()
	cand, ok := sv.cands[candidateSHA]
	sv.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("reviewsup: unknown candidate %s", candidateSHA)
	}

	if cand.State != StatePending {
		return nil, nil
	}

	authorFamily := lookupFamily(cand.AuthorModel)
	needsCross := RequireCrossFamily(cand.Tier)

	for _, r := range pool {
		rFamily := lookupFamily(r.Model)
		if needsCross && !CrossFamilyOK(authorFamily, rFamily) {
			continue
		}
		return &r, nil
	}

	// Backpressure: no suitable reviewer available.
	return nil, nil
}

// --- Launch review (transition pending -> reviewing) ---

// LaunchReview transitions a candidate to reviewing state. Returns error if the
// candidate is not in the right state or doesn't exist.
func (sv *ReviewSupervisor) LaunchReview(candidateSHA, reviewer, reviewModel string) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	cand, ok := sv.cands[candidateSHA]
	if !ok {
		return fmt.Errorf("reviewsup: unknown candidate %s", candidateSHA)
	}
	if cand.State != StatePending {
		return fmt.Errorf("reviewsup: candidate %s is not pending (state=%s)", candidateSHA, cand.State)
	}

	reviewFamily := string(lookupFamily(reviewModel))
	cand.State = StateReviewing
	cand.Reviewer = reviewer
	cand.ReviewFamily = reviewFamily
	cand.UpdatedAt = sv.now()
	cand.Attempts++

	sv.appendRow(&Row{
		Event:        string(EventReview),
		SHA:          candidateSHA,
		Reviewer:     reviewer,
		ReviewFamily: reviewFamily,
		Tier:         string(cand.Tier),
		Attempts:     cand.Attempts,
	})

	return nil
}

// --- Verdict ingestion ---

// ReviewVerdict is what a reviewer returns.
type ReviewVerdict struct {
	SHA     string
	Reviewer string
	Verdict Verdict
	Reason  string
}

// SubmitVerdict processes a reviewer's verdict. On non-PASS, the candidate is
// returned to pending for re-review or marked blocked. On PASS, it enters the
// evidence queue.
func (sv *ReviewSupervisor) SubmitVerdict(v ReviewVerdict) (newState CandidateState, err error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	cand, ok := sv.cands[v.SHA]
	if !ok {
		return "", fmt.Errorf("reviewsup: unknown candidate %s", v.SHA)
	}

	cand.Verdict = v.Verdict
	cand.VerdictReason = v.Reason
	cand.UpdatedAt = sv.now()

	sv.appendRow(&Row{
		Event:    string(EventVerdict),
		SHA:      v.SHA,
		Reviewer: v.Reviewer,
		Verdict:  string(v.Verdict),
		Reason:   v.Reason,
	})

	switch v.Verdict {
	case VerdictPASS:
		cand.State = StatePass
		sv.pendingCount--

		// Enqueue for harvest.
		sv.appendQueue(&Row{
			Event:        string(EventHarvest),
			SHA:          v.SHA,
			Reviewer:     v.Reviewer,
			AuthorFamily: cand.AuthorFamily,
			ReviewFamily: cand.ReviewFamily,
			Tier:         string(cand.Tier),
			Attempts:     cand.Attempts,
		})
		cand.State = StateHarvested

	case VerdictFAIL:
		sv.pendingCount--
		if cand.Attempts >= sv.cfg.RetryLimit {
			cand.State = StateBlocked
			cand.Verdict = VerdictBLOCKED
		} else {
			// Return to pending for re-review.
			cand.State = StatePending
			cand.Reviewer = ""
			cand.ReviewFamily = ""
			cand.Verdict = ""
		}

	case VerdictBLOCKED:
		cand.State = StateBlocked
		sv.pendingCount--
	}

	return cand.State, nil
}

// --- Capacity / Backpressure ---

// PendingCount returns the number of candidates awaiting or undergoing review.
func (sv *ReviewSupervisor) PendingCount() int {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	return sv.pendingCount
}

// AtCapacity returns true when pending count >= configured max.
func (sv *ReviewSupervisor) AtCapacity() bool {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	return sv.pendingCount >= sv.cfg.MaxPendingReviews
}

// AvailableCapacity returns how many more reviews can be launched.
func (sv *ReviewSupervisor) AvailableCapacity() int {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	free := sv.cfg.MaxPendingReviews - sv.pendingCount
	if free < 0 {
		return 0
	}
	return free
}

// --- Queue management ---

// HarvestCandidate is an evidence bundle ready for integration.
type HarvestCandidate struct {
	SHA          string
	AuthorFamily string
	ReviewFamily string
	Tier         RiskTier
	Attempts     int
	HarvestedAt  time.Time
}

// ReadyForHarvest returns candidates with PASS verdicts, sorted oldest-first.
func (sv *ReviewSupervisor) ReadyForHarvest(max int) ([]HarvestCandidate, error) {
	sv.mu.RLock()
	defer sv.mu.RUnlock()

	qrows, err := readRows(sv.cfg.QueuePath)
	if err != nil {
		return nil, err
	}

	// Group by SHA, keep newest.
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

// MarkHarvested marks a candidate as consumed from the queue.
func (sv *ReviewSupervisor) MarkHarvested(sha string) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	if cand, ok := sv.cands[sha]; ok {
		cand.State = StateHarvested
		cand.UpdatedAt = sv.now()
	}

	// Re-read and rewrite queue, marking matching rows harvested.
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
	return nil
}

// --- Eviction / Staleness ---

// EvictStale transitions candidates past the staleness duration to evicted.
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
			cand.State = StateEvicted
			cand.UpdatedAt = sv.now()
			if cand.State == StatePending || cand.State == StateReviewing {
				sv.pendingCount--
			}
			sv.appendRow(&Row{
				Event:  string(EventEvict),
				SHA:    sha,
				Reason: "stale",
			})
			evicted++
		}
	}
	return evicted, nil
}

// --- Reconstruct from ledger ---

// Reconstruct rebuilds in-memory state from the durable ledger. Must be called
// at startup before any mutations. Returns count of candidates recovered.
func (sv *ReviewSupervisor) Reconstruct() (int, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	rows, err := readRows(sv.cfg.LedgerPath)
	if err != nil {
		return 0, fmt.Errorf("reviewsup: read ledger: %w", err)
	}

	sv.cands = make(map[string]*Candidate)
	sv.shaIdx = make(map[string]string)
	sv.pendingCount = 0
	sv.evrows = nil

	for _, r := range rows {
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
				SHA:          r.SHA,
				Branch:       r.Branch,
				PatchID:      r.PatchID,
				AuthorModel:  r.AuthorModel,
				AuthorFamily: r.AuthorFamily,
				Tier:         tier,
				State:        StatePending,
				IngestedAt:   ingestedAt,
				UpdatedAt:    sv.cfg.Now(),
			}
			sv.cands[r.SHA] = cand
			if r.PatchID != "" {
				sv.shaIdx[r.PatchID] = r.SHA
			}
			sv.pendingCount++

		case EventReview:
			if cand, ok := sv.cands[r.SHA]; ok {
				if cand.State == StatePending {
					cand.State = StateReviewing
					cand.Reviewer = r.Reviewer
					cand.ReviewFamily = r.ReviewFamily
					cand.Attempts = r.Attempts
				}
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
					sv.pendingCount--
					if cand.Attempts >= sv.cfg.RetryLimit {
						cand.State = StateBlocked
					} else {
						cand.State = StatePending
						cand.Reviewer = ""
						cand.ReviewFamily = ""
					}
				case VerdictBLOCKED:
					cand.State = StateBlocked
					sv.pendingCount--
				}
			}

		case EventSupersede:
			if cand, ok := sv.cands[r.PrevSHA]; ok {
				cand.State = StateEvicted
				if cand.State == StatePending || cand.State == StateReviewing || cand.State == StatePass {
					sv.pendingCount--
				}
			}

		case EventEvict:
			if cand, ok := sv.cands[r.SHA]; ok {
				cand.State = StateEvicted
				if cand.State == StatePending || cand.State == StateReviewing {
					sv.pendingCount--
				}
			}

		case EventCapacity:
			// Informational: capacity changes are read from config.
		}
	}

	// Check queue for harvested entries.
	qrows, err := readRows(sv.cfg.QueuePath)
	if err == nil {
		for _, r := range qrows {
			if r.Event == string(EventHarvest) && r.Harvested {
				if cand, ok := sv.cands[r.SHA]; ok {
					cand.State = StateHarvested
				}
			}
		}
	}

	return len(sv.cands), nil
}

// --- Status ---

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

// Candidate returns a single candidate by SHA, or nil.
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
