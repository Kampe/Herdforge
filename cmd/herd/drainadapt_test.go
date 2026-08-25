package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/router"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// TestDrainSelftestCasesHoldForEveryGate runs the shared selftest table in
// process. Each refusal case must still refuse and the authorized case must
// still succeed: this is the same table `herd drain --selftest` runs, so a
// weakened gate turns both red.
func TestDrainSelftestCasesHoldForEveryGate(t *testing.T) {
	for _, tc := range drainSelftestCases() {
		t.Run(tc.name, func(t *testing.T) {
			a, tasks, launcher, err := newDrainSelftestAdapters(t.TempDir())
			if err != nil {
				t.Fatalf("wire adapters: %v", err)
			}
			e := drainSelftestEvidence()
			if tc.mutate != nil {
				tc.mutate(a, tasks, launcher, &e)
			}
			if verdict := drainSelftestVerdict(tc, tc.action(a, e)); verdict != "" {
				t.Fatalf("%s: %s", tc.name, verdict)
			}
		})
	}
}

func TestDrainSelftestReportsPass(t *testing.T) {
	var out strings.Builder
	if code := drainSelftest(&out); code != 0 {
		t.Fatalf("hermetic selftest failed: %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "bounded review + dry-run harvest beat") {
		t.Fatalf("selftest never exercised the bounded beat: %s", out.String())
	}
}

// TestDrainReviewRejectsBranchAsTaskRef pins the FAC-65 defect: the legacy
// shape filtered in-progress tasks by the branch string, matched nothing, and
// returned a false success with no launch. The mutant is reproduced here, and
// the compiled adapter must reject the same input instead of succeeding.
func TestDrainReviewRejectsBranchAsTaskRef(t *testing.T) {
	branch := "herd/fac-999"
	a, _, launcher, err := newDrainSelftestAdapters(t.TempDir())
	if err != nil {
		t.Fatalf("wire adapters: %v", err)
	}

	// Mutant fixture: treating the branch as a task ref silently finds nothing.
	tasks, listErr := a.tasks.ListTasks(context.Background(), a.project, "in-progress")
	if listErr != nil {
		t.Fatalf("fixture list: %v", listErr)
	}
	matched := 0
	for _, task := range tasks {
		if strings.EqualFold(hsync.NormalizeRef(task.Ref), hsync.NormalizeRef(branch)) {
			matched++
		}
	}
	if matched != 0 || listErr != nil {
		t.Fatalf("fixture no longer reproduces branch-as-task-ref: matched=%d err=%v", matched, listErr)
	}

	e := drainSelftestEvidence()
	e.Branch = branch
	err = a.launchReview(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "no live board task") {
		t.Fatalf("compiled adapter accepted a branch as a task ref: %v", err)
	}
	if len(launcher.packets) != 0 {
		t.Fatalf("refused review still reached the process API: %v", launcher.packets)
	}
}

// TestDrainHarvestRejectsUngatedMerge is the direct-merge mutant: an
// integration that reports a merge the review gate never admitted must not be
// counted as a harvest.
func TestDrainHarvestRejectsUngatedMerge(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  *harvest.IntegrationResult
		want string
	}{
		{
			name: "merged without review gate",
			res:  &harvest.IntegrationResult{MergedSHAs: []harvest.MergeOutcome{{SHA: drainSelftestSHA, Pushed: true}}},
			want: "never gated exact candidate",
		},
		{
			name: "merged while gate refused",
			res: &harvest.IntegrationResult{
				ReviewGatedSHAs: []harvest.ReviewGateOutcome{{SHA: drainSelftestSHA, Eligible: false, Reason: "no verdict for exact candidate sha"}},
				MergedSHAs:      []harvest.MergeOutcome{{SHA: drainSelftestSHA, Pushed: true}},
			},
			want: "review admission refused",
		},
		{
			name: "gated a different candidate",
			res: &harvest.IntegrationResult{
				ReviewGatedSHAs: []harvest.ReviewGateOutcome{{SHA: strings.Repeat("a", 40), Eligible: true}},
				MergedSHAs:      []harvest.MergeOutcome{{SHA: drainSelftestSHA, Pushed: true}},
			},
			want: "never gated exact candidate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := drainAdaptersWithRecord(t)
			a.run = func(context.Context, string, harvest.AdmissionContext, bool) (*harvest.IntegrationResult, error) {
				return tc.res, nil
			}
			err := a.integrate(context.Background(), drainSelftestEvidence(), false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ungated merge accepted: %v", err)
			}
		})
	}
}

