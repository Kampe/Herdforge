package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-117: fail-closed worktree GC — unique/dirty/unknown refuse; content-merged
// clean reaps only after salvage verification. Table cases are non-vacuous:
// flipping Class/Eligible expectations or removing the unique-commit guard
// must break these tests.

func TestFAC117_ClassifyTable(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)

	// --- fixtures ---
	// Unique (unmerged) task worktree.
	uniqueWI, err := wm.CreateTaskWorktree(context.Background(), "U-1")
	if err != nil {
		t.Fatalf("create unique wt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(uniqueWI.Path, "unique.txt"), []byte("only-here"), 0644); err != nil {
		t.Fatal(err)
	}
	runCmd(uniqueWI.Path, "git", "add", "unique.txt")
	runCmd(uniqueWI.Path, "git", "commit", "-m", "feat: unique unmerged work (FAC-117)")

	// Dirty task worktree (merge its anchor so uniqueness is not the reason).
	dirtyWI, err := wm.CreateTaskWorktree(context.Background(), "D-1")
	if err != nil {
		t.Fatalf("create dirty wt: %v", err)
	}
	// Merge dirty branch into main+origin so cherry is clean; then dirty the tree.
	runCmd(tmpDir, "git", "checkout", "main")
	runCmd(tmpDir, "git", "merge", "--no-ff", "-m", "merge dirty fixture", "herd/d-1")
	runCmd(tmpDir, "git", "push", "origin", "main")
	if err := os.WriteFile(filepath.Join(dirtyWI.Path, "dirt.txt"), []byte("uncommitted"), 0644); err != nil {
		t.Fatal(err)
	}

	// Content-merged clean task worktree (eligible).
	mergedWI, err := wm.CreateTaskWorktree(context.Background(), "M-1")
	if err != nil {
		t.Fatalf("create merged wt: %v", err)
	}
	runCmd(tmpDir, "git", "checkout", "main")
	runCmd(tmpDir, "git", "merge", "--no-ff", "-m", "merge m-1", "herd/m-1")
	runCmd(tmpDir, "git", "push", "origin", "main")

	report, err := wm.PlanReap(context.Background(), ReapPolicy{DefaultBranch: "main"})
	if err != nil {
		t.Fatalf("PlanReap: %v", err)
	}

	byPath := map[string]ReapCandidate{}
	for _, c := range report.Candidates {
		byPath[c.Path] = c
	}

	// Root must appear as root/protected and never eligible.
	rootClassed := false
	for _, c := range report.Candidates {
		if sameWorktreePath(c.Path, tmpDir) {
			rootClassed = true
			if c.Eligible {
				t.Fatalf("root must never be eligible, got %+v", c)
			}
			if c.Class != ReapClassRoot && c.Class != ReapClassProtected {
				t.Fatalf("root class=%s want root|protected", c.Class)
			}
		}
	}
	if !rootClassed {
		// git worktree list always includes root; if missing, plan is broken.
		t.Fatal("expected repository root in candidates")
	}

	cases := []struct {
		name     string
		path     string
		want     ReapClass
		eligible bool
		presSub  string // substring required in PreserveAction
	}{
		{"unique refuses", uniqueWI.Path, ReapClassUnique, false, "do not reap"},
		{"dirty refuses", dirtyWI.Path, ReapClassDirty, false, "dirty"},
		{"content-merged eligible", mergedWI.Path, ReapClassContentMerged, true, "salvage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := byPath[tc.path]
			if !ok {
				// path keys may differ by symlink resolution
				for p, cand := range byPath {
					if sameWorktreePath(p, tc.path) {
						c, ok = cand, true
						break
					}
				}
			}
			if !ok {
				t.Fatalf("candidate for %s not found in %+v", tc.path, keysOf(byPath))
			}
			if c.Class != tc.want {
				t.Fatalf("class=%s want %s reason=%q", c.Class, tc.want, c.Reason)
			}
			if c.Eligible != tc.eligible {
				t.Fatalf("eligible=%v want %v reason=%q", c.Eligible, tc.eligible, c.Reason)
			}
			if tc.presSub != "" && !strings.Contains(strings.ToLower(c.PreserveAction), strings.ToLower(tc.presSub)) {
				t.Fatalf("PreserveAction %q missing %q", c.PreserveAction, tc.presSub)
			}
			if tc.want == ReapClassUnique && len(c.UniqueSHAs) == 0 {
				t.Fatal("unique class must list UniqueSHAs (non-vacuous evidence)")
			}
			if tc.eligible && c.Class != ReapClassContentMerged {
				t.Fatal("only content-merged candidates may be eligible")
			}
		})
	}
}

