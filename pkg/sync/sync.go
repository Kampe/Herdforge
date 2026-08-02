package sync

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// BoardDrift is the aggregated result of reconciling a board against git
// reality. Drift is the number of findings (a working count parallel to the
// zsh's `drift` accumulator); Findings carries the classified findings.
type BoardDrift struct {
	Drift    int
	Findings []BoardFinding
}

// BoardFinding is one classified drift. Action holds the exact remediation
// text, matching the zsh findings verbatim where one exists.
type BoardFinding struct {
	Kind    string // SHIPPED, UNKNOWN, STALE, BOARD_LAG
	Ref     string
	TaskID  string
	Status  string
	Title   string
	Action  string
}

type BoardSyncer struct {
	Provider provider.TaskProvider
}

func NewBoardSyncer(p provider.TaskProvider) *BoardSyncer {
	return &BoardSyncer{Provider: p}
}

// allowedStatuses ports the jq filter `status in (to-do|in-progress|in-review)`.
var allowedStatuses = map[string]bool{"to-do": true, "in-progress": true, "in-review": true}

// ReconcileBoard reconciles the board against git reality, returning drift
// findings. repoDir is where git operations run (runBoardSync passes ".").
// Report-only: it never writes status to the provider.
// Port of bin/herd-board-sync (lines 271-331).
func (b *BoardSyncer) ReconcileBoard(ctx context.Context, projectID, repoDir string) (*BoardDrift, error) {
	if b.Provider == nil {
		return nil, fmt.Errorf("task provider is nil")
	}

	tasks, err := b.Provider.ListTasks(ctx, projectID, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks for board sync: %w", err)
	}

	facts := bsyncFacts(repoDir)

	drift := &BoardDrift{}
	for _, t := range tasks {
		// jq: select((.status // "") | test("^(to-do|in-progress|in-review)$"))
		if !allowedStatuses[t.Status] {
			continue
		}
		// jq: select((.title // "") | test("standing epic"; "i") | not)
		if strings.Contains(strings.ToLower(t.Title), "standing epic") {
			continue
		}
		if t.Ref == "" {
			continue
		}

		f := b.classify(ctx, t, facts)
		if f != nil {
			drift.Drift++
			drift.Findings = append(drift.Findings, *f)
		}
	}
	return drift, nil
}

// bsyncFacts is the git-truth snapshot the classification reads (bin lines
// 237-269). All git failures degrade to the empty/false value, exactly like
// the zsh's `|| true` / `2>/dev/null`.
type bsyncFacts struct {
	branches     string // lowercased live branch names, newline joined
	localRefs    string // lowercased ahead-of-main subject+body text
	workInFlight bool
	mergedLog    string // lowercased "ct<TAB>subject" entries
}

// bsyncFacts gathers git facts once per reconcile. `branches` comes from
// `git worktree list --porcelain` (the `^branch refs/heads/<name>` lines,
// stripped and lowercased); `localRefs` is the concatenation of per-worktree
// `git -C wt log origin/main..HEAD --pretty=%s%n%b`; `work_in_flight` is
// true when any worktree is ahead of origin/main or dirty.
func bsyncFacts(repoDir string) *bsyncFacts {
	f := &bsyncFacts{}

	out, _ := git(repoDir, "worktree", "list", "--porcelain")
	var wts []string
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "worktree "):
			wts = append(wts, strings.TrimSpace(strings.TrimPrefix(ln, "worktree ")))
		case strings.HasPrefix(ln, "branch refs/heads/"):
			name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(ln, "branch refs/heads/")))
			if name != "" {
				f.branches += name + "\n"
			}
		}
	}

	for _, wt := range wts {
		ahead, _ := git(repoDir, "-C", wt, "log", "origin/main..HEAD", "--pretty=%s%n%b")
		if strings.TrimSpace(ahead) != "" {
			f.localRefs += strings.ToLower(ahead) + "\n"
			f.workInFlight = true
		}
		status, _ := git(repoDir, "-C", wt, "status", "--porcelain")
		if strings.TrimSpace(status) != "" {
			f.workInFlight = true
		}
	}

	// Recent merged history as "committer-epoch<TAB>subject" (lowercased).
	f.mergedLog, _ = git(repoDir, "log", "origin/main", "-500", "--pretty=%ct%x09%s")
	f.mergedLog = strings.ToLower(f.mergedLog)
	return f
}

// classify decides the finding for one ticket, mirroring the zsh case
// statement (lines 304-331). Returns nil when the board is honest.
func (b *BoardSyncer) classify(ctx context.Context, facts *bsyncFacts, t *provider.Task) *BoardFinding {
	ref := NormalizeRef(t.Ref)
	lref := strings.ToLower(ref)
	nref := strings.ReplaceAll(lref, "-", "")

	// active = branch-name match OR ref named in unpushed work
	re := regexp.MustCompile(`(?m)(` + regexp.QuoteMeta(lref) + `|` + regexp.QuoteMeta(nref) + `)([^0-9]|$)`)
	active := re.MatchString(f.branches) || re.MatchString(f.localRefs)

	// created = 0 disables the date gate (zero time.Time ports `created=0`).
	var created int64
	if !t.CreatedAt.IsZero() {
		created = t.CreatedAt.Unix()
	}
	merged := RefShipped(f.mergedLog, lref, created)

	switch t.Status {
	case "in-progress", "in-review":
		switch {
		case active:
			return nil // being worked, board is honest
		case merged:
			return &BoardFinding{
				Kind: "SHIPPED", Ref: ref, TaskID: t.ID, Status: t.Status, Title: t.Title,
				Action: "verify, then: kaneo task status " + t.ID + " done",
			}
		case facts.workInFlight:
			// Cannot prove death while lanes hold unpushed or uncommitted
			// work; degrade to a LABELLED UNKNOWN with no status-flip remedy.
			return &BoardFinding{
				Kind: "UNKNOWN", Ref: ref, TaskID: t.ID, Status: t.Status, Title: t.Title,
				Action: "cannot prove dead (a person is not visible but a/p work may be in-flight), do NOT flip to to-do",
			}
		default:
			return &BoardFinding{
				Kind: "STALE", Ref: ref, TaskID: t.ID, Status: t.Status, Title: t.Title,
				Action: "dead claim",
			}
		}
	case "to-do":
		if active {
			return &BoardFinding{
				Kind: "BOARD_LAG", Ref: ref, TaskID: t.ID, Status: t.Status, Title: t.Title,
				Action: "BOARD-LAG " + ref + " (to-do) has a live worktree branch -> kaneo task status " + t.ID + " in-progress",
			}
		}
	}
	return nil
}