// TestDrainHarvestAdmissionUsesLaunchProvenance proves the admission context
// is built from the durable launch record plus an independently computed patch
// id — not from the verdict row Admit is about to check it against.
func TestDrainHarvestAdmissionUsesLaunchProvenance(t *testing.T) {
	a := drainAdaptersWithRecord(t)
	var got harvest.AdmissionContext
	a.run = func(_ context.Context, sha string, adm harvest.AdmissionContext, dry bool) (*harvest.IntegrationResult, error) {
		got = adm
		return &harvest.IntegrationResult{ReviewGatedSHAs: []harvest.ReviewGateOutcome{{SHA: sha, Eligible: true}}}, nil
	}
	if err := a.integrate(context.Background(), drainSelftestEvidence(), true); err != nil {
		t.Fatalf("dry-run harvest: %v", err)
	}
	if got.Task != "FAC-1" || got.Lease != "7" || got.AuthorFamily != "anthropic" {
		t.Fatalf("admission lost launch provenance: %+v", got)
	}
	if got.PatchURL != "patch-"+drainSelftestSHA {
		t.Fatalf("admission did not use an independent patch identity: %+v", got)
	}

	// An empty patch identity is missing authority, never an empty assertion.
	a.patchID = func(context.Context, string) (string, error) { return "  ", nil }
	if err := a.integrate(context.Background(), drainSelftestEvidence(), true); err == nil || !strings.Contains(err.Error(), "empty patch identity") {
		t.Fatalf("empty patch identity was admitted: %v", err)
	}
}

// TestDrainReviewRecordsExactLaunchProvenance proves the launch record the
// harvest gate later depends on is written with the exact candidate, the
// recorded builder family, and the routed reviewer identity.
func TestDrainReviewRecordsExactLaunchProvenance(t *testing.T) {
	dir := t.TempDir()
	a, _, launcher, err := newDrainSelftestAdapters(dir)
	if err != nil {
		t.Fatalf("wire adapters: %v", err)
	}
	var routedLane *config.LaneDef
	originalRoute := a.route
	a.route = func(lane *config.LaneDef, task *provider.Task) (*router.LaunchDecision, error) {
		routedLane = lane
		return originalRoute(lane, task)
	}
	if err := a.launchReview(context.Background(), drainSelftestEvidence()); err != nil {
		t.Fatalf("review launch: %v", err)
	}
	if routedLane == nil || routedLane.Name != "review" || routedLane.Role != launch.ReviewerRole {
		t.Fatalf("review launch routed through non-review lane: %+v", routedLane)
	}
	rows, err := a.ledger.AllRows()
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var record *reviewledger.LedgerRow
	for i := range rows {
		if rows[i].Event == string(reviewledger.EventRecord) {
			record = &rows[i]
		}
	}
	if record == nil {
		t.Fatalf("no launch record written: %+v", rows)
	}
	if record.SHA != drainSelftestSHA || record.BuilderFamily != "anthropic" || record.ReviewerFamily != "google" || record.Task != "FAC-1" || record.Lease != "7" || record.Reviewer != "forge-review" {
		t.Fatalf("launch record lost provenance: %+v", record)
	}
	if len(launcher.packets) != 1 || !strings.Contains(launcher.packets[0], drainSelftestSHA) || !strings.Contains(launcher.packets[0], "FAC-1") {
		t.Fatalf("review packet did not carry the exact candidate: %v", launcher.packets)
	}
	if !strings.Contains(launcher.packets[0], "REPORT_TARGET: forge-review-supervisor") || strings.Contains(launcher.packets[0], "REPORT_TARGET: coordinator") {
		t.Fatalf("review packet supervisor target drifted: %s", launcher.packets[0])
	}
	for _, banned := range []string{"chainseer", "bin/herd-", "zsh"} {
		if strings.Contains(launcher.packets[0], banned) {
			t.Fatalf("review packet references %q: %s", banned, launcher.packets[0])
		}
	}
}