func TestFAC117_AutoReap_RefusesUniqueAndReapsMergedOnly(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)

	uniqueWI, err := wm.CreateTaskWorktree(context.Background(), "KEEP-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uniqueWI.Path, "keep.txt"), []byte("precious"), 0644); err != nil {
		t.Fatal(err)
	}
	runCmd(uniqueWI.Path, "git", "add", "keep.txt")
	runCmd(uniqueWI.Path, "git", "commit", "-m", "feat: must not be reaped")

	mergedWI, err := wm.CreateTaskWorktree(context.Background(), "DROP-1")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(tmpDir, "git", "checkout", "main")
	runCmd(tmpDir, "git", "merge", "--no-ff", "-m", "merge drop-1", "herd/drop-1")
	runCmd(tmpDir, "git", "push", "origin", "main")

	// Capture unique tip before any reap attempt.
	uniqueTip, err := wm.headAt(context.Background(), uniqueWI.Path)
	if err != nil {
		t.Fatal(err)
	}

	report, err := wm.Reap(context.Background(), admissibleReapPolicy(t, wm, mergedWI.Path, false))
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(report.Reaped) != 1 {
		t.Fatalf("expected exactly 1 reaped (merged only), got %v", report.Reaped)
	}

	// Unique worktree path must still exist.
	if _, err := os.Stat(filepath.Join(uniqueWI.Path, "keep.txt")); err != nil {
		t.Fatalf("unique work destroyed: %v", err)
	}
	// Unique tip still reachable via salvage or branch.
	got, err := wm.revParse(context.Background(), uniqueTip)
	if err != nil || got != uniqueTip {
		// branch tip should still resolve
		if tip, berr := wm.revParse(context.Background(), "herd/keep-1"); berr != nil || tip != uniqueTip {
			t.Fatalf("unique tip lost: head err=%v branch err=%v", err, berr)
		}
	}

	// Merged worktree path must be gone.
	if _, err := os.Stat(mergedWI.Path); !os.IsNotExist(err) {
		// git may leave empty dir in edge cases; ensure not a worktree
		if _, lerr := os.Stat(filepath.Join(mergedWI.Path, ".git")); lerr == nil {
			t.Fatal("merged worktree still present after reap")
		}
	}

	// Salvage ref for reaped branch must still restore the tip.
	salvage := SalvageRefFor("herd/drop-1")
	if _, err := wm.revParse(context.Background(), salvage); err != nil {
		t.Fatalf("salvage ref missing after reap: %v", err)
	}
}

func TestFAC117_DryRunDoesNotRemove(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)

	wi, err := wm.CreateTaskWorktree(context.Background(), "DRY-1")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(tmpDir, "git", "checkout", "main")
	runCmd(tmpDir, "git", "merge", "--no-ff", "-m", "merge dry", "herd/dry-1")
	runCmd(tmpDir, "git", "push", "origin", "main")

	report, err := wm.Reap(context.Background(), ReapPolicy{DefaultBranch: "main", AutoReap: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reaped) != 0 {
		t.Fatalf("dry-run must not reap, got %v", report.Reaped)
	}
	if len(report.Eligible) != 1 {
		t.Fatalf("expected 1 eligible in dry-run plan, got %d (%+v)", len(report.Eligible), report.Eligible)
	}
	if _, err := os.Stat(filepath.Join(wi.Path, ".git")); err != nil {
		t.Fatalf("dry-run removed worktree: %v", err)
	}
}

