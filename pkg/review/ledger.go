package review

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LedgerEvent types.
type LedgerEvent string

const (
	EventRecord   LedgerEvent = "record"
	EventVerdict  LedgerEvent = "verdict"
	EventRepair   LedgerEvent = "repair"
	EventConsumed LedgerEvent = "consumed"
	EventEnqueue  LedgerEvent = "enqueue"
	EventRevoked  LedgerEvent = "revoked"
)

// Verdict values.
type Verdict string

const (
	VerdictPASS    Verdict = "PASS"
	VerdictFAIL    Verdict = "FAIL"
	VerdictBLOCKED Verdict = "BLOCKED"
)

// LedgerFamilyAllowlist is the 11-token allowlist for builder families.
var LedgerFamilyAllowlist = map[string]bool{
	"anthropic":   true,
	"openai":      true,
	"google":      true,
	"xai":         true,
	"zhipu":       true,
	"moonshot":    true,
	"alibaba":     true,
	"deepseek":    true,
	"open-weight": true,
	"antigravity": true,
	"proxy":       true,
}

// LedgerRow matches the jq-emitted JSON exactly.
type LedgerRow struct {
	Timestamp      string `json:"ts"`
	Event          string `json:"event"`
	SHA            string `json:"sha,omitempty"`
	Branch         string `json:"branch,omitempty"`
	BuilderFamily  string `json:"builder_family,omitempty"`
	ReviewerFamily string `json:"reviewer_family,omitempty"`
	Reviewer       string `json:"reviewer,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Pane           string `json:"pane,omitempty"`
	Pid            string `json:"pid,omitempty"`
	Artifact       string `json:"artifact,omitempty"`
	Gate           string `json:"gate,omitempty"`
	Tier           string `json:"tier,omitempty"`
	Verdict        string `json:"verdict,omitempty"`
	RepairAuthor   string `json:"repair_author,omitempty"`
	RepairFamily   string `json:"repair_family,omitempty"`
	Lane           string `json:"lane,omitempty"`
	MergeSHA       string `json:"merge_sha,omitempty"`
	Status         string `json:"status,omitempty"`
}

// VerdictRecord is the small, stable view consumed by the drain coordinator.
// At is the parsed event time; Index is retained as the deterministic tie
// breaker for ledgers whose producers use the same timestamp.
type VerdictRecord struct {
	SHA           string
	Verdict       string
	Branch        string
	Tier          string
	BuilderFamily string
	At            int64 // Unix nanoseconds; zero means an omitted timestamp.
	Index         int
}

// OpenLedger opens an existing ledger without creating or mutating state.
// Drain uses this constructor so a missing ledger is an empty, readable
// review pile while an unreadable ledger remains a hard error.
func OpenLedger(path string) *Ledger {
	return &Ledger{Path: path, QueuePath: filepath.Join(filepath.Dir(path), "harvest-queue.jsonl")}
}

// LedgerSnapshot is the one-beat read of the two append-only streams.
type LedgerSnapshot struct {
	Rows  []LedgerRow
	Queue []LedgerRow
}

func (l *Ledger) Snapshot() (LedgerSnapshot, error) {
	rows, err := readRowsStrict(l.Path)
	if err != nil {
		return LedgerSnapshot{}, err
	}
	queue, err := readRowsStrict(l.QueuePath)
	if err != nil {
		return LedgerSnapshot{}, err
	}
	if err := validateLedgerRows(l.Path, rows); err != nil {
		return LedgerSnapshot{}, err
	}
	if err := validateLedgerRows(l.QueuePath, queue); err != nil {
		return LedgerSnapshot{}, err
	}
	if _, err := orderedEvents(rows); err != nil {
		return LedgerSnapshot{}, err
	}
	if _, err := orderedEvents(queue); err != nil {
		return LedgerSnapshot{}, err
	}
	return LedgerSnapshot{Rows: rows, Queue: queue}, nil
}

// Verdicts returns verdict events in file order. Last verdict wins is applied
// by PASSes and Vetoed, never by map iteration order.
func (l *Ledger) Verdicts(ctx context.Context) ([]VerdictRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := readRowsStrict(l.Path)
	if err != nil {
		return nil, err
	}
	events, err := orderedEvents(rows)
	if err != nil {
		return nil, err
	}
	result := make([]VerdictRecord, 0)
	for _, event := range events {
		row := event.row
		if row.Event != string(EventVerdict) {
			continue
		}
		at := int64(0)
		if ts, e := parseRowTime(row.Timestamp); e == nil {
			at = ts.UnixNano()
		}
		result = append(result, VerdictRecord{SHA: row.SHA, Verdict: row.Verdict, Branch: row.Branch, Tier: row.Tier, BuilderFamily: row.BuilderFamily, At: at, Index: event.index})
	}
	return result, nil
}

// PASSes returns PASS SHA -> recorded tier. A tier is taken from the verdict
// only when present, otherwise from the latest record for that SHA.
func (l *Ledger) PASSes(ctx context.Context) (map[string]string, error) {
	rows, err := readRowsStrict(l.Path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := orderedEvents(rows); err != nil {
		return nil, err
	}
	return l.passMap(rows), nil
}

// Vetoed returns SHAs whose latest verdict for any reviewer is FAIL/BLOCKED.
func (l *Ledger) Vetoed(ctx context.Context) (map[string]bool, error) {
	rows, err := readRowsStrict(l.Path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err = orderedRows(rows)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]LedgerRow)
	for _, row := range rows {
		if row.Event == string(EventVerdict) {
			latest[row.SHA+"\x00"+row.Reviewer] = row
		}
	}
	result := make(map[string]bool)
	for _, row := range latest {
		if row.Verdict == string(VerdictFAIL) || row.Verdict == string(VerdictBLOCKED) {
			result[row.SHA] = true
		}
	}
	return result, nil
}

// TierProp resolves the latest recorded tier for a SHA, or empty when the
// ledger has no recorded tier. It deliberately does not infer a tier.
func (l *Ledger) TierProp(ctx context.Context, sha string) string {
	rows, err := readRowsStrict(l.Path)
	if err != nil || ctx.Err() != nil {
		return ""
	}
	rows, err = orderedRows(rows)
	if err != nil {
		return ""
	}
	var tier string
	for _, row := range rows {
		if row.Event == string(EventRecord) && row.SHA == sha {
			tier = row.Tier
		}
	}
	return tier
}

func (l *Ledger) passMap(rows []LedgerRow) map[string]string {
	ordered, err := orderedRows(rows)
	if err != nil {
		return map[string]string{}
	}
	rows = ordered
	recordTier := make(map[string]string)
	latest := make(map[string]LedgerRow)
	for _, row := range rows {
		switch row.Event {
		case string(EventRecord):
			recordTier[row.SHA] = row.Tier
		case string(EventVerdict):
			latest[row.SHA+"\x00"+row.Reviewer] = row
		}
	}
	result := make(map[string]string)
	for _, row := range latest {
		if row.Verdict == string(VerdictPASS) {
			result[row.SHA] = recordTier[row.SHA]
		}
	}
	return result
}

func parseRowTime(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, raw)
}

func orderedRows(rows []LedgerRow) ([]LedgerRow, error) {
	items, err := orderedEvents(rows)
	if err != nil {
		return nil, err
	}
	out := make([]LedgerRow, len(items))
	for i, item := range items {
		out[i] = item.row
	}
	return out, nil
}

type orderedLedgerRow struct {
	row   LedgerRow
	index int
	at    time.Time
}

func orderedEvents(rows []LedgerRow) ([]orderedLedgerRow, error) {
	items := make([]orderedLedgerRow, 0, len(rows))
	for i, row := range rows {
		at, err := parseRowTime(row.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("invalid event timestamp at JSONL index %d: %w", i, err)
		}
		items = append(items, orderedLedgerRow{row: row, index: i, at: at})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].at.Equal(items[j].at) {
			return items[i].index < items[j].index
		}
		return items[i].at.Before(items[j].at)
	})
	return items, nil
}

// familyResolve carries the 3-state resolution so null!=empty maps to a bit.
type familyResolve int

const (
	familyUnset    familyResolve = iota // "" in both
	familySet                           // non-empty, authoritative
	familyConflict                      // both set but disagree
)

// familyState pairs the resolved value with its conflict status.
type familyState struct {
	Value string
	State familyResolve
}

// Ledger is an append-only JSONL review-attempt ledger + harvest queue.
type Ledger struct {
	RepoRoot     string
	Path         string
	QueuePath    string
	Now          func() time.Time
	Coordinators map[string]struct{}
}

// NewReviewLedger creates a Ledger, deriving QueuePath from ledgerPath's dir.
func NewReviewLedger(repoRoot, ledgerPath string) (*Ledger, error) {
	dir := filepath.Dir(ledgerPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create ledger dir: %w", err)
	}
	// Create if missing (: > equivalent)
	if _, err := os.Stat(ledgerPath); os.IsNotExist(err) {
		if f, err := os.Create(ledgerPath); err != nil {
			return nil, fmt.Errorf("create ledger: %w", err)
		} else {
			f.Close()
		}
	}
	queuePath := filepath.Join(dir, "harvest-queue.jsonl")
	if _, err := os.Stat(queuePath); os.IsNotExist(err) {
		if f, err := os.Create(queuePath); err != nil {
			return nil, fmt.Errorf("create queue: %w", err)
		} else {
			f.Close()
		}
	}
	return &Ledger{
		RepoRoot:     repoRoot,
		Path:         ledgerPath,
		QueuePath:    queuePath,
		Now:          time.Now,
		Coordinators: map[string]struct{}{"chainseer-orchestrator": {}, "coordinator": {}},
	}, nil
}

// nowISO returns the current timestamp in ISO 8601 format.
func (l *Ledger) nowISO() string {
	return l.Now().UTC().Format(time.RFC3339)
}

// appendRow writes a JSON row to the given file (append-only).
func (l *Ledger) appendRow(path string, row *LedgerRow) error {
	row.Timestamp = l.nowISO()
	data, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("marshal row: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		return fmt.Errorf("write newline: %w", err)
	}
	return nil
}

// readRows reads all rows from a JSONL file. Missing/unreadable returns empty.
func readRows(path string) ([]LedgerRow, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, nil // swallow errors (|| true semantics)
	}
	defer f.Close()
	var rows []LedgerRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row LedgerRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue // skip unparseable lines
		}
		rows = append(rows, row)
	}
	return rows, sc.Err()
}

func readRowsStrict(path string) ([]LedgerRow, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read ledger %s: %w", path, err)
	}
	defer f.Close()
	var rows []LedgerRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row LedgerRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("read ledger %s: invalid JSONL: %w", path, err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read ledger %s: %w", path, err)
	}
	return rows, nil
}

func validateLedgerRows(path string, rows []LedgerRow) error {
	for i, row := range rows {
		switch row.Event {
		case string(EventRecord), string(EventVerdict), string(EventEnqueue), string(EventRevoked), string(EventConsumed), string(EventRepair):
			if strings.TrimSpace(row.SHA) == "" {
				return fmt.Errorf("read ledger %s: JSONL index %d event %q is missing sha", path, i, row.Event)
			}
		}
	}
	return nil
}

// newestBy groups rows by key and keeps the last (newest) per key.
func newestBy(rows []LedgerRow, key func(*LedgerRow) string) map[string]LedgerRow {
	out := make(map[string]LedgerRow)
	for i := range rows {
		k := key(&rows[i])
		out[k] = rows[i]
	}
	return out
}

// NormalizeSHA returns the full object ID from git rev-parse, or the input unchanged.
func (l *Ledger) NormalizeSHA(sha string) string {
	cmd := exec.Command("git", "-C", l.RepoRoot, "rev-parse", "--verify", "-q", sha+"^{commit}")
	out, err := cmd.Output()
	if err != nil {
		return sha
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return sha
	}
	return s
}

// -- Appenders --

// RecordOpts carries optional fields for Record.
type RecordOpts struct {
	SHA            string
	Branch         string
	BuilderFamily  string
	ReviewerFamily string
	Reviewer       string
	Provider       string
	Model          string
	Pane           string
	Pid            string
	Artifact       string
	Gate           string
	Tier           string
}

// Record appends a record event.
func (l *Ledger) Record(opts RecordOpts) error {
	row := &LedgerRow{
		Event:          string(EventRecord),
		SHA:            opts.SHA,
		Branch:         opts.Branch,
		BuilderFamily:  opts.BuilderFamily,
		ReviewerFamily: opts.ReviewerFamily,
		Reviewer:       opts.Reviewer,
		Provider:       opts.Provider,
		Model:          opts.Model,
		Pane:           opts.Pane,
		Pid:            opts.Pid,
		Artifact:       opts.Artifact,
		Gate:           opts.Gate,
		Tier:           opts.Tier,
	}
	return l.appendRow(l.Path, row)
}

// Tier returns the newest recorded tier for a sha, or empty string.
func (l *Ledger) Tier(sha string) (string, error) {
	rows, err := readRows(l.Path)
	if err != nil {
		return "", err
	}
	sha = l.NormalizeSHA(sha)
	var tier string
	for _, r := range rows {
		if r.Event == string(EventRecord) && r.SHA == sha && r.Tier != "" {
			tier = r.Tier
		}
	}
	return tier, nil
}

// VerdictOpts carries fields for Verdict.
type VerdictOpts struct {
	SHA            string
	Reviewer       string
	Verdict        Verdict
	Artifact       string
	ReviewerFamily string
	BuilderFamily  string
	Branch         string
	Lane           string
}

// Verdict appends a verdict event and side-writes to the queue.
func (l *Ledger) Verdict(opts VerdictOpts) (enqueued bool, err error) {
	row := &LedgerRow{
		Event:    string(EventVerdict),
		SHA:      opts.SHA,
		Reviewer: opts.Reviewer,
		Verdict:  string(opts.Verdict),
		Artifact: opts.Artifact,
	}
	if opts.ReviewerFamily != "" {
		row.ReviewerFamily = opts.ReviewerFamily
	}
	if opts.BuilderFamily != "" {
		row.BuilderFamily = opts.BuilderFamily
	}
	if err := l.appendRow(l.Path, row); err != nil {
		return false, err
	}

	// Side-write to queue.
	qRow := &LedgerRow{
		Event:    string(EventEnqueue),
		SHA:      opts.SHA,
		Reviewer: opts.Reviewer,
		Branch:   opts.Branch,
		Lane:     opts.Lane,
		Status:   "queued",
	}
	if opts.Verdict == VerdictFAIL || opts.Verdict == VerdictBLOCKED {
		qRow.Event = string(EventRevoked)
		qRow.Verdict = string(opts.Verdict)
		qRow.Status = "revoked"
	}
	if err := l.appendRow(l.QueuePath, qRow); err != nil {
		return false, err
	}
	return opts.Verdict == VerdictPASS, nil
}

// RepairOpts carries fields for Repair.
type RepairOpts struct {
	SHA          string
	RepairAuthor string
	Branch       string
	RepairFamily string
}

// Repair appends a repair event.
func (l *Ledger) Repair(opts RepairOpts) error {
	row := &LedgerRow{
		Event:        string(EventRepair),
		SHA:          opts.SHA,
		RepairAuthor: opts.RepairAuthor,
		Branch:       opts.Branch,
	}
	if opts.RepairFamily != "" {
		row.RepairFamily = opts.RepairFamily
	}
	return l.appendRow(l.Path, row)
}

// Consumed marks a sha as consumed in both ledger and queue.
func (l *Ledger) Consumed(sha, mergeSHA string) error {
	row := &LedgerRow{
		Event:    string(EventConsumed),
		SHA:      sha,
		MergeSHA: mergeSHA,
	}
	if err := l.appendRow(l.Path, row); err != nil {
		return err
	}
	qRow := &LedgerRow{
		Event:    string(EventConsumed),
		SHA:      sha,
		MergeSHA: mergeSHA,
		Status:   "consumed",
	}
	return l.appendRow(l.QueuePath, qRow)
}

// EnqueueOpts carries fields for manual enqueue.
type EnqueueOpts struct {
	SHA      string
	Reviewer string
	Branch   string
	Lane     string
}

// Enqueue manually appends an enqueue event to the queue.
func (l *Ledger) Enqueue(opts EnqueueOpts) error {
	reviewer := opts.Reviewer
	if reviewer == "" {
		reviewer = "manual"
	}
	row := &LedgerRow{
		Event:    string(EventEnqueue),
		SHA:      opts.SHA,
		Reviewer: reviewer,
		Branch:   opts.Branch,
		Lane:     opts.Lane,
		Status:   "queued",
	}
	return l.appendRow(l.QueuePath, row)
}

// -- Queries --

// AllRows returns every row from the ledger.
func (l *Ledger) AllRows() ([]LedgerRow, error) {
	return readRows(l.Path)
}

// QueueRows returns every row from the queue.
func (l *Ledger) QueueRows() ([]LedgerRow, error) {
	return readRows(l.QueuePath)
}

// Eligible returns true if sha is harvestable for the given builderFamily.
// Empty builderFamily means "no filter" — passes when launch family is allowlisted.
func (l *Ledger) Eligible(sha, builderFamily string) (bool, error) {
	rows, err := readRows(l.Path)
	if err != nil {
		return false, err
	}
	qrows, err := readRows(l.QueuePath)
	if err != nil {
		return false, err
	}

	sha = l.NormalizeSHA(sha)

	// build launch map: newest record per (sha, reviewer)
	launch := make(map[string]LedgerRow) // key = sha:reviewer
	for _, r := range rows {
		if r.Event == string(EventRecord) {
			k := r.SHA + ":" + r.Reviewer
			launch[k] = r
		}
	}

	// build latest verdict map: newest verdict per (sha, reviewer)
	latest := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventVerdict) {
			k := r.SHA + ":" + r.Reviewer
			latest[k] = r
		}
	}

	// consumed set
	done := make(map[string]bool)
	for _, r := range qrows {
		if r.Event == string(EventConsumed) {
			done[r.SHA] = true
		}
	}

	if done[sha] {
		return false, nil
	}

	// Check if this sha's reviewers have a PASS with no FAIL/BLOCKED,
	// using the family ladder.
	var hasPass bool
	for k, verdict := range latest {
		sparts := strings.SplitN(k, ":", 2)
		if len(sparts) != 2 || sparts[0] != sha {
			continue
		}
		reviewer := sparts[1]

		// Exclude coordinators
		if _, isCoord := l.Coordinators[reviewer]; isCoord {
			continue
		}

		launchRow, hasLaunch := launch[k]

		// Family ladder
		gate := "independent"
		var lbf, lrf, vrf, vbf string
		if hasLaunch {
			if launchRow.Gate != "" {
				gate = launchRow.Gate
			}
			lbf = launchRow.BuilderFamily
			lrf = launchRow.ReviewerFamily
		}
		vrf = verdict.ReviewerFamily
		vbf = verdict.BuilderFamily

		var rf string
		if lrf != "" && vrf != "" && lrf != vrf {
			rf = "" // conflict → null in jq speak
		} else if lbf != "" && vbf != "" && lbf != vbf {
			rf = "" // conflict fail-closed
		} else if lrf != "" {
			rf = lrf
		} else {
			rf = vrf
		}

		// Only PASS verdicts qualify
		if verdict.Verdict != string(VerdictPASS) {
			continue
		}

		// Now evaluate eligibility
		if gate == "mechanical" && (reviewer == "mechanical" || rf == "mechanical") {
			hasPass = true
			continue
		}

		if lbf == "" || !LedgerFamilyAllowlist[lbf] {
			continue
		}

		if builderFamily == "" {
			hasPass = true
			continue
		}

		if rf != "" && rf != builderFamily {
			hasPass = true
			continue
		}
	}

	if !hasPass {
		// Check for coordinator self-verification
		for k, verdict := range latest {
			sparts := strings.SplitN(k, ":", 2)
			if len(sparts) != 2 || sparts[0] != sha {
				continue
			}
			reviewer := sparts[1]
			if _, isCoord := l.Coordinators[reviewer]; isCoord {
				// Check if this is the only reviewer
				onlyCoord := true
				for k2 := range latest {
					sp := strings.SplitN(k2, ":", 2)
					if len(sp) == 2 && sp[0] == sha && !l.isCoordinator(sp[1]) {
						onlyCoord = false
						break
					}
				}
				if onlyCoord && verdict.Verdict == string(VerdictPASS) {
					return false, fmt.Errorf("A coordinator self-verification never qualifies.\nherd-review-ledger: refuse sha=%s reviewer=%s verdict=%s", sha, reviewer, verdict.Verdict)
				}
			}
		}
		return false, fmt.Errorf("herd-review-ledger: refuse sha=%s", sha)
	}

	return true, nil
}

func (l *Ledger) isCoordinator(name string) bool {
	_, ok := l.Coordinators[name]
	return ok
}

// isPassVerdictLatest checks whether the latest verdict set for a sha has
// any PASS and no FAIL/BLOCKED, using the family ladder.
func (l *Ledger) isPassVerdictLatest(sha string, latest map[string]LedgerRow, launch map[string]LedgerRow) bool {
	var hasPass bool
	for k, verdict := range latest {
		sparts := strings.SplitN(k, ":", 2)
		if len(sparts) != 2 || sparts[0] != sha {
			continue
		}
		reviewer := sparts[1]
		if _, isCoord := l.Coordinators[reviewer]; isCoord {
			continue
		}
		launchRow, hasLaunch := launch[k]

		gate := "independent"
		var lbf, lrf, vrf, vbf string
		if hasLaunch {
			if launchRow.Gate != "" {
				gate = launchRow.Gate
			}
			lbf = launchRow.BuilderFamily
			lrf = launchRow.ReviewerFamily
		}
		vrf = verdict.ReviewerFamily
		vbf = verdict.BuilderFamily

		var rf string
		if lrf != "" && vrf != "" && lrf != vrf {
			rf = ""
		} else if lbf != "" && vbf != "" && lbf != vbf {
			rf = ""
		} else if lrf != "" {
			rf = lrf
		} else {
			rf = vrf
		}

		if gate == "mechanical" && (reviewer == "mechanical" || rf == "mechanical") {
			if verdict.Verdict == string(VerdictPASS) {
				hasPass = true
			}
			if verdict.Verdict == string(VerdictFAIL) || verdict.Verdict == string(VerdictBLOCKED) {
				return false
			}
			continue
		}

		if lbf == "" || !LedgerFamilyAllowlist[lbf] {
			continue
		}

		if verdict.Verdict == string(VerdictPASS) {
			hasPass = true
		}
		if verdict.Verdict == string(VerdictFAIL) || verdict.Verdict == string(VerdictBLOCKED) {
			return false
		}
	}
	return hasPass
}

// Queued returns the candidate harvest list.
func (l *Ledger) Queued() ([]LedgerRow, error) {
	rows, err := l.AllRows()
	if err != nil {
		return nil, err
	}
	qrows, err := l.QueueRows()
	if err != nil {
		return nil, err
	}

	// consumed set
	done := make(map[string]bool)
	for _, r := range qrows {
		if r.Event == string(EventConsumed) {
			done[r.SHA] = true
		}
	}

	// launch map: newest record per (sha, reviewer)
	launch := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventRecord) {
			k := r.SHA + ":" + r.Reviewer
			launch[k] = r
		}
	}

	// latest verdict map
	latest := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventVerdict) {
			k := r.SHA + ":" + r.Reviewer
			latest[k] = r
		}
	}

	// queue: enqueue events grouped by sha, keep newest per sha
	type qentry struct {
		row   LedgerRow
		order int
	}
	shaEnqueues := make(map[string]qentry)
	for i, r := range qrows {
		if r.Event == string(EventEnqueue) {
			shaEnqueues[r.SHA] = qentry{row: r, order: i}
		}
	}

	var result []LedgerRow
	for sha, eq := range shaEnqueues {
		if done[sha] {
			continue
		}
		if l.isPassVerdictLatest(sha, latest, launch) {
			result = append(result, eq.row)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Timestamp < result[j].Timestamp
	})
	return result, nil
}

// Pending returns launched-and-not-yet-verdict records, order-based.
func (l *Ledger) Pending() ([]LedgerRow, error) {
	rows, err := l.AllRows()
	if err != nil {
		return nil, err
	}

	// Map verdict rows to their index in the file.
	verdictIdx := make(map[string]int) // key = sha:reviewer -> last index
	for i, r := range rows {
		if r.Event == string(EventVerdict) {
			k := r.SHA + ":" + r.Reviewer
			verdictIdx[k] = i
		}
	}

	// Newest record per (sha, reviewer).
	type recEntry struct {
		row   LedgerRow
		index int
	}
	newestRec := make(map[string]recEntry)
	for i, r := range rows {
		if r.Event == string(EventRecord) {
			k := r.SHA + ":" + r.Reviewer
			newestRec[k] = recEntry{row: r, index: i}
		}
	}

	var pending []LedgerRow
	for k, rec := range newestRec {
		vi, hasVerdict := verdictIdx[k]
		if !hasVerdict || vi < rec.index {
			pending = append(pending, rec.row)
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		return pending[i].Timestamp < pending[j].Timestamp
	})
	return pending, nil
}

// PassSHAs returns distinct shas with PASS and no FAIL/BLOCKED in latest verdicts, no cap.
func (l *Ledger) PassSHAs() ([]string, error) {
	rows, err := l.AllRows()
	if err != nil {
		return nil, err
	}

	launch := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventRecord) {
			k := r.SHA + ":" + r.Reviewer
			launch[k] = r
		}
	}

	latest := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventVerdict) {
			k := r.SHA + ":" + r.Reviewer
			latest[k] = r
		}
	}

	// Group verdicts by sha
	shaVerdicts := make(map[string][]LedgerRow)
	for k, v := range latest {
		sparts := strings.SplitN(k, ":", 2)
		if len(sparts) == 2 {
			shaVerdicts[sparts[0]] = append(shaVerdicts[sparts[0]], v)
		}
	}

	var shas []string
	for sha, vset := range shaVerdicts {
		hasPass := false
		hasVeto := false
		hasIndependent := false
		for _, verdict := range vset {
			reviewer := verdict.Reviewer
			if _, isCoord := l.Coordinators[reviewer]; isCoord {
				continue
			}
			lk := sha + ":" + reviewer
			lr, hasLaunch := launch[lk]
			if !hasLaunch {
				continue
			}
			lbf := lr.BuilderFamily
			if lbf == "" || !LedgerFamilyAllowlist[lbf] {
				continue
			}

			gate := lr.Gate
			if gate == "" {
				gate = "independent"
			}
			lrf := lr.ReviewerFamily

			hasIndependent = true

			if gate == "mechanical" && (reviewer == "mechanical" || lrf == "mechanical") {
				if verdict.Verdict == string(VerdictPASS) {
					hasPass = true
				}
				continue
			}

			if verdict.Verdict == string(VerdictPASS) {
				hasPass = true
			}
			if verdict.Verdict == string(VerdictFAIL) || verdict.Verdict == string(VerdictBLOCKED) {
				hasVeto = true
			}
		}
		if hasIndependent && hasPass && !hasVeto {
			shas = append(shas, sha)
		}
	}

	// Sort newest-first by timestamp of newest record
	sort.Slice(shas, func(i, j int) bool {
		return shas[i] > shas[j]
	})
	return shas, nil
}

// VetoSHAs returns distinct shas with any FAIL/BLOCKED in latest verdicts.
func (l *Ledger) VetoSHAs() ([]string, error) {
	rows, err := l.AllRows()
	if err != nil {
		return nil, err
	}

	launch := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventRecord) {
			k := r.SHA + ":" + r.Reviewer
			launch[k] = r
		}
	}

	latest := make(map[string]LedgerRow)
	for _, r := range rows {
		if r.Event == string(EventVerdict) {
			k := r.SHA + ":" + r.Reviewer
			latest[k] = r
		}
	}

	shaVerdicts := make(map[string][]LedgerRow)
	for k, v := range latest {
		sparts := strings.SplitN(k, ":", 2)
		if len(sparts) == 2 {
			shaVerdicts[sparts[0]] = append(shaVerdicts[sparts[0]], v)
		}
	}

	var shas []string
	for sha, vset := range shaVerdicts {
		hasVeto := false
		for _, verdict := range vset {
			reviewer := verdict.Reviewer
			if _, isCoord := l.Coordinators[reviewer]; isCoord {
				continue
			}
			lk := sha + ":" + reviewer
			lr, hasLaunch := launch[lk]
			if !hasLaunch {
				continue
			}
			lbf := lr.BuilderFamily
			if lbf == "" || !LedgerFamilyAllowlist[lbf] {
				continue
			}

			if verdict.Verdict == string(VerdictFAIL) || verdict.Verdict == string(VerdictBLOCKED) {
				hasVeto = true
			}
		}
		if hasVeto {
			shas = append(shas, sha)
		}
	}

	sort.Slice(shas, func(i, j int) bool {
		return shas[i] > shas[j]
	})
	return shas, nil
}
