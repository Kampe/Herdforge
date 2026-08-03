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

type EventType string

const (
	EventCompletion EventType = "completion"
	EventReview     EventType = "review"
	EventVerdict    EventType = "verdict"
	EventSupersede  EventType = "supersede"
	EventEvict      EventType = "evict"
	EventHarvest    EventType = "harvest"
	EventCapacity   EventType = "capacity"
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
	Timestamp    string `json:"ts"`
	Event        string `json:"event"`
	SHA          string `json:"sha"`
	Branch       string `json:"branch,omitempty"`
	PatchID      string `json:"patch_id,omitempty"`
	AuthorModel  string `json:"author_model,omitempty"`
	AuthorFamily string `json:"author_family,omitempty"`
	Reviewer     string `json:"reviewer,omitempty"`
	ReviewFamily string `json:"review_family,omitempty"`
	Tier         string `json:"tier,omitempty"`
	Verdict      string `json:"verdict,omitempty"`
	Reason       string `json:"reason,omitempty"`
	PrevSHA      string `json:"prev_sha,omitempty"`
	Harvested    bool   `json:"harvested,omitempty"`
	Capacity     int    `json:"capacity,omitempty"`
	Attempts     int    `json:"attempts,omitempty"`
	IngestedAt   string `json:"ingested_at,omitempty"`
	LeaseID      string `json:"lease_id,omitempty"`
	Generation   int64  `json:"generation,omitempty"`
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
	Attempts      int
	IngestedAt    time.Time
	UpdatedAt     time.Time
	LeaseID       string
	LeaseExpiry   time.Time
	Generation    int64
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

type Config struct {
	LedgerPath        string
	QueuePath         string
	MaxPendingReviews int
	StaleDuration     time.Duration
	RetryLimit        int
	LeaseDuration     time.Duration
	Now               func() time.Time
}

func DefaultConfig(ledgerDir string) Config {
	return Config{
		LedgerPath:        filepath.Join(ledgerDir, "supervisor-ledger.jsonl"),
		QueuePath:         filepath.Join(ledgerDir, "harvest-evidence-queue.jsonl"),
		MaxPendingReviews: 3,
		StaleDuration:     24 * time.Hour,
		RetryLimit:        3,
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
}

func (sv *ReviewSupervisor) Ingest(cb CompletionCallback) (accepted bool, staleSHA string, err error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	if cb.SHA == "" {
		return false, "", fmt.Errorf("reviewsup: empty SHA in completion callback")
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
		SHA:          cb.SHA,
		Branch:       cb.Branch,
		PatchID:      cb.PatchID,
		AuthorModel:  cb.AuthorModel,
		AuthorFamily: string(lookupFamily(cb.AuthorModel)),
		Tier:         cb.Tier,
		State:        StatePending,
		IngestedAt:   sv.now(),
		UpdatedAt:    sv.now(),
		LeaseID:      cb.LeaseID,
		Generation:   cb.Generation,
	}

	if err := sv.appendRow(&Row{
		Event:        string(EventCompletion),
		SHA:          cb.SHA,
		Branch:       cb.Branch,
		PatchID:      cb.PatchID,
		AuthorModel:  cb.AuthorModel,
		AuthorFamily: cand.AuthorFamily,
		Tier:         string(cb.Tier),
		IngestedAt:   sv.nowISO(),
		LeaseID:      cb.LeaseID,
		Generation:   cb.Generation,
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
	Name  string
	Model string
}

func (sv *ReviewSupervisor) SelectReviewer(candidateSHA string, pool []ReviewerEntry) (*ReviewerEntry, error) {
	sv.mu.RLock()
	cand, ok := sv.cands[candidateSHA]
	sv.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("reviewsup: unknown candidate %s", candidateSHA)
	}

	if cand.State != StatePending {
		return nil, fmt.Errorf("reviewsup: candidate %s is not pending (state=%s)", candidateSHA, cand.State)
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

	return nil, nil
}

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

	reviewFamily := lookupFamily(reviewModel)
	authorFamily := lookupFamily(cand.AuthorModel)
	needsCross := RequireCrossFamily(cand.Tier)

	if needsCross && !CrossFamilyOK(authorFamily, reviewFamily) {
		return fmt.Errorf("reviewsup: candidate %s requires cross-family review (author=%s, reviewer=%s)", candidateSHA, authorFamily, reviewFamily)
	}

	cand.State = StateReviewing
	cand.Reviewer = reviewer
	cand.ReviewFamily = string(reviewFamily)
	cand.UpdatedAt = sv.now()
	cand.Attempts++

	if err := sv.appendRow(&Row{
		Event:        string(EventReview),
		SHA:          candidateSHA,
		Reviewer:     reviewer,
		ReviewFamily: string(reviewFamily),
		Tier:         string(cand.Tier),
		Attempts:     cand.Attempts,
	}); err != nil {
		cand.State = StatePending
		cand.Reviewer = ""
		cand.ReviewFamily = ""
		cand.Attempts--
		cand.UpdatedAt = sv.now()
		return fmt.Errorf("reviewsup: append review row: %w", err)
	}

	return nil
}

type ReviewVerdict struct {
	SHA      string
	Reviewer string
	Verdict  Verdict
	Reason   string
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

	cand.Verdict = v.Verdict
	cand.VerdictReason = v.Reason
	cand.UpdatedAt = sv.now()

	if err := sv.appendRow(&Row{
		Event:    string(EventVerdict),
		SHA:      v.SHA,
		Reviewer: v.Reviewer,
		Verdict:  string(v.Verdict),
		Reason:   v.Reason,
	}); err != nil {
		cand.Verdict = ""
		cand.VerdictReason = ""
		cand.UpdatedAt = sv.now()
		return "", fmt.Errorf("reviewsup: append verdict row: %w", err)
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
		}); err != nil {
			sv.pendingCount++
			cand.Verdict = ""
			cand.VerdictReason = ""
			cand.UpdatedAt = sv.now()
			return "", fmt.Errorf("reviewsup: append harvest row: %w", err)
		}
		cand.State = StateHarvested

	case VerdictFAIL:
		sv.pendingCount--
		if cand.Attempts >= sv.cfg.RetryLimit {
			cand.State = StateBlocked
			cand.Verdict = VerdictBLOCKED
		} else {
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
				LeaseID:      r.LeaseID,
				Generation:   r.Generation,
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

