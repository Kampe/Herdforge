package review

import (
	"errors"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/procsignal"
	"github.com/Kampe/Herdforge/pkg/provider"
)

type ConflictState string

const (
	ConflictClean   ConflictState = "clean"
	ConflictRebase  ConflictState = "rebase"
	ConflictUnknown ConflictState = "unknown"
)

type PinFreshness struct {
	SHA          string        `json:"sha"`
	Lane         string        `json:"lane,omitempty"`
	Branch       string        `json:"branch,omitempty"`
	WorktreePath string        `json:"worktree_path,omitempty"`
	Behind       int           `json:"behind"`
	Conflict     ConflictState `json:"conflict"`
	Note         string        `json:"note"`
}

// Drain is the dependency-light coordinator scan. Board access is injected so
// review remains usable in offline fixtures and cannot silently guess a board.
type Drain struct {
	// Progress, when set, is called before each tip is probed so a caller can
	// measure real per-item cost instead of guessing a budget.
	Progress func(done, total int, branch string)

	// ProbeBudget bounds ONE content-merge probe. Zero uses the default.
	ProbeBudget time.Duration

	RepoRoot, StateDir, LedgerPath, LedgerBin string
	Cap, MaxRelaunch, StaleBehind             int
	ArtifactDir                               string
	Ledger                                    *Ledger
	Provider                                  provider.TaskProvider
	KaneoProject                              string
	WindDown                                  bool
}
type Pipeline = Drain

type DrainShas struct {
	Harvestable   []string `json:"harvestable_shas"`
	NeedReview    []string `json:"need_review_shas"`
	ContentMerged []string `json:"content_merged_already_shas"`
	ReviewPass    []string `json:"review_pass_shas"`
	HarvestReady  []string `json:"harvest_ready_shas"`
	RebaseNeeded  []string `json:"rebase_needed_shas"`
}

// DrainReport.NeedReview is the live unmerged-candidate set discovered by the
// drain scan. It is distinct from pulse.ReviewObservation.RawVetoed, which is
// the review ledger's unfiltered, unexpired vetoed-SHA set.
// SlowTip is one tip whose probe blew the per-probe bound.
type SlowTip struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	Budget string `json:"budget"`
}

type DrainReport struct {
	// SlowTips are tips whose individual probe exceeded the per-probe bound.
	// Named so a pathological object can be investigated directly.
	SlowTips []SlowTip `json:"slow_tips,omitempty"`

	// ScanTruncated reports that the tip loop stopped on its deadline, so
	// dispositions cover ScannedTips of TotalTips only. A truncated report must
	// never be read as a complete drain decision.
	ScanTruncated bool `json:"scan_truncated,omitempty"`
	ScannedTips   int  `json:"scanned_tips,omitempty"`
	TotalTips     int  `json:"total_tips,omitempty"`

	WindDown                                                                                                                                                    bool `json:"wind_down"`
	Pending, HarvestQueue, RefactoringCount, Harvestable, NeedReview, ReviewPass, HarvestReady, ContentMerged, KaneoInReview, Cap, RebaseNeeded, StaleBehindMax int
	Pressure                                                                                                                                                    string `json:"pressure"`
	KaneoOK                                                                                                                                                     bool   `json:"kaneo_ok"`
	KaneoProject                                                                                                                                                string `json:"kaneo_project"`
	KaneoError                                                                                                                                                  string `json:"kaneo_error"`
	ParkBranches                                                                                                                                                int    `json:"park_branches"`
	ParkCHAWithDups                                                                                                                                             int    `json:"park_cha_with_dups"`
	Skips7d, LedgerPass, Rejected                                                                                                                               int
	Shas                                                                                                                                                        DrainShas             `json:"-"`
	Pins                                                                                                                                                        []PinFreshness        `json:"-"`
	BoardGit                                                                                                                                                    []BoardGitRow         `json:"-"`
	Errors                                                                                                                                                      []string              `json:"-"`
	ActionEvidence                                                                                                                                              []DrainActionEvidence `json:"-"`
	StandingLanes                                                                                                                                               []string              `json:"-"`
}

