// Package retention joins Git worktree truth with board, lease, session, PR,
// review-receipt, and graph truth to produce a read-only retention inventory
// and a deterministic dry-run cleanup plan (FAC-179).
//
// This package never removes anything and holds no destructive entry point.
// The action path is worktree.WorktreeManager.Reap, which carries its own
// FAC-178 evidence binding, lease fencing, salvage verification, and
// just-in-time revalidation. A retention plan is permission to *ask* Reap
// about an exact target; it is never permission to remove one.
//
// Everything this package cannot read for itself arrives through Policy.Truth.
// A nil probe, a probe error, or an incomplete Truth is a refusal, never an
// assumption that cleanup is safe.
//
// # Truth producers
//
// Policy.Truth is deliberately a seam rather than a wired-up implementation.
// Most of its fields already have a producer on origin/main:
//
//	TaskRef, TaskStatus   provider.TaskProvider.GetTask
//	LeaseGeneration       lifecycle.HoldAuthority.CurrentGeneration
//	ReviewReceipt         reviewledger.Ledger
//	GraphRevision         the herd-deps-v1 graph revision on the task packet
//
// Two do not, and this package will not fake them:
//
//   - SessionKnown/SessionActive need an authoritative live-versus-orphaned
//     verdict for an exact worktree path. herdr.AgentList exposes AgentEntry.Cwd,
//     but no exported symbol today distinguishes a live pane from a stale roster
//     entry. FAC-158 owns that verdict.
//   - PurposeMutation is inventoried and classified here, but the cleanup
//     semantics for a mutation worktree are owned by FAC-157.
//
// Until those land there is no production TruthProbe and no herd status/health
// integration; a caller must supply its own probe and accept that an honest
// probe refuses whatever it cannot read.
package retention

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

// Class is the retention classification of one worktree. Every inventoried
// worktree receives exactly one Class.
type Class string

const (
	// classPending is the internal sentinel for an entry that survived every
	// protective branch and is resolved only by the cross-entry pass, which
	// always assigns a real class. It is the only place eligibility is ever
	// set to true, so no other class can become reclaimable by accident.
	classPending Class = ""

	// ClassRoot is the shared repository checkout.
	ClassRoot Class = "root"
	// ClassProtected is an integration, detached, or non-herd worktree that is
	// outside retention scope entirely.
	ClassProtected Class = "protected"
	// ClassUnknown means some required truth was unreadable. Unknown is always
	// a refusal; it is never silently eligible.
	ClassUnknown Class = "unknown"
	// ClassQuarantined is held for incident or review provenance.
	ClassQuarantined Class = "quarantined"
	// ClassActive has a live session or a non-terminal board status.
	ClassActive Class = "active"
	// ClassReviewHeld is in review, or is rejected-review evidence a reviewer
	// still requires.
	ClassReviewHeld Class = "review-held"
	// ClassDirty has uncommitted changes.
	ClassDirty Class = "dirty"
	// ClassUnique has committed patches not present on the integration base.
	ClassUnique Class = "unique"
	// ClassAbandoned is a terminated lane, clean and content-merged.
	ClassAbandoned Class = "abandoned"
	// ClassSuperseded is a generational audit/review worktree for which a
	// strictly newer generation of the same lane exists.
	ClassSuperseded Class = "superseded-generation"
	// ClassRecoverable is clean, content-merged, terminal, and fully recoverable
	// from its salvage ref and receipt.
	ClassRecoverable Class = "merged-recoverable"
)

// Purpose is why a worktree exists. Mutation worktrees are inventoried here but
// their cleanup semantics are owned by FAC-157; this package only classifies.
type Purpose string

const (
	PurposeTask     Purpose = "task"
	PurposeAudit    Purpose = "audit"
	PurposeReview   Purpose = "review"
	PurposeMutation Purpose = "mutation"
	PurposeStanding Purpose = "standing"
)

// Status is the durable board status of the worktree's task.
type Status string

const (
	StatusActive    Status = "active"
	StatusBlocked   Status = "blocked"
	StatusInReview  Status = "in-review"
	StatusDone      Status = "done"
	StatusAbandoned Status = "abandoned"
)

// Truth is the non-Git truth for one exact worktree, joined from the board,
// the durable lease authority, the Herdr session/pane roster, the PR provider,
// the review/admission ledger, and the dependency graph.
//
// Empty required fields are missing truth, not defaults. SessionKnown exists
// precisely so an unreadable Herdr roster cannot be reported as "no session":
// the probe must say when it does not know.
type Truth struct {
	TaskRef         string
	TaskStatus      Status
	Purpose         Purpose
	Generation      int // generation number for audit/review lanes; task lanes use 0
	LeaseGeneration string
	SessionKnown    bool
	SessionActive   bool
	PRState         string
	ReviewReceipt   string
	GraphRevision   string
	Quarantined     bool
	// RequiredEvidence marks a generation a reviewer still needs, most often a
	// rejected review. It is protected regardless of age or generation count.
	RequiredEvidence bool
}