// TestDrainReviewFailsClosedBeforeSideEffects proves each refusal happens
// before the process API is touched and before anything is recorded.
func TestDrainReviewFailsClosedBeforeSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*drainAdapters, *drainActionEvidence)
	}{
		{name: "route rejects", mutate: func(a *drainAdapters, _ *drainActionEvidence) {
			a.route = func(*config.LaneDef, *provider.Task) (*router.LaunchDecision, error) {
				return nil, errors.New("routed decision rejected")
			}
		}},
		{name: "decision drops exact candidate", mutate: func(a *drainAdapters, _ *drainActionEvidence) {
			a.route = func(*config.LaneDef, *provider.Task) (*router.LaunchDecision, error) {
				return &router.LaunchDecision{Role: router.RoleReviewer, Family: "google"}, nil
			}
		}},
		{name: "author model unprovable", mutate: func(a *drainAdapters, _ *drainActionEvidence) {
			a.authorModel = func(string) (string, error) { return "", errors.New("no accepted builder launch receipt") }
		}},
		{name: "board unreachable", mutate: func(a *drainAdapters, _ *drainActionEvidence) {
			a.tasks = &fakeDrainProvider{err: errors.New("board offline")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, launcher, err := newDrainSelftestAdapters(t.TempDir())
			if err != nil {
				t.Fatalf("wire adapters: %v", err)
			}
			e := drainSelftestEvidence()
			tc.mutate(a, &e)
			if err := a.launchReview(context.Background(), e); err == nil {
				t.Fatal("review launched without authority")
			}
			if len(launcher.packets) != 0 {
				t.Fatalf("refused review reached the process API: %v", launcher.packets)
			}
			rows, err := a.ledger.AllRows()
			if err != nil {
				t.Fatalf("read ledger: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("refused review wrote ledger rows: %+v", rows)
			}
		})
	}
}

func TestDrainCandidateRef(t *testing.T) {
	for branch, want := range map[string]string{
		"task/FAC-1-description": "FAC-1",
		"herd/fac-184":           "FAC-184",
		"FAC-018":                "FAC-18",
		"main":                   "",
		"wt/reviewer":            "",
	} {
		if got := drainCandidateRef(branch); got != want {
			t.Fatalf("drainCandidateRef(%q)=%q want %q", branch, got, want)
		}
	}
}

func TestDrainExactSHARejectsShortPins(t *testing.T) {
	for _, bad := range []string{"", "  ", "0123456", strings.Repeat("0", 39), strings.Repeat("z", 40)} {
		if _, err := drainExactSHA(bad); err == nil {
			t.Fatalf("accepted inexact candidate %q", bad)
		}
	}
	got, err := drainExactSHA("  " + strings.ToUpper(drainSelftestSHA) + "  ")
	if err != nil || got != drainSelftestSHA {
		t.Fatalf("exact SHA normalization: %q %v", got, err)
	}
}

func TestDrainReviewPacketTargetsConfiguredSupervisor(t *testing.T) {
	packet := drainReviewPacket("FAC-349", drainSelftestSHA, ".herd/worktrees/review", "forge-review-supervisor")
	if !strings.Contains(packet, "REPORT_TARGET: forge-review-supervisor (mandatory; never coordinator)") {
		t.Fatalf("packet must target configured standing supervisor:\n%s", packet)
	}
	if strings.Contains(packet, "REPORT_TARGET: review-harvest-supervisor") || strings.Contains(packet, "REPORT_TARGET: coordinator") {
		t.Fatalf("packet must exclude legacy supervisor and coordinator targets:\n%s", packet)
	}
}

func TestNewDrainAdaptersFailsClosedOnMissingAuthority(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.jsonl")
	reviewerLane := config.LaneDef{Name: "review", Role: launch.ReviewerRole, Worktree: ".herd/worktrees/review"}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		tp   provider.TaskProvider
		want string
	}{
		{name: "no config", tp: &fakeDrainProvider{}, want: "no compiled config authority"},
		{name: "no provider", cfg: &config.Config{Lanes: []config.LaneDef{reviewerLane}}, want: "no board provider authority"},
		{name: "no reviewer lane", cfg: &config.Config{}, tp: &fakeDrainProvider{}, want: "no reviewer lane configured"},
		{name: "reviewer lane without worktree", cfg: &config.Config{Lanes: []config.LaneDef{{Name: "review", Role: launch.ReviewerRole}}}, tp: &fakeDrainProvider{}, want: "no isolated worktree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newDrainAdapters(dir, ledger, tc.cfg, tc.tp, 2); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("wired adapters without authority: %v", err)
			}
		})
	}
}

// TestDrainAdaptersHaveNoScriptOrSiblingDependency asserts the compiled action
// path never reaches for a deleted bin/ script, a shell, or a sibling repo.
func TestDrainAdaptersHaveNoScriptOrSiblingDependency(t *testing.T) {
	for _, name := range []string{"drainadapt.go", "drainselftest.go"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// Absolute-path leaks are preflight's gate; this one is about the
		// deleted script seams and the sibling tree they lived in.
		for _, banned := range []string{"bin/herd-", "zsh", "chainseer", `"sh"`, "sh -c"} {
			if strings.Contains(string(body), banned) {
				t.Fatalf("%s depends on %q", name, banned)
			}
		}
	}
}