// DrainActionEvidence is projected from the same ordered ledger snapshot as
// the report. It is intentionally excluded from the fixed JSON packet.
type DrainActionEvidence struct {
	SHA, Branch, Lane, BuilderFamily, Tier string
	TierRecorded, Pending, Vetoed          bool
	HarvestReady, RebaseNeeded             bool
}

type BoardGitRow struct {
	Ref, Title, Tip string
	Main, Park      bool
}

func (s LedgerSnapshot) Pending() []LedgerRow {
	recordIndex, verdictIndex := map[string]int{}, map[string]int{}
	for i, row := range s.Rows {
		key := row.SHA + "\x00" + row.Reviewer
		if row.Event == string(EventRecord) {
			recordIndex[key] = i
		}
		if row.Event == string(EventVerdict) {
			verdictIndex[key] = i
		}
	}
	type pendingRow struct {
		index int
		row   LedgerRow
	}
	ordered := make([]pendingRow, 0)
	for key, index := range recordIndex {
		if verdict, ok := verdictIndex[key]; !ok || verdict < index {
			ordered = append(ordered, pendingRow{index: index, row: s.Rows[index]})
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	out := make([]LedgerRow, len(ordered))
	for i, item := range ordered {
		out[i] = item.row
	}
	return out
}

func (s LedgerSnapshot) Vetoed() map[string]bool {
	latest := map[string]LedgerRow{}
	for _, row := range s.Rows {
		if row.Event == string(EventVerdict) {
			latest[row.SHA+"\x00"+row.Reviewer] = row
		}
	}
	out := map[string]bool{}
	for _, row := range latest {
		if row.Verdict == string(VerdictFAIL) || row.Verdict == string(VerdictBLOCKED) {
			out[row.SHA] = true
		}
	}
	return out
}

func NewPipeline(d Drain) *Pipeline { return &d }

func (r *DrainReport) MarshalJSON() ([]byte, error) {
	type packet struct {
		WindDown         bool    `json:"wind_down"`
		Pending          int     `json:"pending"`
		HarvestQueue     int     `json:"harvest_queue"`
		RefactoringCount int     `json:"refactoring_count"`
		Harvestable      int     `json:"harvestable"`
		NeedReview       int     `json:"need_review"`
		ReviewPass       int     `json:"review_pass"`
		HarvestReady     int     `json:"harvest_ready"`
		ContentMerged    int     `json:"content_merged_already"`
		KaneoInReview    int     `json:"kaneo_in_review"`
		Cap              int     `json:"cap"`
		Pressure         string  `json:"pressure"`
		KaneoOK          bool    `json:"kaneo_ok"`
		KaneoProject     *string `json:"kaneo_project"`
		KaneoError       *string `json:"kaneo_error"`
		ParkBranches     int     `json:"park_branches"`
		ParkCHAWithDups  int     `json:"park_cha_with_dups"`
		Skips7d          int     `json:"review_gate_skips_7d"`
		LedgerPass       int     `json:"ledger_pass"`
		Rejected         int     `json:"review_artifacts_rejected"`
		RebaseNeeded     int     `json:"rebase_needed"`
		StaleBehindMax   int     `json:"stale_behind_max"`
		DrainShas
	}
	var project, kerr *string
	if r.KaneoProject != "" {
		project = &r.KaneoProject
	}
	if r.KaneoError != "" {
		kerr = &r.KaneoError
	}
	return json.Marshal(packet{r.WindDown, r.Pending, r.HarvestQueue, r.RefactoringCount, r.Harvestable, r.NeedReview, r.ReviewPass, r.HarvestReady, r.ContentMerged, r.KaneoInReview, r.Cap, r.Pressure, r.KaneoOK, project, kerr, r.ParkBranches, r.ParkCHAWithDups, r.Skips7d, r.LedgerPass, r.Rejected, r.RebaseNeeded, r.StaleBehindMax, r.Shas})
}

func (d *Drain) Scan(ctx context.Context, unmerged []harvest.UnmergedWork) (*DrainReport, error) {
	if d.RepoRoot == "" {
		d.RepoRoot = "."
	}
	if d.Cap <= 0 {
		d.Cap = 8
	}
	if d.StaleBehind <= 0 {
		d.StaleBehind = 20
	}
	if d.Ledger == nil {
		d.Ledger = OpenLedger(d.LedgerPath)
	}
	snap, err := d.Ledger.Snapshot()
	if err != nil {
		return nil, err
	}
	pass := d.Ledger.passMap(snap.Rows)
	veto := snap.Vetoed()
	queued := queuePins(snap, pass, veto)
	pending := snap.Pending()
	records := map[string]LedgerRow{}
	for _, row := range snap.Rows {
		if row.Event == string(EventRecord) {
			records[row.SHA] = row
		}
	}
	pendingSHA := map[string]bool{}
	for _, row := range pending {
		pendingSHA[row.SHA] = true
	}
	tips, queueLanes := buildTipSet(unmerged, queued)

	// The legacy packet key is retained, but this coordinator has no
	// authoritative refactoring probe. -1 is explicit unknown, never a count
	// inferred from the unmerged worktree list.
	r := &DrainReport{Pending: len(pending), HarvestQueue: len(queued), Cap: d.Cap, RefactoringCount: -1, StaleBehindMax: d.StaleBehind, KaneoOK: false, KaneoInReview: -1, Shas: DrainShas{}, WindDown: d.WindDown || envWindDown()}
	var parkErr error
	r.ParkBranches, r.ParkCHAWithDups, parkErr = parkStats(ctx, d.RepoRoot)
	if parkErr != nil {
		r.ParkBranches, r.ParkCHAWithDups = -1, -1
		r.Errors = append(r.Errors, parkErr.Error())
	}
	for _, row := range snap.Rows {
		if row.Event == string(EventVerdict) && row.Verdict == string(VerdictPASS) {
			r.LedgerPass++
		}
	}
	var countErr error
	r.Skips7d, countErr = recentCount(filepath.Join(d.StateDir, "review-gate-skips.log"), time.Now().Add(-7*24*time.Hour))
	if countErr != nil {
		r.Skips7d = -1
		r.Errors = append(r.Errors, countErr.Error())
	}
	r.Rejected, countErr = rejectedCount(d.ArtifactDir)
	if countErr != nil {
		r.Rejected = -1
		r.Errors = append(r.Errors, countErr.Error())
	}
	for sha := range pass {
		r.Shas.ReviewPass = append(r.Shas.ReviewPass, sha)
	}
	sort.Strings(r.Shas.ReviewPass)
	r.ReviewPass = len(r.Shas.ReviewPass)
	for i, u := range tips {
		// FAC-560: the per-tip work is a git merge-tree probe, so this loop is
		// the O(N) cost. Report progress and stop cleanly on deadline: the scan
		// previously consumed its whole budget and returned nothing, so neither
		// the operator nor I could measure the real per-item cost and every
		// budget was a guess.
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Fail closed: the error is returned so no caller mistakes a
			// truncated scan for a completed one. The report is returned
			// ALONGSIDE it so the caller can show what was reached and the
			// observed per-item cost.
			r.ScanTruncated = true
			r.ScannedTips = i
			r.TotalTips = len(tips)
			return r, ctxErr
		}
		sha := u.Unmerged[0]
		if d.Progress != nil {
			// Identify the tip even when it has no branch: queued ledger pins
			// carry no worktree, and an unlabeled progress line makes a
			// pathological object unfindable.
			label := u.Branch
			if label == "" {
				label = "sha:" + shortSHA(sha)
			}
			d.Progress(i, len(tips), label)
		}
		// FAC-562: bound the WHOLE per-tip iteration, not just the content-merge
		// probe. The reported 1m49s items were spent in freshness (behind count
		// and conflict probe), which was outside the earlier probe-only bound,
		// so the 20s cap never applied to the cost that actually mattered.
		itemCtx, cancelItem := context.WithTimeout(ctx, d.probeBudget())
		pin := d.freshness(itemCtx, sha, u.Branch, u.WorktreePath)
		if u.WorktreePath == "" && queueLanes[sha] != "" {
			pin.Lane = queueLanes[sha]
		}
		r.Pins = append(r.Pins, pin)
		record := records[sha]
		r.ActionEvidence = append(r.ActionEvidence, DrainActionEvidence{SHA: sha, Branch: pin.Branch, Lane: pin.Lane, BuilderFamily: strings.ToLower(strings.TrimSpace(record.BuilderFamily)), Tier: record.Tier, TierRecorded: strings.TrimSpace(record.Tier) != "", Pending: pendingSHA[sha], Vetoed: veto[sha]})
		// FAC-561: a single tip was observed consuming 1m56.294s while its peers
		// were sub-second, eating a whole budget by itself. Each probe is
		// therefore individually bounded: a pathological object is reported by
		// name instead of silently starving every remaining tip.
		merged, probeErr := harvest.ContentMerged(itemCtx, d.RepoRoot, "origin/main", sha)
		// Read the per-item deadline state BEFORE cancelling it. cancel() sets
		// Err() to Canceled, so checking after cancel reports every fast
		// failure as a timeout -- which misclassified unprobeable objects as
		// merely slow and broke the fail-closed contract.
		probeTimedOut := errors.Is(itemCtx.Err(), context.DeadlineExceeded)
		cancelItem()
		if probeErr != nil {
			// Distinguish OUR per-probe bound from unknown git evidence. A slow
			// object is a cost problem; an unprobeable object is an evidence
			// problem, and only the latter may abort the scan.
			if probeTimedOut && ctx.Err() == nil {
				r.ScanTruncated = true
				r.ScannedTips = i
				r.TotalTips = len(tips)
				r.SlowTips = append(r.SlowTips, SlowTip{Branch: u.Branch, SHA: sha, Budget: d.probeBudget().String()})
				continue
			}
			// The OUTER deadline commonly lands inside a probe rather than at
			// the top-of-loop check. Returning nil here is why the measured
			// per-tip summary never printed for the consumer: the caller had no
			// report to read. Fail closed with the error, but hand back the
			// partial alongside it.
			if ctx.Err() != nil {
				r.ScanTruncated = true
				r.ScannedTips = i
				r.TotalTips = len(tips)
				return r, ctx.Err()
			}
			// Unknown git evidence FAILS CLOSED. Continuing past an unprobeable
			// object was tried and correctly rejected by
			// TestPipelineContract_UnknownEvidenceFailsClosed: a disposition set
			// built around an object we could not verify invites treating it as
			// merged or harvestable, and that invariant outranks scan progress.
			r.ScannedTips, r.TotalTips = i, len(tips)
			return nil, fmt.Errorf("content-merge probe for %s: %w", sha, probeErr)
		}
		if probeErr == nil && probeTimedOut {
			// freshness consumed the item budget; the probe then had none left
			// yet returned no error. Name it rather than silently recording a
			// disposition derived from a truncated measurement.
			r.SlowTips = append(r.SlowTips, SlowTip{Branch: u.Branch, SHA: sha, Budget: d.probeBudget().String()})
			r.ScanTruncated = true
			continue
		}
		if merged {
			r.Shas.ContentMerged = append(r.Shas.ContentMerged, sha)
			continue
		}
		if veto[sha] {
			r.Shas.NeedReview = append(r.Shas.NeedReview, sha)
			continue
		}
		if _, ok := pass[sha]; ok {
			r.Shas.Harvestable = append(r.Shas.Harvestable, sha)
			if pin.Conflict == ConflictClean && pin.Behind >= 0 && pin.Behind <= d.StaleBehind {
				r.Shas.HarvestReady = append(r.Shas.HarvestReady, sha)
			} else {
				r.Shas.RebaseNeeded = append(r.Shas.RebaseNeeded, sha)
			}
		} else {
			r.Shas.NeedReview = append(r.Shas.NeedReview, sha)
		}
		evidence := &r.ActionEvidence[len(r.ActionEvidence)-1]
		if containsSHA(r.Shas.HarvestReady, sha) {
			evidence.HarvestReady = true
		}
		if containsSHA(r.Shas.RebaseNeeded, sha) {
			evidence.RebaseNeeded = true
		}
	}
	for _, p := range [][]string{r.Shas.Harvestable, r.Shas.NeedReview, r.Shas.ContentMerged, r.Shas.HarvestReady, r.Shas.RebaseNeeded} {
		sort.Strings(p)
	}
	r.Harvestable = len(r.Shas.Harvestable)
	r.NeedReview = len(r.Shas.NeedReview)
	r.ContentMerged = len(r.Shas.ContentMerged)
	r.HarvestReady = len(r.Shas.HarvestReady)
	r.RebaseNeeded = len(r.Shas.RebaseNeeded)
	sort.Slice(r.ActionEvidence, func(i, j int) bool { return r.ActionEvidence[i].SHA < r.ActionEvidence[j].SHA })
	r.Pressure = "ok"
	if r.KaneoInReview >= r.Cap {
		r.Pressure = "OVER_CAP"
	} else if r.NeedReview+r.Pending >= r.Cap {
		r.Pressure = "PIN_PRESSURE"
	}
	if d.Provider != nil {
		project := d.KaneoProject
		if project == "" {
			project = ResolveKaneoProject(d.RepoRoot)
		}
		if project == "" {
			r.KaneoError = "project context unavailable"
		} else if tasks, e := d.Provider.ListTasks(ctx, project, "in-review"); e != nil {
			r.KaneoProject = project
			r.KaneoError = e.Error()
		} else {
			r.KaneoOK = true
			r.KaneoProject = project
			r.KaneoInReview = len(tasks)
			for _, task := range tasks {
				if task != nil {
					r.BoardGit = append(r.BoardGit, boardGitRow(ctx, d.RepoRoot, task.Ref, task.Title, r.Pins))
				}
			}
			sort.Slice(r.BoardGit, func(i, j int) bool { return r.BoardGit[i].Ref < r.BoardGit[j].Ref })
			if r.KaneoInReview >= r.Cap {
				r.Pressure = "OVER_CAP"
			}
		}
	} else {
		r.KaneoError = "provider unavailable"
	}
	return r, nil
}

func containsSHA(shas []string, want string) bool {
	for _, sha := range shas {
		if sha == want {
			return true
		}
	}
	return false
}

func boardGitRow(ctx context.Context, repo, ref, title string, pins []PinFreshness) BoardGitRow {
	row := BoardGitRow{Ref: ref, Title: title}
	if runes := []rune(row.Title); len(runes) > 50 {
		row.Title = string(runes[:50])
	}
	if out, err := gitOut(ctx, repo, "log", "origin/main", "--format=%s"); err == nil {
		pattern := regexp.MustCompile(`(?i)(^|[^a-z0-9])` + regexp.QuoteMeta(ref) + `([^a-z0-9]|$)`)
		for _, subject := range strings.Split(out, "\n") {
			if pattern.MatchString(subject) {
				row.Main = true
				break
			}
		}
	}
	for _, pin := range pins {
		if ticketTokenMatch(pin.Branch, ref) {
			row.Tip = pin.SHA
			break
		}
	}
	if out, err := gitOut(ctx, repo, "for-each-ref", "--format=%(refname:short)", "refs/heads"); err == nil {
		for _, branch := range strings.Split(out, "\n") {
			low := strings.ToLower(branch)
			if isParkBranch(low) && ticketTokenMatch(branch, ref) {
				row.Park = true
			}
		}
	}
	return row
}

func isParkBranch(name string) bool {
	for _, segment := range strings.Split(strings.ToLower(strings.TrimSpace(name)), "/") {
		if segment == "park" || segment == "parked" {
			return true
		}
	}
	return false
}

func ticketTokenMatch(value, ref string) bool {
	parts := strings.SplitN(strings.TrimSpace(ref), "-", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	canonical := regexp.QuoteMeta(parts[0] + "-" + parts[1])
	slash := regexp.QuoteMeta(parts[0] + "/" + parts[1])
	pattern := regexp.MustCompile(`(?i)(^|[^a-z0-9])(?:` + canonical + `|` + slash + `)([^a-z0-9]|$)`)
	return pattern.MatchString(value)
}

func (d *Drain) freshness(ctx context.Context, sha, branch, worktree string) PinFreshness {
	lane := strings.TrimSpace(branch)
	if worktree != "" {
		lane = filepath.Base(strings.TrimRight(worktree, string(filepath.Separator)))
	}
	p := PinFreshness{SHA: sha, Lane: lane, Branch: branch, WorktreePath: worktree, Conflict: ConflictUnknown, Behind: -1, Note: "conflict=unknown"}
	if ctx.Err() != nil {
		return p
	}
	if out, e := gitOut(ctx, d.RepoRoot, "rev-list", "--count", sha+"..origin/main"); e == nil {
		p.Behind, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	mb, e := gitOut(ctx, d.RepoRoot, "merge-base", "origin/main", sha)
	if e != nil {
		return p
	}
	if !mergeTreeCapable(ctx, d.RepoRoot) {
		return p
	}
	if _, e := gitOut(ctx, d.RepoRoot, "cat-file", "-e", "origin/main^{commit}"); e != nil {
		return p
	}
	if _, e := gitOut(ctx, d.RepoRoot, "cat-file", "-e", sha+"^{commit}"); e != nil {
		return p
	}
	cmd := procsignal.CommandContext(ctx, "git", "merge-tree", "--write-tree", "--merge-base="+strings.TrimSpace(mb), "origin/main", sha)
	cmd.Dir = d.RepoRoot
	if e := cmd.Run(); e == nil {
		p.Conflict = ConflictClean
		p.Note = "applies clean"
	} else if ctx.Err() == nil {
		p.Conflict = ConflictRebase
		p.Note = "REBASE-NEEDED"
	}
	return p
}
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	c := procsignal.CommandContext(ctx, "git", args...)
	c.Dir = dir
	b, e := c.Output()
	return string(b), e
}

var mergeTreeCache sync.Map

func mergeTreeCapable(ctx context.Context, dir string) bool {
	if v, ok := mergeTreeCache.Load(dir); ok {
		return v.(bool)
	}
	c := procsignal.CommandContext(ctx, "git", "merge-tree", "--write-tree", "--merge-base=HEAD", "HEAD", "HEAD")
	c.Dir = dir
	ok := c.Run() == nil
	mergeTreeCache.Store(dir, ok)
	return ok
}

func envWindDown() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("HERD_WIND_DOWN")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func parkStats(ctx context.Context, repo string) (int, int, error) {
	out, err := gitOut(ctx, repo, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return 0, 0, fmt.Errorf("park branch probe: %w", err)
	}
	count := 0
	seen := map[string]int{}
	ticket := regexp.MustCompile(`(?i)\b(?:FAC|CHA)-[0-9]+\b`)
	for _, raw := range strings.Split(out, "\n") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if isParkBranch(name) {
			count++
			subject, e := gitOut(ctx, repo, "log", "-1", "--format=%s", strings.TrimSpace(raw))
			if e != nil {
				return count, 0, fmt.Errorf("park subject probe %s: %w", raw, e)
			}
			if key := strings.ToUpper(ticket.FindString(subject)); key != "" {
				seen[key]++
			}
		}
	}
	dups := 0
	for _, n := range seen {
		if n > 1 {
			dups++
		}
	}
	return count, dups, nil
}

func recentCount(path string, since time.Time) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("skip log probe: %w", err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if len(line) >= 10 {
			if t, e := time.Parse("2006-01-02", line[:10]); e == nil && !t.Before(since.Truncate(24*time.Hour)) {
				n++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("skip log probe: %w", err)
	}
	return n, nil
}
func rejectedCount(dir string) (int, error) {
	if dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(filepath.Join(dir, "rejected"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("rejected artifact probe: %w", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n, nil
}

// ResolveKaneoProject follows the explicit operator context order used by
// drain; it never treats an unknown project as an empty board.
func ResolveKaneoProject(root string) string {
	if v := strings.TrimSpace(os.Getenv("KANEO_PROJECT")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("HERD_KANEO_PROJECT")); v != "" {
		return v
	}
	if v := provider.ResolveKaneoProjectID(root); v != "" {
		return v
	}
	cmd := exec.Command("kaneo", "context", "--json")
	cmd.Dir = root
	data, err := cmd.Output()
	if err != nil {
		return ""
	}
	var context struct {
		ProjectID string `json:"project_id"`
		Project   string `json:"project"`
	}
	if json.Unmarshal(data, &context) != nil {
		return ""
	}
	if context.ProjectID != "" {
		return context.ProjectID
	}
	return context.Project
}

// defaultProbeBudget bounds a single content-merge probe. One observed tip took
// 1m56s while its peers were sub-second, so an unbounded probe can starve every
// remaining tip in the scan.
const defaultProbeBudget = 20 * time.Second

func (d *Drain) probeBudget() time.Duration {
	if d.ProbeBudget > 0 {
		return d.ProbeBudget
	}
	return defaultProbeBudget
}