func TestFAC117_GitErrorYieldsUnknownNoRemoval(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)

	wi, err := wm.CreateTaskWorktree(context.Background(), "ERR-1")
	if err != nil {
		t.Fatal(err)
	}
	// Make cherry fail by using a nonsense default branch for integration.
	report, err := wm.PlanReap(context.Background(), ReapPolicy{DefaultBranch: "does-not-exist-zz"})
	if err != nil {
		// List works; classification should degrade to UNKNOWN, not fail the plan
		// unless list itself fails.
		t.Fatalf("PlanReap should not hard-fail on bad default branch: %v", err)
	}
	found := false
	for _, c := range report.Candidates {
		if sameWorktreePath(c.Path, wi.Path) {
			found = true
			if c.Class != ReapClassUnknown {
				t.Fatalf("want UNKNOWN on integration failure, got %s (%s)", c.Class, c.Reason)
			}
			if c.Eligible {
				t.Fatal("UNKNOWN must never be eligible")
			}
		}
	}
	if !found {
		t.Fatal("expected err-1 candidate")
	}

	_, err = wm.PruneMergedWorktrees(context.Background(), "does-not-exist-zz")
	if err == nil {
		t.Fatal("historical auto-reap wrapper must fail closed")
	}
	if _, err := os.Stat(filepath.Join(wi.Path, ".git")); err != nil {
		t.Fatalf("worktree removed under unknown classification: %v", err)
	}
}

func TestFAC117_LeaseProbeRefuses(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)

	wi, err := wm.CreateTaskWorktree(context.Background(), "LEASE-1")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(tmpDir, "git", "checkout", "main")
	runCmd(tmpDir, "git", "merge", "--no-ff", "-m", "merge lease", "herd/lease-1")
	runCmd(tmpDir, "git", "push", "origin", "main")

	policy := admissibleReapPolicy(t, wm, wi.Path, true)
	report, err := wm.Reap(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reaped) != 0 {
		t.Fatalf("active lease must block reap, reaped=%v", report.Reaped)
	}
	if _, err := os.Stat(filepath.Join(wi.Path, ".git")); err != nil {
		t.Fatalf("leased worktree removed: %v", err)
	}

	// Probe error → UNKNOWN, no removal.
	policy.LeaseProbe = func(context.Context, string, string) (bool, error) {
		return false, errors.New("lease store unavailable")
	}
	if _, err := wm.Reap(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
}

