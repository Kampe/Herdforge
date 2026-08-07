package retention

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// fixedNow keeps Age deterministic so two plans of the same tree compare equal.
var fixedNow = func() time.Time { return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC) }

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := testgit.Command(dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// newRepo creates a repository with an origin/main to resolve against.
func newRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// payload.bin is ignored, so a size fixture can consume disk without
	// dirtying the worktree it is measuring.
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("payload.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "README.md", ".gitignore")
	git(t, root, "commit", "-m", "initial")
	git(t, root, "branch", "-M", "main")

	bare := filepath.Join(tmp, "origin.git")
	git(t, root, "init", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	git(t, root, "push", "-u", "origin", "main")
	return root
}

// addWorktree registers a linked worktree in the real Herdforge layout so the
// nested-usage path is exercised, and returns its path.
func addWorktree(t *testing.T, root, branch string) string {
	t.Helper()
	name := strings.ReplaceAll(strings.TrimPrefix(branch, "herd/"), "/", "-")
	path := filepath.Join(root, ".herd", "worktrees", name)
	git(t, root, "worktree", "add", "-b", branch, path, "main")
	return path
}

func commitIn(t *testing.T, path, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, path, "add", file)
	git(t, path, "commit", "-m", "work "+file)
}

func writeIn(t *testing.T, path, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// completeTruth is a fully answered Truth for a done, clean task lane. Tests
// mutate exactly the field under test so a missing-field assertion cannot pass
// for the wrong reason.
func completeTruth(ref string) Truth {
	return Truth{
		TaskRef:         ref,
		TaskStatus:      StatusDone,
		Purpose:         PurposeTask,
		LeaseGeneration: "gen-7",
		SessionKnown:    true,
		PRState:         "merged",
		ReviewReceipt:   "receipt-" + ref,
		GraphRevision:   "graph-rev-1",
	}
}

// truthTable answers per branch and refuses any branch it was not told about,
// so a fixture worktree can never silently take a default.
func truthTable(m map[string]Truth) TruthProbe {
	return func(_ context.Context, _, branch string) (Truth, error) {
		if tr, ok := m[branch]; ok {
			return tr, nil
		}
		return Truth{}, fmt.Errorf("no truth recorded for %s", branch)
	}
}

func planOrDie(t *testing.T, root string, policy Policy) *Report {
	t.Helper()
	if policy.Now == nil {
		policy.Now = fixedNow
	}
	if policy.MinEvidenceGenerations == 0 {
		policy.MinEvidenceGenerations = 1
	}
	report, err := Plan(context.Background(), worktree.NewWorktreeManager(root), policy)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return report
}

func byBranch(t *testing.T, report *Report, branch string) Entry {
	t.Helper()
	for _, e := range report.Entries {
		if e.Branch == branch {
			return e
		}
	}
	t.Fatalf("branch %q absent from plan", branch)
	return Entry{}
}

func byPath(t *testing.T, report *Report, path string) Entry {
	t.Helper()
	for _, e := range report.Entries {
		if absClean(e.Path) == absClean(path) {
			return e
		}
	}
	t.Fatalf("path %q absent from plan", path)
	return Entry{}
}

// -- acceptance: every entry classified exactly once ------------------------

func TestFAC179_EveryWorktreeClassifiedExactlyOnceAndNoUnknownIsEligible(t *testing.T) {
	root := newRepo(t)
	truth := map[string]Truth{}

	// A spread of shapes plus filler, so the invariant is checked at fleet
	// scale rather than on a hand-picked handful.
	for i := 0; i < 24; i++ {
		branch := fmt.Sprintf("herd/fac-%03d", i)
		path := addWorktree(t, root, branch)
		tr := completeTruth(fmt.Sprintf("FAC-%03d", i))
		switch i % 6 {
		case 0:
			tr.TaskStatus = StatusActive
		case 1:
			tr.TaskStatus = StatusInReview
		case 2:
			tr.Quarantined = true
		case 3:
			writeIn(t, path, "scratch.txt", "uncommitted")
		case 4:
			commitIn(t, path, "extra.txt", "unique work")
		}
		truth[branch] = tr
	}
	// One worktree whose truth is deliberately absent: the probe errors.
	orphan := "herd/fac-orphan"
	addWorktree(t, root, orphan)

	report := planOrDie(t, root, Policy{Truth: truthTable(truth)})

	wm := worktree.NewWorktreeManager(root)
	list, err := wm.ListWorktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Entries) != len(list) {
		t.Fatalf("classified %d entries, git registers %d", len(report.Entries), len(list))
	}

	seen := map[string]bool{}
	total := 0
	for _, e := range report.Entries {
		if seen[e.Path] {
			t.Fatalf("path classified twice: %s", e.Path)
		}
		seen[e.Path] = true
		if e.Class == classPending {
			t.Fatalf("%s escaped with the pending sentinel", e.Branch)
		}
		if e.Class == ClassUnknown && e.Eligible {
			t.Fatalf("%s is UNKNOWN and eligible", e.Branch)
		}
	}
	for _, n := range report.Counts {
		total += n
	}
	if total != len(report.Entries) {
		t.Fatalf("class counts sum to %d, want %d", total, len(report.Entries))
	}
	if len(report.Eligible)+len(report.Refused) != len(report.Entries) {
		t.Fatalf("eligible+refused = %d, want %d",
			len(report.Eligible)+len(report.Refused), len(report.Entries))
	}

	if got := byBranch(t, report, orphan); got.Class != ClassUnknown || got.Eligible {
		t.Fatalf("unreadable truth gave class %s eligible=%v, want unknown/false", got.Class, got.Eligible)
	}
}

// -- acceptance: the protected set is protected -----------------------------

func TestFAC179_ProtectedShapesAreNeverEligible(t *testing.T) {
	root := newRepo(t)

	activeA := addWorktree(t, root, "herd/fac-151")
	activeB := addWorktree(t, root, "herd/fac-175")
	addWorktree(t, root, "herd/fac-159-v3")
	addWorktree(t, root, "herd/fac-172")
	dirty := addWorktree(t, root, "herd/fac-dirty")
	unique := addWorktree(t, root, "herd/fac-unique")
	addWorktree(t, root, "herd/fac-rejected-review")
	// A non-herd branch worktree is outside retention scope entirely.
	git(t, root, "worktree", "add", "-b", "spike/manual",
		filepath.Join(root, ".herd", "worktrees", "spike-manual"), "main")

	_ = activeA
	_ = activeB
	writeIn(t, dirty, "wip.txt", "half-written")
	commitIn(t, unique, "patch.txt", "not on main")

	t151 := completeTruth("FAC-151")
	t151.TaskStatus = StatusActive
	t175 := completeTruth("FAC-175")
	t175.SessionActive = true
	t159 := completeTruth("FAC-159")
	t159.TaskStatus = StatusInReview
	t159.Purpose = PurposeReview
	t159.Generation = 3
	t172 := completeTruth("FAC-172")
	t172.Quarantined = true
	tRejected := completeTruth("FAC-166")
	tRejected.Purpose = PurposeReview
	tRejected.Generation = 2
	tRejected.RequiredEvidence = true

	report := planOrDie(t, root, Policy{Truth: truthTable(map[string]Truth{
		"herd/fac-151":             t151,
		"herd/fac-175":             t175,
		"herd/fac-159-v3":          t159,
		"herd/fac-172":             t172,
		"herd/fac-dirty":           completeTruth("FAC-DIRTY"),
		"herd/fac-unique":          completeTruth("FAC-UNIQUE"),
		"herd/fac-rejected-review": tRejected,
	})})

	want := map[string]Class{
		"herd/fac-151":             ClassActive,
		"herd/fac-175":             ClassActive,
		"herd/fac-159-v3":          ClassReviewHeld,
		"herd/fac-172":             ClassQuarantined,
		"herd/fac-dirty":           ClassDirty,
		"herd/fac-unique":          ClassUnique,
		"herd/fac-rejected-review": ClassReviewHeld,
		"spike/manual":             ClassProtected,
	}
	for branch, wantClass := range want {
		got := byBranch(t, report, branch)
		if got.Class != wantClass {
			t.Errorf("%q class = %s, want %s (reason %q)", branch, got.Class, wantClass, got.Reason)
		}
		if got.Eligible {
			t.Errorf("%q is eligible; protected shapes must never be", branch)
		}
	}
	if rootEntry := byPath(t, report, root); rootEntry.Class != ClassRoot || rootEntry.Eligible {
		t.Errorf("root class=%s eligible=%v, want root/false", rootEntry.Class, rootEntry.Eligible)
	}
	if len(report.Eligible) != 0 {
		t.Fatalf("eligible = %d, want 0: %+v", len(report.Eligible), report.Eligible)
	}
}

func TestFAC179_RootIsClassifiedRootNotProtected(t *testing.T) {
	root := newRepo(t)
	report := planOrDie(t, root, Policy{Truth: truthTable(nil)})
	if got := byPath(t, report, root); got.Class != ClassRoot {
		t.Fatalf("root class = %s, want %s", got.Class, ClassRoot)
	}
	if report.Counts[ClassRoot] != 1 {
		t.Fatalf("root count = %d, want 1", report.Counts[ClassRoot])
	}
}

// -- acceptance: superseded generations, evidence floor ---------------------

func TestFAC179_SupersededGenerationEligibleOnlyAfterEvidenceFloor(t *testing.T) {
	root := newRepo(t)
	truth := map[string]Truth{}
	for gen := 1; gen <= 3; gen++ {
		branch := fmt.Sprintf("herd/fac-160-audit-v%d", gen)
		addWorktree(t, root, branch)
		tr := completeTruth("FAC-160")
		tr.Purpose = PurposeAudit
		tr.Generation = gen
		truth[branch] = tr
	}
	// A sibling lane that must never be consulted by FAC-160's floor.
	sibling := "herd/fac-161-audit-v1"
	addWorktree(t, root, sibling)
	sib := completeTruth("FAC-161")
	sib.Purpose = PurposeAudit
	sib.Generation = 1
	truth[sibling] = sib

	for _, tc := range []struct {
		min      int
		eligible []string
	}{
		{min: 1, eligible: []string{"herd/fac-160-audit-v1", "herd/fac-160-audit-v2"}},
		{min: 2, eligible: []string{"herd/fac-160-audit-v1"}},
		{min: 3, eligible: nil},
		{min: 4, eligible: nil},
	} {
		t.Run(fmt.Sprintf("min=%d", tc.min), func(t *testing.T) {
			report := planOrDie(t, root, Policy{
				Truth:                  truthTable(truth),
				MinEvidenceGenerations: tc.min,
			})
			var got []string
			for _, e := range report.Eligible {
				got = append(got, e.Branch)
			}
			if !reflect.DeepEqual(got, tc.eligible) {
				t.Fatalf("eligible = %v, want %v", got, tc.eligible)
			}
			// The newest generation is never the reclaim target, at any floor.
			newest := byBranch(t, report, "herd/fac-160-audit-v3")
			if newest.Eligible {
				t.Fatal("newest evidence generation is eligible")
			}
			if newest.Class != ClassRecoverable {
				t.Fatalf("newest generation class = %s, want %s (only older generations are superseded)",
					newest.Class, ClassRecoverable)
			}
			// The sibling lane keeps its own floor, independent of FAC-160.
			if sib := byBranch(t, report, sibling); sib.Eligible {
				t.Fatalf("sibling lane %s became eligible under FAC-160's floor", sibling)
			}
			for _, b := range []string{"herd/fac-160-audit-v1", "herd/fac-160-audit-v2"} {
				if e := byBranch(t, report, b); e.Class != ClassSuperseded {
					t.Fatalf("%s class = %s, want %s", b, e.Class, ClassSuperseded)
				}
			}
		})
	}
}

func TestFAC179_ActiveNewerGenerationStillSupersedesOlderOnes(t *testing.T) {
	root := newRepo(t)
	addWorktree(t, root, "herd/fac-162-audit-v1")
	addWorktree(t, root, "herd/fac-162-audit-v2")

	old := completeTruth("FAC-162")
	old.Purpose = PurposeAudit
	old.Generation = 1
	live := completeTruth("FAC-162")
	live.Purpose = PurposeAudit
	live.Generation = 2
	live.TaskStatus = StatusActive
	live.SessionActive = true

	report := planOrDie(t, root, Policy{Truth: truthTable(map[string]Truth{
		"herd/fac-162-audit-v1": old,
		"herd/fac-162-audit-v2": live,
	})})

	// The newest generation is active and occupies the whole min=1 floor, so the
	// older one stays superseded but is not retained by the floor.
	if e := byBranch(t, report, "herd/fac-162-audit-v1"); e.Class != ClassSuperseded || !e.Eligible {
		t.Fatalf("v1 class=%s eligible=%v, want superseded/true", e.Class, e.Eligible)
	}
	if e := byBranch(t, report, "herd/fac-162-audit-v2"); e.Class != ClassActive || e.Eligible {
		t.Fatalf("v2 class=%s eligible=%v, want active/false", e.Class, e.Eligible)
	}
}

func TestFAC179_AbandonedLaneIsReclaimable(t *testing.T) {
	root := newRepo(t)
	addWorktree(t, root, "herd/fac-163")
	tr := completeTruth("FAC-163")
	tr.TaskStatus = StatusAbandoned
	tr.PRState = "closed"

	report := planOrDie(t, root, Policy{Truth: truthTable(map[string]Truth{"herd/fac-163": tr})})
	e := byBranch(t, report, "herd/fac-163")
	if e.Class != ClassAbandoned || !e.Eligible {
		t.Fatalf("class=%s eligible=%v, want abandoned/true", e.Class, e.Eligible)
	}
}

// -- acceptance: missing truth refuses and raises attention -----------------

func TestFAC179_EachMissingTruthSourceRefusesAndNamesItself(t *testing.T) {
	// Plan is read-only, so one fixture serves every case.
	root := newRepo(t)
	addWorktree(t, root, "herd/fac-164")

	for _, tc := range []struct {
		name    string
		mutate  func(*Truth)
		missing string
	}{
		{"board task ref", func(tr *Truth) { tr.TaskRef = "" }, "board task ref"},
		{"board status", func(tr *Truth) { tr.TaskStatus = "" }, "board status"},
		{"invalid board status", func(tr *Truth) { tr.TaskStatus = "Done" }, "board status"},
		{"purpose", func(tr *Truth) { tr.Purpose = "" }, "worktree purpose"},
		{"lease generation", func(tr *Truth) { tr.LeaseGeneration = "" }, "lease generation"},
		{"herdr session", func(tr *Truth) { tr.SessionKnown = false }, "herdr session state"},
		{"pr state", func(tr *Truth) { tr.PRState = "" }, "pull request state"},
		{"review receipt", func(tr *Truth) { tr.ReviewReceipt = "" }, "review receipt"},
		{"graph revision", func(tr *Truth) { tr.GraphRevision = "" }, "graph revision"},
		{"audit generation", func(tr *Truth) { tr.Purpose = PurposeAudit; tr.Generation = 0 }, "generation number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := completeTruth("FAC-164")
			tc.mutate(&tr)

			var sunk []Attention
			report := planOrDie(t, root, Policy{
				Truth:         truthTable(map[string]Truth{"herd/fac-164": tr}),
				AttentionSink: func(a Attention) error { sunk = append(sunk, a); return nil },
			})

			e := byBranch(t, report, "herd/fac-164")
			if e.Class != ClassUnknown || e.Eligible {
				t.Fatalf("class=%s eligible=%v, want unknown/false", e.Class, e.Eligible)
			}
			found := false
			for _, m := range e.MissingTruth {
				if m == tc.missing {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing truth %v does not name %q", e.MissingTruth, tc.missing)
			}
			if len(sunk) != 1 || sunk[0].Branch != "herd/fac-164" {
				t.Fatalf("attention sink got %+v, want one record for the refused branch", sunk)
			}
			if strings.Contains(sunk[0].Reason, root) {
				t.Fatalf("attention evidence leaked an absolute path: %q", sunk[0].Reason)
			}
		})
	}
}

// A probe that answers *and* errors is the dangerous shape: the answer looks
// complete, so only the error itself can refuse it.
func TestFAC179_PopulatedTruthAlongsideAProbeErrorIsStillRefused(t *testing.T) {
	root := newRepo(t)
	addWorktree(t, root, "herd/fac-167")

	report := planOrDie(t, root, Policy{
		Truth: func(context.Context, string, string) (Truth, error) {
			return completeTruth("FAC-167"), errors.New("board read timed out after a cached hit")
		},
	})

	e := byBranch(t, report, "herd/fac-167")
	if e.Class != ClassUnknown || e.Eligible {
		t.Fatalf("class=%s eligible=%v, want unknown/false", e.Class, e.Eligible)
	}
	if !strings.Contains(e.Reason, "board read timed out") {
		t.Fatalf("reason %q does not carry the probe error", e.Reason)
	}
}

// Durable attention evidence must be movable between worktrees, so it can
// never quote a probe's or Git's own path-bearing output.
func TestFAC179_AttentionEvidenceNeverQuotesProbeOutput(t *testing.T) {
	root := newRepo(t)
	addWorktree(t, root, "herd/fac-168")
	leak := filepath.Join(root, ".herd", "worktrees", "fac-168")

	report := planOrDie(t, root, Policy{
		Truth: func(context.Context, string, string) (Truth, error) {
			return Truth{}, fmt.Errorf("kaneo probe failed reading %s", leak)
		},
	})

	if e := byBranch(t, report, "herd/fac-168"); !strings.Contains(e.Reason, leak) {
		t.Fatalf("entry reason %q dropped the probe detail operators need", e.Reason)
	}
	if len(report.Attention) != 1 {
		t.Fatalf("attention = %d records, want 1", len(report.Attention))
	}
	for _, a := range report.Attention {
		if strings.Contains(a.Reason, leak) || filepath.IsAbs(a.Reason) {
			t.Fatalf("attention evidence leaked a path: %q", a.Reason)
		}
	}
}

func TestFAC179_AttentionSinkFailureFailsClosed(t *testing.T) {
	root := newRepo(t)
	addWorktree(t, root, "herd/fac-165")
	boom := errors.New("durable sink unavailable")

	_, err := Plan(context.Background(), worktree.NewWorktreeManager(root), Policy{
		MinEvidenceGenerations: 1,
		Now:                    fixedNow,
		Truth:                  truthTable(nil), // every branch refuses -> attention
		AttentionSink:          func(Attention) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the sink failure to surface", err)
	}
}

func TestFAC179_PolicyWithoutTruthOrEvidenceFloorIsRejected(t *testing.T) {
	root := newRepo(t)
	wm := worktree.NewWorktreeManager(root)

	if _, err := Plan(context.Background(), wm, Policy{MinEvidenceGenerations: 1}); err == nil {
		t.Fatal("a plan with no truth probe was accepted")
	}
	if _, err := Plan(context.Background(), wm, Policy{Truth: truthTable(nil)}); err == nil {
		t.Fatal("a plan with a zero evidence floor was accepted")
	}
	if _, err := Plan(context.Background(), nil, Policy{Truth: truthTable(nil), MinEvidenceGenerations: 1}); err == nil {
		t.Fatal("a plan with no worktree manager was accepted")
	}
}

// -- acceptance: idempotent, deterministic, read-only -----------------------

func TestFAC179_RepeatedPlansAreIdenticalAndMutateNothing(t *testing.T) {
	root := newRepo(t)
	truth := map[string]Truth{}
	for _, spec := range []struct {
		branch string
		gen    int
	}{{"herd/fac-170-audit-v1", 1}, {"herd/fac-170-audit-v2", 2}, {"herd/fac-171", 0}} {
		addWorktree(t, root, spec.branch)
		tr := completeTruth("FAC-" + strings.TrimPrefix(spec.branch, "herd/fac-"))
		if spec.gen > 0 {
			tr.Purpose = PurposeAudit
			tr.Generation = spec.gen
		}
		truth[spec.branch] = tr
	}

	before := gitState(t, root)
	first := planOrDie(t, root, Policy{Truth: truthTable(truth)})
	second := planOrDie(t, root, Policy{Truth: truthTable(truth)})
	after := gitState(t, root)

	if before != after {
		t.Fatalf("planning mutated repository state:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two plans of an unchanged tree differ")
	}
	if len(first.Eligible) == 0 {
		t.Fatal("fixture produced no eligible entry; the idempotence check would be vacuous")
	}
}

func gitState(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	for _, args := range [][]string{
		{"worktree", "list", "--porcelain"},
		{"for-each-ref", "--format=%(refname) %(objectname)"},
	} {
		out, err := testgit.Command(root, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		b.Write(out)
	}
	return b.String()
}

func TestFAC179_PlanOrderIsRiskThenTicketRef(t *testing.T) {
	root := newRepo(t)
	truth := map[string]Truth{}
	for _, ref := range []string{"FAC-300", "FAC-100", "FAC-200"} {
		branch := "herd/" + strings.ToLower(ref)
		addWorktree(t, root, branch)
		tr := completeTruth(ref)
		tr.TaskStatus = StatusActive
		truth[branch] = tr
	}
	report := planOrDie(t, root, Policy{Truth: truthTable(truth)})

	var active []string
	lastRisk := 1 << 30
	for _, e := range report.Entries {
		if e.Risk > lastRisk {
			t.Fatalf("risk ascended at %s: %d after %d", e.Branch, e.Risk, lastRisk)
		}
		lastRisk = e.Risk
		if e.Class == ClassActive {
			active = append(active, e.TaskRef)
		}
	}
	if want := []string{"FAC-100", "FAC-200", "FAC-300"}; !reflect.DeepEqual(active, want) {
		t.Fatalf("ticket order within class = %v, want %v", active, want)
	}
}

// -- acceptance: disk pressure prioritises without widening eligibility -----

func TestFAC179_PressurePrioritisesButNeverWidensEligibility(t *testing.T) {
	root := newRepo(t)
	truth := map[string]Truth{}
	// Two reclaimable generations of differing size, plus a dirty worktree that
	// pressure must not be able to unlock.
	for gen, size := range map[int]int{1: 64, 2: 40960} {
		branch := fmt.Sprintf("herd/fac-180-audit-v%d", gen)
		path := addWorktree(t, root, branch)
		// Ignored payload: real disk usage, still a clean content-merged tree.
		writeIn(t, path, "payload.bin", strings.Repeat("x", size))
		tr := completeTruth("FAC-180")
		tr.Purpose = PurposeAudit
		tr.Generation = gen
		truth[branch] = tr
	}
	addWorktree(t, root, "herd/fac-180-audit-v3")
	newest := completeTruth("FAC-180")
	newest.Purpose = PurposeAudit
	newest.Generation = 3
	truth["herd/fac-180-audit-v3"] = newest

	dirty := addWorktree(t, root, "herd/fac-181")
	writeIn(t, dirty, "wip.txt", strings.Repeat("y", 8192))
	truth["herd/fac-181"] = completeTruth("FAC-181")

	calm := planOrDie(t, root, Policy{Truth: truthTable(truth)})
	pressed := planOrDie(t, root, Policy{Truth: truthTable(truth), PressureBytes: 1})

	if calm.Pressured {
		t.Fatal("unpressured plan reported pressure")
	}
	if !pressed.Pressured {
		t.Fatal("pressured plan did not report pressure")
	}
	if got, want := eligibleSet(pressed), eligibleSet(calm); !reflect.DeepEqual(got, want) {
		t.Fatalf("pressure changed the eligible set: %v vs %v", got, want)
	}
	if len(pressed.Eligible) < 2 {
		t.Fatalf("fixture produced %d eligible entries; ordering check needs at least 2", len(pressed.Eligible))
	}
	for i := 1; i < len(pressed.Eligible); i++ {
		if pressed.Eligible[i-1].Bytes < pressed.Eligible[i].Bytes {
			t.Fatalf("pressured order is not largest-first: %v", pressed.Eligible)
		}
	}
	if e := byBranch(t, pressed, "herd/fac-181"); e.Eligible {
		t.Fatal("pressure made a dirty worktree eligible")
	}
	if e := byBranch(t, pressed, "herd/fac-180-audit-v3"); e.Eligible {
		t.Fatal("pressure reclaimed the newest evidence generation")
	}
	sawPressure := false
	for _, a := range pressed.Attention {
		if a.Class == "pressure" {
			sawPressure = true
		}
	}
	if !sawPressure {
		t.Fatal("pressure raised no attention evidence")
	}
}

func eligibleSet(r *Report) map[string]bool {
	set := map[string]bool{}
	for _, e := range r.Eligible {
		set[e.Branch] = true
	}
	return set
}

// -- accounting and the action seam ----------------------------------------

func TestFAC179_RootUsageExcludesNestedRegisteredWorktrees(t *testing.T) {
	root := newRepo(t)
	path := addWorktree(t, root, "herd/fac-190")
	payload := strings.Repeat("z", 200_000)
	writeIn(t, path, "payload.bin", payload)

	report := planOrDie(t, root, Policy{Truth: truthTable(map[string]Truth{
		"herd/fac-190": completeTruth("FAC-190"),
	})})

	child := byBranch(t, report, "herd/fac-190")
	rootEntry := byPath(t, report, root)
	if !child.SizeKnown || !rootEntry.SizeKnown {
		t.Fatal("usage was not measured")
	}
	if child.Bytes < int64(len(payload)) {
		t.Fatalf("child bytes = %d, want at least %d", child.Bytes, len(payload))
	}
	if rootEntry.Bytes >= child.Bytes {
		t.Fatalf("root bytes %d absorbed the nested worktree's %d", rootEntry.Bytes, child.Bytes)
	}
	if report.TotalBytes < child.Bytes {
		t.Fatalf("total bytes %d excludes the child's %d", report.TotalBytes, child.Bytes)
	}
}

func TestFAC179_ActionTargetsAreExactlyTheEligiblePaths(t *testing.T) {
	root := newRepo(t)
	truth := map[string]Truth{}
	for gen := 1; gen <= 2; gen++ {
		branch := fmt.Sprintf("herd/fac-195-audit-v%d", gen)
		addWorktree(t, root, branch)
		tr := completeTruth("FAC-195")
		tr.Purpose = PurposeAudit
		tr.Generation = gen
		truth[branch] = tr
	}
	dirty := addWorktree(t, root, "herd/fac-196")
	writeIn(t, dirty, "wip.txt", "x")
	truth["herd/fac-196"] = completeTruth("FAC-196")

	report := planOrDie(t, root, Policy{Truth: truthTable(truth)})
	targets := report.ActionTargets()
	if len(targets) != len(report.Eligible) {
		t.Fatalf("targets = %d, eligible = %d", len(targets), len(report.Eligible))
	}
	for i, e := range report.Eligible {
		if targets[i] != e.Path {
			t.Fatalf("target %d = %q, want %q", i, targets[i], e.Path)
		}
	}
	for _, target := range targets {
		if target == dirty || target == root {
			t.Fatalf("action targets included a protected path: %s", target)
		}
	}
	if (*Report)(nil).ActionTargets() != nil {
		t.Fatal("nil report produced targets")
	}
}