// TruthProbe resolves Truth for one exact worktree. An error refuses that
// worktree; it never refuses the whole inventory.
type TruthProbe func(ctx context.Context, path, branch string) (Truth, error)

// Attention is one durable, portable operator-visible refusal. It carries no
// filesystem path so it can be moved between worktrees, matching the
// portability rule the reap receipts already enforce.
type Attention struct {
	Branch  string   `json:"branch"`
	TaskRef string   `json:"task_ref"`
	Class   string   `json:"class"`
	Reason  string   `json:"reason"`
	Missing []string `json:"missing,omitempty"`
}

// Policy configures a retention plan. The zero value is invalid: a plan must
// name its evidence floor and its truth source explicitly.
type Policy struct {
	DefaultBranch string
	// MinEvidenceGenerations is how many newest generations are retained per
	// generational (task ref, purpose) lane no matter what other evidence says.
	// Must be at least 1: retention is never permitted to keep nothing.
	MinEvidenceGenerations int
	// PressureBytes, when positive, marks the report pressured once total
	// inventoried usage exceeds it. Pressure reorders reclaim work only; it
	// never changes which entries are eligible.
	PressureBytes int64
	// Truth is required. Retention refuses to infer board, lease, session, PR,
	// receipt, or graph state.
	Truth TruthProbe
	// AttentionSink durably records refusals. A sink error fails the plan closed.
	AttentionSink func(Attention) error
	// Now supplies the clock for age reporting. Nil uses time.Now.
	Now func() time.Time
}

// Entry is one inventoried worktree with its retention verdict.
type Entry struct {
	Path           string             `json:"-"`
	Branch         string             `json:"branch"`
	HEAD           string             `json:"head"`
	TaskRef        string             `json:"task_ref"`
	Class          Class              `json:"class"`
	Purpose        Purpose            `json:"purpose"`
	Generation     int                `json:"generation"`
	GitClass       worktree.ReapClass `json:"git_class"`
	SalvageRef     string             `json:"salvage_ref"`
	Reason         string             `json:"reason"`
	PreserveAction string             `json:"preserve_action"`
	Eligible       bool               `json:"eligible"`
	Risk           int                `json:"risk"`
	Bytes          int64              `json:"bytes"`
	Inodes         int64              `json:"inodes"`
	SizeKnown      bool               `json:"size_known"`
	Age            time.Duration      `json:"age"`
	MissingTruth   []string           `json:"missing_truth,omitempty"`

	// attention is the portable refusal summary. Reason may quote Git or probe
	// output, which can name absolute paths; this never does, so durable
	// attention evidence stays movable between worktrees.
	attention string
	// status is the observed board status, kept for the cross-entry pass.
	status Status
	// gitEligible is the reap classifier's own verdict that this worktree is
	// clean, content-merged, and has a resolvable salvage tip.
	gitEligible bool
}

// Report is the deterministic read-only plan.
type Report struct {
	Entries           []Entry       `json:"entries"`
	Eligible          []Entry       `json:"eligible"`
	Refused           []Entry       `json:"refused"`
	Counts            map[Class]int `json:"counts"`
	TotalBytes        int64         `json:"total_bytes"`
	TotalInodes       int64         `json:"total_inodes"`
	ReclaimableBytes  int64         `json:"reclaimable_bytes"`
	ReclaimableInodes int64         `json:"reclaimable_inodes"`
	OldestEligibleAge time.Duration `json:"oldest_eligible_age"`
	Pressured         bool          `json:"pressured"`
	Attention         []Attention   `json:"attention,omitempty"`
}

// ActionTargets returns the exact worktree paths a destructive action may
// consider, in plan order. It is the only bridge from a retention plan to
// worktree.WorktreeManager.Reap, which independently re-derives every fence
// before it removes anything.
func (r *Report) ActionTargets() []string {
	if r == nil {
		return nil
	}
	targets := make([]string, 0, len(r.Eligible))
	for _, e := range r.Eligible {
		targets = append(targets, e.Path)
	}
	return targets
}

