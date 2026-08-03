package review

import (
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

type DrainReport struct {
	WindDown                                                                                                                                                    bool `json:"wind_down"`
	Pending, HarvestQueue, RefactoringCount, Harvestable, NeedReview, ReviewPass, HarvestReady, ContentMerged, KaneoInReview, Cap, RebaseNeeded, StaleBehindMax int
	Pressure                                                                                                                                                    string `json:"pressure"`
	KaneoOK                                                                                                                                                     bool   `json:"kaneo_ok"`
	KaneoProject                                                                                                                                                string `json:"kaneo_project"`
	KaneoError                                                                                                                                                  string `json:"kaneo_error"`
	ParkBranches                                                                                                                                                int    `json:"park_branches"`
	ParkCHAWithDups                                                                                                                                             int    `json:"park_cha_with_dups"`
	Skips7d, LedgerPass, Rejected                                                                                                                               int
	Shas                                                                                                                                                        DrainShas      `json:"-"`
	Pins                                                                                                                                                        []PinFreshness `json:"-"`
	BoardGit                                                                                                                                                    []BoardGitRow  `json:"-"`
	Errors                                                                                                                                                      []string       `json:"-"`
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
	out := make([]LedgerRow, 0)
	for key, index := range recordIndex {
		if verdict, ok := verdictIndex[key]; !ok || verdict < index {
			out = append(out, s.Rows[index])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
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
	seen := map[string]bool{}
	tips := make([]harvest.UnmergedWork, 0)
	for _, u := range unmerged {
		for _, sha := range u.Unmerged {
			if !seen[sha] {
				seen[sha] = true
				tips = append(tips, harvest.UnmergedWork{WorktreePath: u.WorktreePath, Branch: u.Branch, Unmerged: []string{sha}})
			}
		}
	}
	for _, q := range queued {
		if !seen[q.sha] {
			seen[q.sha] = true
			tips = append(tips, harvest.UnmergedWork{Branch: q.branch, Unmerged: []string{q.sha}})
		}
	}
	r := &DrainReport{Pending: len(pending), HarvestQueue: len(queued), Cap: d.Cap, StaleBehindMax: d.StaleBehind, KaneoOK: false, KaneoInReview: -1, Shas: DrainShas{}, WindDown: d.WindDown || envWindDown()}
	var parkErr error
	r.ParkBranches, r.ParkCHAWithDups, parkErr = parkStats(ctx, d.RepoRoot)
	if parkErr != nil {
		r.Errors = append(r.Errors, parkErr.Error())
	}
	for _, row := range snap.Rows {
		if row.Event == string(EventVerdict) && row.Verdict == string(VerdictPASS) {
			r.LedgerPass++
		}
	}
	r.Skips7d = recentCount(filepath.Join(d.StateDir, "review-gate-skips.log"), time.Now().Add(-7*24*time.Hour))
	r.Rejected = rejectedCount(d.ArtifactDir)
	for sha := range pass {
		r.Shas.ReviewPass = append(r.Shas.ReviewPass, sha)
	}
	sort.Strings(r.Shas.ReviewPass)
	r.ReviewPass = len(r.Shas.ReviewPass)
	for _, u := range tips {
		sha := u.Unmerged[0]
		pin := d.freshness(ctx, sha, u.Branch, u.WorktreePath)
		r.Pins = append(r.Pins, pin)
		merged, probeErr := harvest.ContentMerged(ctx, d.RepoRoot, "origin/main", sha)
		if probeErr != nil {
			return nil, fmt.Errorf("content-merge probe for %s: %w", sha, probeErr)
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
	}
	for _, p := range [][]string{r.Shas.Harvestable, r.Shas.NeedReview, r.Shas.ContentMerged, r.Shas.HarvestReady, r.Shas.RebaseNeeded} {
		sort.Strings(p)
	}
	r.Harvestable = len(r.Shas.Harvestable)
	r.NeedReview = len(r.Shas.NeedReview)
	r.ContentMerged = len(r.Shas.ContentMerged)
	r.HarvestReady = len(r.Shas.HarvestReady)
	r.RebaseNeeded = len(r.Shas.RebaseNeeded)
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
					r.BoardGit = append(r.BoardGit, boardGitRow(ctx, d.RepoRoot, task.Ref, task.Title))
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

func boardGitRow(ctx context.Context, repo, ref, title string) BoardGitRow {
	row := BoardGitRow{Ref: ref, Title: title}
	if len(row.Title) > 50 {
		row.Title = row.Title[:50]
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
	if out, err := gitOut(ctx, repo, "for-each-ref", "--format=%(refname:short)", "refs/heads", "refs/remotes"); err == nil {
		for _, branch := range strings.Split(out, "\n") {
			low := strings.ToLower(branch)
			needle := strings.ToLower(strings.ReplaceAll(ref, "-", "/"))
			matches := strings.Contains(low, strings.ToLower(ref)) || strings.Contains(low, needle)
			if strings.Contains(low, "park") && matches {
				row.Park = true
			}
			if matches && row.Tip == "" {
				if sha, e := gitOut(ctx, repo, "rev-parse", "--short=12", strings.TrimSpace(branch)); e == nil {
					row.Tip = strings.TrimSpace(sha)
				}
			}
		}
	}
	return row
}

func (d *Drain) freshness(ctx context.Context, sha, branch, worktree string) PinFreshness {
	lane := strings.TrimSpace(branch)
	if lane == "" {
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
	cmd := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", "--merge-base="+strings.TrimSpace(mb), "origin/main", sha)
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
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = dir
	b, e := c.Output()
	return string(b), e
}

var mergeTreeCache sync.Map

func mergeTreeCapable(ctx context.Context, dir string) bool {
	if v, ok := mergeTreeCache.Load(dir); ok {
		return v.(bool)
	}
	c := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", "--merge-base=HEAD", "HEAD", "HEAD")
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
	for _, raw := range strings.Split(out, "\n") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if strings.Contains(name, "/park/") || strings.HasPrefix(name, "park/") || strings.HasPrefix(name, "parked/") || strings.Contains(name, "/parked/") {
			count++
			parts := strings.FieldsFunc(name, func(r rune) bool { return r == '/' })
			if len(parts) > 1 {
				key := parts[len(parts)-1]
				seen[key]++
			}
		}
	}
	dups := 0
	for _, n := range seen {
		if n > 1 {
			dups += n - 1
		}
	}
	return count, dups, nil
}

func recentCount(path string, since time.Time) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
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
	return n
}
func rejectedCount(dir string) int {
	if dir == "" {
		return 0
	}
	entries, err := os.ReadDir(filepath.Join(dir, "rejected"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n
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