func TestDrainAuthorModelRequiresAcceptedBuilderReceipt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "launch-receipts.jsonl")
	t.Setenv("HERD_LAUNCH_RECEIPTS", path)

	if _, err := drainAuthorModel("FAC-1"); err == nil {
		t.Fatal("author model resolved without any receipt")
	}

	sink := &launch.JSONLSink{Path: path}
	if err := sink.Write(launch.Receipt{TaskRef: "FAC-1", Role: launch.ReviewerRole, Model: "reviewer-model", Accepted: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := drainAuthorModel("FAC-1"); err == nil {
		t.Fatal("a reviewer receipt was accepted as builder provenance")
	}
	if err := sink.Write(launch.Receipt{TaskRef: "FAC-1", Role: launch.WorkerRole, Model: "builder-model", Accepted: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := drainAuthorModel("FAC-1"); err == nil {
		t.Fatal("a rejected builder receipt was accepted as provenance")
	}
	if err := sink.Write(launch.Receipt{TaskRef: "FAC-1", Role: launch.WorkerRole, Model: "builder-model", Accepted: true}); err != nil {
		t.Fatal(err)
	}
	model, err := drainAuthorModel("FAC-1")
	if err != nil || model != "builder-model" {
		t.Fatalf("author model=%q err=%v", model, err)
	}
}

// drainAdaptersWithRecord returns adapters whose ledger already holds the
// launch record a harvest admits against.
func drainAdaptersWithRecord(t *testing.T) *drainAdapters {
	t.Helper()
	a, _, _, err := newDrainSelftestAdapters(t.TempDir())
	if err != nil {
		t.Fatalf("wire adapters: %v", err)
	}
	if err := a.launchReview(context.Background(), drainSelftestEvidence()); err != nil {
		t.Fatalf("seed launch record: %v", err)
	}
	return a
}

// FAC-644: the live chainseer ledger records NO lease on any of its 2177 rows
// (the keys "lease", "patch_url" and "verification_digest" never appear), so
// Ledger.Admit -- which binds all four -- could never admit anything. A
// 1327-tip drain reported 318 harvestable and act_harvests=0 with 905 refusals.
//
// The old message blamed each candidate for carrying no provenance, so one
// recorder defect was reported 905 times as 905 suspect candidates. Those need
// opposite responses: investigate a candidate, versus fix the writer. This pins
// the distinction, which the pre-existing provenance test could not catch
// because its synthetic ledger writes Lease "7" -- it proved the gate READS
// provenance, never that any writer PRODUCES it.
func TestAdmissionNamesTheRecorderWhenNoLedgerRowRecordsALease(t *testing.T) {
	a := drainAdaptersWithRecord(t)
	// Strip the lease from every row, reproducing the live ledger's shape.
	rows, err := a.ledger.AllRows()
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	var buf strings.Builder
	for i := range rows {
		rows[i].Lease = ""
		b, err := json.Marshal(rows[i])
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(a.ledger.Path, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = a.admission(context.Background(), drainSelftestSHA)
	if err == nil {
		t.Fatal("a missing lease must still refuse; the binding is real anti-stale-verdict safety")
	}
	if !strings.Contains(err.Error(), "UNSATISFIABLE") {
		t.Errorf("the refusal must name the unsatisfiable gate so an operator fixes the recorder instead of auditing candidates: %v", err)
	}
	if !strings.Contains(err.Error(), "not itself suspect") {
		t.Errorf("the refusal must exonerate the candidate: %v", err)
	}
}

// A ledger that DOES record leases elsewhere keeps the per-candidate message,
// so the unsatisfiable-gate branch cannot swallow a genuine one-candidate gap.
func TestAdmissionKeepsPerCandidateMessageWhenLeasesAreRecordedElsewhere(t *testing.T) {
	a := drainAdaptersWithRecord(t)
	rows, err := a.ledger.AllRows()
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	var buf strings.Builder
	for i := range rows {
		// This candidate loses its lease; a second row keeps one.
		rows[i].Lease = ""
		b, _ := json.Marshal(rows[i])
		buf.Write(b)
		buf.WriteByte('\n')
	}
	other := rows[0]
	other.SHA = strings.Repeat("c", 40)
	other.Lease = "9"
	b, _ := json.Marshal(other)
	buf.Write(b)
	buf.WriteByte('\n')
	if err := os.WriteFile(a.ledger.Path, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = a.admission(context.Background(), drainSelftestSHA)
	if err == nil {
		t.Fatal("a missing lease must refuse")
	}
	if strings.Contains(err.Error(), "UNSATISFIABLE") {
		t.Errorf("a ledger that records leases elsewhere is a per-candidate gap, not an unsatisfiable gate: %v", err)
	}
	if !strings.Contains(err.Error(), "carries no task/lease provenance") {
		t.Errorf("expected the per-candidate message: %v", err)
	}
}