// Plan inventories every registered worktree and classifies it exactly once.
// It performs no mutation of any kind.
func Plan(ctx context.Context, wm *worktree.WorktreeManager, policy Policy) (*Report, error) {
	if wm == nil {
		return nil, fmt.Errorf("retention: worktree manager is required")
	}
	if policy.Truth == nil {
		return nil, fmt.Errorf("retention: truth probe is required; board, lease, session, PR, receipt, and graph state are never inferred")
	}
	if policy.MinEvidenceGenerations < 1 {
		return nil, fmt.Errorf("retention: MinEvidenceGenerations must be at least 1")
	}
	now := time.Now
	if policy.Now != nil {
		now = policy.Now
	}

	// Git truth reuses the fail-closed reap classifier rather than re-deriving
	// dirty/unique/root state. Report-only policy: no probes, no action.
	plan, err := wm.PlanReap(ctx, worktree.ReapPolicy{DefaultBranch: policy.DefaultBranch})
	if err != nil {
		return nil, fmt.Errorf("retention: git inventory: %w", err)
	}

	registered := make([]string, 0, len(plan.Candidates))
	for _, c := range plan.Candidates {
		registered = append(registered, c.Path)
	}

	report := &Report{Counts: map[Class]int{}}
	stamp := now()
	for _, c := range plan.Candidates {
		report.Entries = append(report.Entries, buildEntry(ctx, c, policy, registered, stamp))
	}

	resolveGenerational(report.Entries, policy.MinEvidenceGenerations)

	for i := range report.Entries {
		e := &report.Entries[i]
		e.Risk = riskRank(e.Class)
		report.Counts[e.Class]++
		if e.SizeKnown {
			report.TotalBytes += e.Bytes
			report.TotalInodes += e.Inodes
		}
	}

	sort.Slice(report.Entries, func(i, j int) bool {
		return lessEntry(report.Entries[i], report.Entries[j])
	})

	for _, e := range report.Entries {
		if e.Eligible {
			report.Eligible = append(report.Eligible, e)
			if e.SizeKnown {
				report.ReclaimableBytes += e.Bytes
				report.ReclaimableInodes += e.Inodes
			}
			if e.Age > report.OldestEligibleAge {
				report.OldestEligibleAge = e.Age
			}
			continue
		}
		report.Refused = append(report.Refused, e)
		if e.Class == ClassUnknown {
			report.Attention = append(report.Attention, Attention{
				Branch: e.Branch, TaskRef: e.TaskRef, Class: string(e.Class),
				Reason: e.attention, Missing: e.MissingTruth,
			})
		}
	}

	report.Pressured = policy.PressureBytes > 0 && report.TotalBytes > policy.PressureBytes
	if report.Pressured {
		// Pressure reprioritises reclaim work by size. The eligible *set* is
		// already fixed above, so no preservation rule can be bypassed here.
		sort.SliceStable(report.Eligible, func(i, j int) bool {
			a, b := report.Eligible[i], report.Eligible[j]
			if a.Bytes != b.Bytes {
				return a.Bytes > b.Bytes
			}
			if a.TaskRef != b.TaskRef {
				return a.TaskRef < b.TaskRef
			}
			return a.Path < b.Path
		})
		report.Attention = append(report.Attention, Attention{
			Class: "pressure",
			Reason: fmt.Sprintf("inventoried usage %d bytes exceeds pressure budget %d bytes",
				report.TotalBytes, policy.PressureBytes),
		})
	}

	if policy.AttentionSink != nil {
		for _, a := range report.Attention {
			if err := policy.AttentionSink(a); err != nil {
				return nil, fmt.Errorf("retention: attention sink: %w", err)
			}
		}
	}
	return report, nil
}