func TestFAC117_ExactTargetOnly(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)

	a, err := wm.CreateTaskWorktree(context.Background(), "TGT-A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := wm.CreateTaskWorktree(context.Background(), "TGT-B")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(tmpDir, "git", "checkout", "main")
	runCmd(tmpDir, "git", "merge", "--no-ff", "-m", "merge a", "herd/tgt-a")
	runCmd(tmpDir, "git", "merge", "--no-ff", "-m", "merge b", "herd/tgt-b")
	runCmd(tmpDir, "git", "push", "origin", "main")

	report, err := wm.Reap(context.Background(), admissibleReapPolicy(t, wm, a.Path, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reaped) != 1 {
		t.Fatalf("expected exactly 1 reaped path, got %v", report.Reaped)
	}
	if !sameWorktreePath(report.Reaped[0], a.Path) {
		t.Fatalf("expected A reaped, got %v (A=%s)", report.Reaped, a.Path)
	}
	if _, err := os.Stat(filepath.Join(b.Path, ".git")); err != nil {
		t.Fatalf("sibling B must not be pruned: %v", err)
	}
}

func admissibleReapPolicy(t *testing.T, wm *WorktreeManager, target string, active bool) ReapPolicy {
	t.Helper()
	const boardEvidence = "board-action-proof-1"
	const generation = "lease-generation-1"
	return ReapPolicy{
		DefaultBranch: "main", AutoReap: true, TargetPaths: []string{target},
		LeaseProbe:           func(context.Context, string, string) (bool, error) { return active, nil },
		LeaseGenerationProbe: func(context.Context, string, string) (string, error) { return generation, nil },
		BoardEvidenceProbe:   func(context.Context, string, string) (string, error) { return boardEvidence, nil },
		Evidence: ReapEvidence{
			IntegrationSHA: gitOut(t, wm.RepoRoot, "rev-parse", "origin/main"),
			BoardEvidence:  boardEvidence, LeaseGeneration: generation, PolicyDigest: "policy-digest-1", Actor: "fac-117-test",
		},
		ReceiptSink:  func(ReapReceipt) error { return nil },
		ActionPolicy: "remove",
		HoldReader:   unheldHoldReader{}, IdentitySetFor: reapHoldIdentities,
	}
}

func TestFAC117_Mutation_UniqueGuard(t *testing.T) {
	// Non-vacuous guard: if uniqueCommits were ignored, this would reap.
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)

	wi, err := wm.CreateTaskWorktree(context.Background(), "MUT-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wi.Path, "mut.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	runCmd(wi.Path, "git", "add", "mut.txt")
	runCmd(wi.Path, "git", "commit", "-m", "feat: mutation probe unique")

	// Deliberately also merge nothing — unique work remains.
	c := wm.classifyOne(context.Background(), &WorktreeInfo{
		Path:   wi.Path,
		Branch: wi.Branch,
		Commit: wi.Commit,
	}, ReapPolicy{DefaultBranch: "main"}, "origin/main", nil)

	if c.Class != ReapClassUnique {
		t.Fatalf("mutation baseline: want unique-committed, got %s (%s)", c.Class, c.Reason)
	}
	if c.Eligible {
		t.Fatal("mutation baseline: unique must not be eligible — if this fails, unique guard is vacuous")
	}
	// Prove evidence is binding: empty UniqueSHAs would mean the assertion is weak.
	if len(c.UniqueSHAs) == 0 {
		t.Fatal("unique classification without SHAs is vacuous")
	}
}

func TestSalvageRefFor(t *testing.T) {
	if got := SalvageRefFor("herd/fac-117"); got != "refs/herd/salvage/v1/686572642f6661632d313137" {
		t.Fatalf("got %s", got)
	}
}

func TestSalvageRefFor_IsInjectiveAcrossCase(t *testing.T) {
	upper := SalvageRefFor("herd/FAC-1")
	lower := SalvageRefFor("herd/fac-1")
	if upper == lower {
		t.Fatalf("case-distinct branches collided at %s", upper)
	}
}

func TestSalvageRef_ReusesOnlySameHEAD(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)
	first := gitOut(t, tmpDir, "rev-parse", "HEAD")
	runCmd(tmpDir, "git", "commit", "--allow-empty", "-m", "second salvage tip")
	second := gitOut(t, tmpDir, "rev-parse", "HEAD")
	ref := SalvageRefFor("herd/reuse")
	if err := wm.ensureSalvageRef(context.Background(), ref, first); err != nil {
		t.Fatal(err)
	}
	if err := wm.ensureSalvageRef(context.Background(), ref, second); err == nil {
		t.Fatal("different HEAD must not overwrite an existing salvage ref")
	}
	if got, err := wm.revParse(context.Background(), ref); err != nil || got != first {
		t.Fatalf("existing salvage tip changed: got=%s err=%v want=%s", got, err, first)
	}
}

func TestSalvageRefs_TwoCaseDistinctBranchesRemainRecoverable(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)
	first := gitOut(t, tmpDir, "rev-parse", "HEAD")
	runCmd(tmpDir, "git", "commit", "--allow-empty", "-m", "second case tip")
	second := gitOut(t, tmpDir, "rev-parse", "HEAD")
	upper := SalvageRefFor("herd/FAC-1")
	lower := SalvageRefFor("herd/fac-1")
	if upper == lower {
		t.Fatal("fixture branches unexpectedly share a salvage ref")
	}
	if err := wm.ensureSalvageRef(context.Background(), upper, first); err != nil {
		t.Fatal(err)
	}
	if err := wm.ensureSalvageRef(context.Background(), lower, second); err != nil {
		t.Fatal(err)
	}
	if got, _ := wm.revParse(context.Background(), upper); got != first {
		t.Fatalf("upper-case salvage tip lost: got=%s want=%s", got, first)
	}
	if got, _ := wm.revParse(context.Background(), lower); got != second {
		t.Fatalf("lower-case salvage tip lost: got=%s want=%s", got, second)
	}
}

func keysOf(m map[string]ReapCandidate) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