func buildEntry(ctx context.Context, c worktree.ReapCandidate, policy Policy, registered []string, stamp time.Time) Entry {
	e := Entry{
		Path:           c.Path,
		Branch:         c.Branch,
		HEAD:           c.HEAD,
		GitClass:       c.Class,
		SalvageRef:     c.SalvageRef,
		PreserveAction: c.PreserveAction,
		Age:            ageOf(c.Path, stamp),
		gitEligible:    c.Eligible,
	}
	e.Bytes, e.Inodes, e.SizeKnown = usage(c.Path, registered)

	// The shared checkout and non-herd worktrees are out of retention scope, so
	// no task truth is requested for them at all.
	switch c.Class {
	case worktree.ReapClassRoot:
		e.Class = ClassRoot
		e.Reason = "shared repository root"
		e.PreserveAction = "never reclaim the primary checkout"
		return e
	case worktree.ReapClassProtected:
		e.Class = ClassProtected
		e.Reason = c.Reason
		return e
	}

	truth, terr := policy.Truth(ctx, c.Path, c.Branch)
	if terr != nil {
		e.Class = ClassUnknown
		e.Reason = "truth probe error: " + terr.Error()
		e.attention = "truth probe refused this worktree"
		e.PreserveAction = "keep worktree until board, lease, session, PR, receipt, and graph truth are readable"
		return e
	}
	e.TaskRef = truth.TaskRef
	e.Purpose = truth.Purpose
	e.Generation = truth.Generation
	e.status = truth.TaskStatus

	if missing := missingTruth(truth); len(missing) > 0 {
		e.Class = ClassUnknown
		e.MissingTruth = missing
		e.Reason = "incomplete truth: " + strings.Join(missing, ", ")
		e.attention = e.Reason // composed here from fixed labels, so already portable
		e.PreserveAction = "keep worktree until every named truth source is readable"
		return e
	}

	switch {
	case truth.Quarantined:
		e.Class = ClassQuarantined
		e.Reason = "quarantined for incident or review provenance"
		e.PreserveAction = "keep worktree until quarantine is explicitly lifted"
	case truth.SessionActive:
		e.Class = ClassActive
		e.Reason = "live Herdr session or pane is bound to this worktree"
		e.PreserveAction = "wait for the session to end before considering reclaim"
	case truth.TaskStatus == StatusActive || truth.TaskStatus == StatusBlocked:
		e.Class = ClassActive
		e.Reason = "board status " + string(truth.TaskStatus) + " is not terminal"
		e.PreserveAction = "wait for a terminal board status"
	case truth.RequiredEvidence:
		e.Class = ClassReviewHeld
		e.Reason = "reviewer still requires this generation as evidence"
		e.PreserveAction = "keep worktree until the reviewer releases the evidence hold"
	case truth.TaskStatus == StatusInReview:
		e.Class = ClassReviewHeld
		e.Reason = "board status in-review"
		e.PreserveAction = "keep worktree until review concludes"
	case c.Class == worktree.ReapClassDirty:
		e.Class = ClassDirty
		e.Reason = c.Reason
	case c.Class == worktree.ReapClassUnique:
		e.Class = ClassUnique
		e.Reason = c.Reason
	case c.Class != worktree.ReapClassContentMerged:
		// Includes ReapClassUnknown and any class this switch has not been
		// taught: unclassifiable Git state is a refusal.
		e.Class = ClassUnknown
		e.Reason = "git classification " + string(c.Class) + ": " + c.Reason
		// c.Reason quotes Git output, which names paths; the attention summary
		// carries only the class.
		e.attention = "git classification " + string(c.Class)
		e.PreserveAction = "keep worktree until Git state can be classified"
	default:
		// Terminal, clean, content-merged. Whether this is a superseded
		// generation cannot be decided from one entry, so defer it.
		e.Class = classPending
	}
	return e
}

// resolveGenerational settles the classes that depend on sibling entries and
// then applies the evidence floor.
//
// A generation is superseded only when a strictly newer generation of the same
// (task ref, purpose) lane is registered, whatever class that newer generation
// holds. The floor then retains the newest min generations of every
// generational lane, so the newest required evidence generation can never be
// reclaimed and a sibling lane is never consulted.
func resolveGenerational(entries []Entry, min int) {
	type key struct {
		ref     string
		purpose Purpose
	}

	newest := map[key]int{}
	for _, e := range entries {
		if e.Generation < 1 || e.Class == ClassRoot || e.Class == ClassProtected {
			continue
		}
		k := key{e.TaskRef, e.Purpose}
		if e.Generation > newest[k] {
			newest[k] = e.Generation
		}
	}

	for i := range entries {
		e := &entries[i]
		if e.Class != classPending {
			continue
		}
		k := key{e.TaskRef, e.Purpose}
		switch {
		case e.Generation > 0 && e.Generation < newest[k]:
			e.Class = ClassSuperseded
			e.Reason = fmt.Sprintf("generation %d of %s lane %s is superseded by generation %d; clean and content-merged",
				e.Generation, e.Purpose, e.TaskRef, newest[k])
		case e.status == StatusAbandoned:
			e.Class = ClassAbandoned
			e.Reason = "board status abandoned; clean and content-merged"
		default:
			e.Class = ClassRecoverable
			e.Reason = "board status done; clean, content-merged, and recoverable from salvage ref and receipt"
		}
		e.Eligible = e.gitEligible
		e.PreserveAction = "reclaim only through worktree.Reap with full action evidence; recover from " + e.SalvageRef
	}

	// Evidence floor, generational lanes only. A non-generational task lane
	// carries its own salvage ref and receipt and has no generation to retain.
	groups := map[key][]int{}
	for i := range entries {
		e := entries[i]
		if e.Generation < 1 || e.Class == ClassRoot || e.Class == ClassProtected {
			continue
		}
		k := key{e.TaskRef, e.Purpose}
		groups[k] = append(groups[k], i)
	}
	for _, idx := range groups {
		// Newest first: higher generation, then younger, then path for stability.
		sort.Slice(idx, func(a, b int) bool {
			x, y := entries[idx[a]], entries[idx[b]]
			if x.Generation != y.Generation {
				return x.Generation > y.Generation
			}
			if x.Age != y.Age {
				return x.Age < y.Age
			}
			return x.Path < y.Path
		})
		for rank, i := range idx {
			if rank >= min || !entries[i].Eligible {
				continue
			}
			entries[i].Eligible = false
			entries[i].Reason += fmt.Sprintf("; retained as one of the newest %d evidence generations", min)
			entries[i].PreserveAction = fmt.Sprintf("retained evidence generation %d of the newest %d", rank+1, min)
		}
	}
}

// missingTruth names every required truth source that did not answer. An empty
// value is missing truth, never a default.
func missingTruth(t Truth) []string {
	var missing []string
	if strings.TrimSpace(t.TaskRef) == "" {
		missing = append(missing, "board task ref")
	}
	if !validStatus(t.TaskStatus) {
		missing = append(missing, "board status")
	}
	if !validPurpose(t.Purpose) {
		missing = append(missing, "worktree purpose")
	}
	if strings.TrimSpace(t.LeaseGeneration) == "" {
		missing = append(missing, "lease generation")
	}
	if !t.SessionKnown {
		missing = append(missing, "herdr session state")
	}
	if strings.TrimSpace(t.PRState) == "" {
		missing = append(missing, "pull request state")
	}
	if strings.TrimSpace(t.ReviewReceipt) == "" {
		missing = append(missing, "review receipt")
	}
	if strings.TrimSpace(t.GraphRevision) == "" {
		missing = append(missing, "graph revision")
	}
	if (t.Purpose == PurposeAudit || t.Purpose == PurposeReview) && t.Generation < 1 {
		missing = append(missing, "generation number")
	}
	return missing
}

func validStatus(s Status) bool {
	switch s {
	case StatusActive, StatusBlocked, StatusInReview, StatusDone, StatusAbandoned:
		return true
	}
	return false
}

func validPurpose(p Purpose) bool {
	switch p {
	case PurposeTask, PurposeAudit, PurposeReview, PurposeMutation, PurposeStanding:
		return true
	}
	return false
}

// riskRank orders the plan by the risk of acting on an entry. The most
// protected entries surface first so an operator reads refusals before
// reclaim work.
func riskRank(c Class) int {
	switch c {
	case ClassRoot:
		return 100
	case ClassUnknown:
		return 90
	case ClassQuarantined:
		return 80
	case ClassDirty:
		return 70
	case ClassUnique:
		return 60
	case ClassActive:
		return 50
	case ClassReviewHeld:
		return 45
	case ClassProtected:
		return 40
	case ClassAbandoned:
		return 20
	case ClassRecoverable:
		return 10
	case ClassSuperseded:
		return 5
	}
	// An unrecognised class ranks just below root: surface it, never reclaim it.
	return 95
}

// lessEntry is the deterministic plan order: risk descending, then class,
// then ticket ref ascending, then exact path.
func lessEntry(a, b Entry) bool {
	if a.Risk != b.Risk {
		return a.Risk > b.Risk
	}
	if a.Class != b.Class {
		return a.Class < b.Class
	}
	if a.TaskRef != b.TaskRef {
		return a.TaskRef < b.TaskRef
	}
	return a.Path < b.Path
}

func ageOf(path string, stamp time.Time) time.Duration {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	age := stamp.Sub(info.ModTime())
	if age < 0 {
		return 0
	}
	return age
}

// usage sums on-disk bytes and entry count for one worktree. Nested registered
// worktrees are skipped so the shared root does not absorb its children's
// usage. A linked worktree's Git metadata lives under the root's .git, so its
// reclaimable bytes are the checkout only.
func usage(root string, registered []string) (bytes, inodes int64, known bool) {
	rootKey := absClean(root)
	skip := make(map[string]bool, len(registered))
	for _, p := range registered {
		if key := absClean(p); key != rootKey {
			skip[key] = true
		}
	}
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && skip[absClean(p)] {
			return fs.SkipDir
		}
		inodes++
		if d.Type().IsRegular() {
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			bytes += info.Size()
		}
		return nil
	})
	if walkErr != nil {
		return 0, 0, false
	}
	return bytes, inodes, true
}

func absClean(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}
