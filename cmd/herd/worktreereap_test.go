package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// FAC-672: a standing lane's RESIDENT HOME tracks the base and therefore has no
// unique commits, which makes it look exactly like a landed task worktree.
// Caught in dry run before any --apply: the coordinator's own home
// (standing/orchestrator) was classified removable. A reaper that takes out the
// coordinator is worse than one that reclaims nothing, and "no unique commits"
// is precisely the signal that cannot tell the two apart on its own.
func TestResidentHomesAreNeverClassifiedRemovable(t *testing.T) {
	for _, c := range []struct{ branch, path string }{
		{"standing/orchestrator", "/x/chainseer-orchestrator"},
		{"standing/docs-custodian", "/x/wt/docs"},
		{"main", "/x/repo"},
		{"master", "/x/repo"},
		{"feat/thing", "/x/review-harvest-supervisor"},
		{"feat/thing", "/x/herd-smith"},
	} {
		if !isResidentHome(c.branch, c.path) {
			t.Errorf("branch=%q path=%q must be protected as a resident home", c.branch, c.path)
		}
	}
}

// An ordinary task worktree must remain reapable, or the leak this exists to
// close simply continues.
func TestOrdinaryTaskWorktreesRemainReapable(t *testing.T) {
	for _, c := range []struct{ branch, path string }{
		{"fix/cha-2086-gating-finality", "/x/.herd/worktrees/cha-2086"},
		{"harvest/cha-2078", "/x/.herd/worktrees/harvest-cha-2078"},
		{"feat/whatever", "/x/worktrees/whatever"},
	} {
		if isResidentHome(c.branch, c.path) {
			t.Errorf("branch=%q path=%q is a task worktree and must stay reapable", c.branch, c.path)
		}
	}
}

// commitsAhead must never report an unanswerable comparison as zero: zero means
// "nothing unique here", which is the signal that authorises removal.
func TestUnanswerableComparisonIsNotZero(t *testing.T) {
	got := commitsAhead(t.TempDir(), "origin/main", "no-such-branch")
	if got == 0 {
		t.Fatal("a failed comparison must not read as 'no unique commits'; that would authorise deleting unmerged work")
	}
	if got != -1 {
		t.Errorf("expected -1 for unknown, got %d", got)
	}
}

// FAC-673: retiring on PR closure ALONE would destroy abandoned work. A closed
// PR whose patch never landed is not finished work, and its worktree may hold
// the only copy. Both halves are required: closed AND verifiably landed.
func TestPatchIsInBaseUsesIdentityNotAncestry(t *testing.T) {
	root := t.TempDir()
	run := func(a ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", root}, a...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("base\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "base")

	// A branch whose change is NOT in main: must not read as landed, or the
	// reaper would delete the only copy of unmerged work.
	run("checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("work\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "feature work")
	run("checkout", "-q", "main")
	if patchIsInBase(root, "feature", "main") {
		t.Fatal("unlanded work must NEVER report as in base; that authorises deleting it")
	}

	// Cherry-pick it onto main: a DIFFERENT sha, same patch. Ancestry says no,
	// patch identity says yes -- and patch identity is the truthful answer.
	run("cherry-pick", "feature")
	// Whether ancestry still reports the branch as ahead depends on how git
	// collapses the graph, and that is incidental. The claim under test is the
	// behaviour: a landing that changed the SHA must still be RECOGNISED as
	// landed, so the reaper does not strand work that genuinely merged.
	if !patchIsInBase(root, "feature", "main") {
		t.Error("a rebased or cherry-picked landing must be recognised by patch identity")
	}
}

// With no recorded PR this is simply not the function's business, and it must
// say so rather than declining with a reason that reads like a problem.
func TestPRClosureCheckIsSilentWithoutARecordedPR(t *testing.T) {
	ok, why := prClosedAndLanded(t.TempDir(), "feature", "main")
	if ok {
		t.Fatal("no recorded PR cannot authorise retirement")
	}
	if why != "" {
		t.Errorf("absence of a PR is not a decline reason, got %q", why)
	}
}

// FAC-676: the contract is one standing lane = one mutable task worktree plus
// its resident home. Nothing enforced or even measured it, so lanes accumulated
// surfaces silently: 10 lanes over-allocated on the live repository, with
// perf-cost-guard holding 15. That is the accumulation MECHANISM, distinct from
// the retirement leak -- fixing only retirement means sweeping forever.
func TestLaneAllocationNamesOverAllocatedLanes(t *testing.T) {
	lanes := []string{"docs-custodian", "qa-sentinel"}
	entries := []worktreeEntry{
		{Path: "/x/wt/docs-custodian", Branch: "standing/docs-custodian"}, // home: entitled
		{Path: "/x/a", Branch: "harvest/docs-custodian-1"},
		{Path: "/x/b", Branch: "harvest/docs-custodian-2"},
		{Path: "/x/c", Branch: "fix/qa-sentinel-only-one"},
	}
	got := laneAllocations(entries, lanes)
	if len(got) != 1 {
		t.Fatalf("only the over-allocated lane must be reported, got %+v", got)
	}
	if got[0].Lane != "docs-custodian" {
		t.Errorf("wrong lane: %q", got[0].Lane)
	}
	// The home is entitled and must NOT count, or every healthy lane reads as
	// over-allocated by one.
	if len(got[0].TaskPaths) != 2 || got[0].Excess != 1 {
		t.Errorf("home must be excluded from the task count: %+v", got[0])
	}
	if got[0].Home == "" {
		t.Error("the home should still be identified, just not counted")
	}
}

// A lane at exactly one task worktree is meeting the contract, not violating it.
func TestLaneAtTheContractIsNotReported(t *testing.T) {
	got := laneAllocations([]worktreeEntry{
		{Path: "/x/home", Branch: "standing/qa-sentinel"},
		{Path: "/x/one", Branch: "fix/qa-sentinel-task"},
	}, []string{"qa-sentinel"})
	if len(got) != 0 {
		t.Fatalf("one task worktree plus a home is the contract: %+v", got)
	}
}

// A lane whose name prefixes another must not claim the other's surfaces --
// the same boundary rule agent identity needed in FAC-660.
func TestLaneAttributionPrefersTheLongestLaneName(t *testing.T) {
	lanes := []string{"review", "review-harvest"}
	if got := laneOwning("fix/review-harvest-thing", "/x/p", lanes); got != "review-harvest" {
		t.Errorf("longest lane must win, got %q", got)
	}
	if got := laneOwning("fix/review-thing", "/x/p", lanes); got != "review" {
		t.Errorf("short lane must still match its own, got %q", got)
	}
	if got := laneOwning("fix/unrelated", "/x/p", lanes); got != "" {
		t.Errorf("an unowned surface must be claimed by nobody, got %q", got)
	}
}

// FAC-681: `--apply --json` reported applied=true with the path listed as landed
// and exited 0, while the directory and its registration were still there. The
// JSON branch returned BEFORE the removal loop, and `applied` echoed the FLAG
// rather than the outcome -- so it reported what was ASKED, not what HAPPENED.
//
// A reaper that claims it retired something it did not is worse than one that
// retires nothing, because the caller stops checking.
func TestRetireLandedReportsOnlyWhatItActuallyRemoved(t *testing.T) {
	root := t.TempDir()
	run := func(a ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", root}, a...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	run("add", ".")
	run("commit", "-qm", "base")

	dir := filepath.Join(root, "wt-landed")
	run("worktree", "add", "-q", "-b", "landed-branch", dir)
	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()

	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
	retired, failed := retireLandedWithInspector(root, []reapRow{{Path: dir, Branch: "landed-branch", Head: strings.TrimSpace(string(head)), Class: "landed"}}, clean)
	if len(retired) != 1 || len(failed) != 0 {
		t.Fatalf("a real removal must be reported as retired: retired=%v failed=%v", retired, failed)
	}
	// The claim is verified against the filesystem, not the exit status -- the
	// whole defect was a command trusting its own success report.
	if worktreeExists(dir) {
		t.Fatal("reported retired while the directory still exists")
	}
	if out, err := exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", "refs/heads/landed-branch").CombinedOutput(); err == nil {
		t.Fatalf("reported retired while the branch still exists: %s", out)
	}
}

// A patch-equivalent rebase/cherry-pick is landed but not merged by ancestry.
// `git branch -d` refuses that branch; retirement must use the prior patch proof
// as authority and remove both the worktree and branch.
func TestRetireLandedDeletesPatchLandedNonAncestorBranch(t *testing.T) {
	root := t.TempDir()
	run := func(a ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", root}, a...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "base")
	dir := filepath.Join(root, "wt-patch-landed")
	run("worktree", "add", "-q", "-b", "patch-landed", dir)
	os.WriteFile(filepath.Join(dir, "change.txt"), []byte("landed\n"), 0o644)
	run("-C", dir, "add", ".")
	run("-C", dir, "commit", "-qm", "change")
	// Force main to a different parent before cherry-pick, so patch identity is
	// equal while ancestry and commit SHA are provably different.
	run("commit", "--allow-empty", "-qm", "main diverges")
	run("cherry-pick", "patch-landed")
	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()

	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
	retired, failed := retireLandedWithInspector(root, []reapRow{{Path: dir, Branch: "patch-landed", Head: strings.TrimSpace(string(head)), Class: "landed"}}, clean)
	if len(retired) != 1 || len(failed) != 0 {
		t.Fatalf("patch-landed branch must retire transactionally: retired=%v failed=%v", retired, failed)
	}
	if worktreeExists(dir) {
		t.Fatal("worktree survived transactional retirement")
	}
	if err := exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", "refs/heads/patch-landed").Run(); err == nil {
		t.Fatal("branch survived transactional retirement")
	}
}

func TestRetireLandedRestoresWorktreeWhenBranchDeletionFails(t *testing.T) {
	root := t.TempDir()
	exec.Command("git", "-C", root, "init", "-q", "-b", "main").Run()
	exec.Command("git", "-C", root, "config", "user.email", "t@t").Run()
	exec.Command("git", "-C", root, "config", "user.name", "t").Run()
	os.WriteFile(filepath.Join(root, "a"), []byte("x"), 0o644)
	exec.Command("git", "-C", root, "add", ".").Run()
	exec.Command("git", "-C", root, "commit", "-qm", "base").Run()
	dir := filepath.Join(root, "wt")
	exec.Command("git", "-C", root, "worktree", "add", "-q", "-b", "landed", dir).Run()
	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()

	run := func(repo string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "update-ref" {
			for _, arg := range args[1:] {
				if arg == "-d" {
					return []byte("simulated protected branch"), fmt.Errorf("branch deletion refused")
				}
			}
		}
		return runReapGit(repo, args...)
	}
	err := retireLandedOne(root, reapRow{Path: dir, Branch: "landed", Head: strings.TrimSpace(string(head)), Class: "landed"}, run)
	if err == nil || !strings.Contains(err.Error(), "worktree restored") {
		t.Fatalf("branch failure must report compensated retirement, got %v", err)
	}
	if !worktreeExists(dir) {
		t.Fatal("branch deletion failure must restore the exact worktree")
	}
	if err := exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", "refs/heads/landed").Run(); err != nil {
		t.Fatal("compensation lost the branch")
	}
}

func TestRetireLandedRefusesBranchThatAdvancedAfterClassification(t *testing.T) {
	root := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0o644)
	run(root, "add", ".")
	run(root, "commit", "-qm", "base")
	dir := filepath.Join(root, "wt")
	run(root, "worktree", "add", "-q", "-b", "landed", dir)
	observed, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	os.WriteFile(filepath.Join(dir, "later"), []byte("new work"), 0o644)
	run(dir, "add", ".")
	run(dir, "commit", "-qm", "branch advanced")

	err := retireLandedOne(root, reapRow{Path: dir, Branch: "landed", Head: strings.TrimSpace(string(observed)), Class: "landed"}, runReapGit)
	if err == nil || !strings.Contains(err.Error(), "branch identity changed") {
		t.Fatalf("advanced branch must fail closed, got %v", err)
	}
	if !worktreeExists(dir) {
		t.Fatal("identity mismatch must refuse before removing the worktree")
	}
}

func TestSelectReapTargetsBoundsToExactRegisteredPaths(t *testing.T) {
	root := t.TempDir()
	entries := []worktreeEntry{
		{Path: filepath.Join(root, "one"), Branch: "landed-one"},
		{Path: filepath.Join(root, "two"), Branch: "landed-two"},
	}
	selected, err := selectReapTargets(root, entries, reapTargets{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Branch != "landed-one" {
		t.Fatalf("exact selection escaped its target: %+v", selected)
	}
	if _, err := selectReapTargets(root, entries, reapTargets{"missing"}); err == nil {
		t.Fatal("an unregistered target must fail closed")
	}
	if _, err := selectReapTargets(root, entries, reapTargets{"../two"}); err == nil {
		t.Fatal("a target escaping the repository must fail closed")
	}
}

func TestWorktreeReapTargetValidatesBeforeInspectingStatus(t *testing.T) {
	root := t.TempDir()
	selectedPath := filepath.Join(root, "one")
	otherPath := filepath.Join(root, "two")
	if err := os.Mkdir(selectedPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(otherPath, 0o755); err != nil {
		t.Fatal(err)
	}
	registrations := []worktreeEntry{
		{Path: selectedPath, IsMain: true},
		{Path: otherPath, IsMain: true},
	}
	var listed, inspected int
	var inspectedPaths []string
	originalLister, originalInspector := reapRegistrationLister, reapEntryInspector
	t.Cleanup(func() {
		reapRegistrationLister, reapEntryInspector = originalLister, originalInspector
	})
	reapRegistrationLister = func(gotRoot string) ([]worktreeEntry, error) {
		if gotRoot != root {
			t.Fatalf("registration root=%q, want %q", gotRoot, root)
		}
		listed++
		return registrations, nil
	}
	reapEntryInspector = func(entries []worktreeEntry) ([]worktreeEntry, error) {
		inspected++
		for _, entry := range entries {
			inspectedPaths = append(inspectedPaths, entry.Path)
		}
		return entries, nil
	}
	t.Setenv("HERD_ROOT", root)
	if err := runWorktreeReap([]string{"--target", "one", "--json"}); err != nil {
		t.Fatal(err)
	}
	if listed != 1 || inspected != 1 || len(inspectedPaths) != 1 || inspectedPaths[0] != selectedPath {
		t.Fatalf("target inspection counts/list=%d/%d/%v, want one selected path", listed, inspected, inspectedPaths)
	}
	if err := runWorktreeReap([]string{"--target", "missing", "--json"}); err == nil {
		t.Fatal("unregistered target must fail before status inspection")
	}
	if inspected != 1 {
		t.Fatalf("invalid target triggered status inspection: %d calls", inspected)
	}
}

func TestInspectWorktreeEntriesStatusesOnlyProvidedTargets(t *testing.T) {
	entries := []worktreeEntry{
		{Path: "/private/selected-one", Branch: "one"},
		{Path: "/private/selected-two", Branch: "two"},
	}
	original := reapStatusRunner
	t.Cleanup(func() { reapStatusRunner = original })
	var calls []string
	reapStatusRunner = func(path string, args ...string) (string, error) {
		calls = append(calls, path)
		return "", nil
	}
	inspectWorktreeEntries(entries)
	if strings.Join(calls, ",") != "/private/selected-one,/private/selected-two" {
		t.Fatalf("status calls=%v, want exactly selected entries", calls)
	}
}

func TestRetireLandedRefusesDirtyWorktreeAfterClassification(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	dir := filepath.Join(root, "wt")
	runGitT(t, root, "worktree", "add", "-q", "-b", "landed", dir)
	head := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	injected := false
	run := func(repo string, args ...string) ([]byte, error) {
		verify := false
		for _, arg := range args {
			if arg == "--verify" {
				verify = true
				break
			}
		}
		if !injected && len(args) >= 1 && args[0] == "rev-parse" && verify {
			injected = true
			if err := os.WriteFile(filepath.Join(dir, "new-evidence"), []byte("must survive"), 0644); err != nil {
				t.Fatalf("dirty fixture: %v", err)
			}
		}
		return runReapGit(repo, args...)
	}
	err := retireLandedOne(root, reapRow{Path: dir, Branch: "landed", Head: head, Class: "landed"}, run)
	if err == nil || !strings.Contains(err.Error(), "act-time worktree has") {
		t.Fatalf("dirty-after-plan must refuse before removal, got %v", err)
	}
	if !worktreeExists(dir) {
		t.Fatal("dirty worktree was removed")
	}
}

func TestRetireLandedRefusesLockedWorktreeAfterClassification(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	dir := filepath.Join(root, "wt")
	runGitT(t, root, "worktree", "add", "-q", "-b", "landed", dir)
	head := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	injected := false
	run := func(repo string, args ...string) ([]byte, error) {
		verify := false
		for _, arg := range args {
			if arg == "--verify" {
				verify = true
				break
			}
		}
		if !injected && len(args) >= 1 && args[0] == "rev-parse" && verify {
			injected = true
			if out, err := runReapGit(repo, "worktree", "lock", dir); err != nil {
				t.Fatalf("lock fixture: %v: %s", err, out)
			}
		}
		return runReapGit(repo, args...)
	}
	err := retireLandedOne(root, reapRow{Path: dir, Branch: "landed", Head: head, Class: "landed"}, run)
	if err == nil || !strings.Contains(err.Error(), "act-time worktree is locked") {
		t.Fatalf("locked-after-plan must refuse before removal, got %v", err)
	}
	if !worktreeExists(dir) {
		t.Fatal("locked worktree was removed")
	}
}

func TestRetireLandedDoesNotForceOrRetryRemoval(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	dir := filepath.Join(root, "wt")
	runGitT(t, root, "worktree", "add", "-q", "-b", "landed", dir)
	head := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	removeCalls := 0
	run := func(repo string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			removeCalls++
			for _, arg := range args {
				if arg == "--force" || arg == "-f" {
					t.Fatalf("removal must not be forced: %v", args)
				}
			}
			return []byte("synthetic removal refusal"), fmt.Errorf("synthetic removal refusal")
		}
		return runReapGit(repo, args...)
	}
	err := retireLandedOne(root, reapRow{Path: dir, Branch: "landed", Head: head, Class: "landed"}, run)
	if err == nil || removeCalls != 1 {
		t.Fatalf("removal failure must be reported after one safe attempt: err=%v calls=%d", err, removeCalls)
	}
}

type reapProcessInspectorFunc func(context.Context, string) (resources.ProcessUsage, error)

func (f reapProcessInspectorFunc) InUse(ctx context.Context, path string) (resources.ProcessUsage, error) {
	return f(ctx, path)
}

func TestRetireLandedRefusesActiveOwnerButAllowsCleanNonresident(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	dir := filepath.Join(root, "wt")
	runGitT(t, root, "worktree", "add", "-q", "-b", "landed", dir)
	head := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	row := reapRow{Path: dir, Branch: "landed", Head: head, Class: "landed"}
	active := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{CWD: true, PIDs: []int{1234}}, nil
	})
	if err := retireLandedOneWithInspector(root, row, runReapGit, active); err == nil || !strings.Contains(err.Error(), "active use") {
		t.Fatalf("active owner must block retirement: %v", err)
	}
	if !worktreeExists(dir) {
		t.Fatal("active owner refusal removed the worktree")
	}
	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
	if err := retireLandedOneWithInspector(root, row, runReapGit, clean); err != nil {
		t.Fatalf("clean nonresident worktree should retire: %v", err)
	}
}

func TestRetireLandedRefusesUnknownOwnerEvidence(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	dir := filepath.Join(root, "wt")
	runGitT(t, root, "worktree", "add", "-q", "-b", "landed", dir)
	head := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	row := reapRow{Path: dir, Branch: "landed", Head: head, Class: "landed"}
	unknown := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{MetadataUnavailable: true}, nil
	})
	err := retireLandedOneWithInspector(root, row, runReapGit, unknown)
	if err == nil || !strings.Contains(err.Error(), "census is incomplete") {
		t.Fatalf("unknown owner evidence must block: %v", err)
	}
	if !worktreeExists(dir) {
		t.Fatal("unknown owner refusal removed the worktree")
	}
}

func TestIgnoredOnlyWorktreeContentIsNotClean(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	dir := filepath.Join(root, "wt")
	runGitT(t, root, "worktree", "add", "-q", "-b", "landed", dir)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored-cache\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, dir, "add", ".gitignore")
	runGitT(t, dir, "commit", "-qm", "fixture ignore rules")
	if err := os.WriteFile(filepath.Join(dir, "ignored-cache"), []byte("evidence"), 0644); err != nil {
		t.Fatal(err)
	}
	entries, err := listWorktreeEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := exactWorktreeEntry(entries, dir)
	if !ok || !entry.Dirty {
		t.Fatalf("ignored-only content must be classified dirty: %+v", entry)
	}
}

func TestGitStatusErrorsAreReturned(t *testing.T) {
	if _, err := gitOutIn(filepath.Join(t.TempDir(), "missing"), "status", "--porcelain"); err == nil {
		t.Fatal("git status failure must not become an empty clean result")
	}
}

func TestRetireLandedRefusesStatusErrorAtActFence(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	dir := filepath.Join(filepath.Dir(root), "wt-status-error")
	runGitT(t, root, "worktree", "add", "-q", "-b", "landed", dir)
	head := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	payload := filepath.Join(dir, "must-survive")
	if err := os.WriteFile(payload, []byte("evidence"), 0644); err != nil {
		t.Fatal(err)
	}
	removeCalls := 0
	run := func(repo string, args ...string) ([]byte, error) {
		verify := false
		for _, arg := range args {
			if arg == "--verify" {
				verify = true
				break
			}
		}
		if len(args) >= 1 && args[0] == "rev-parse" && verify {
			if err := os.Remove(filepath.Join(dir, ".git")); err != nil {
				t.Fatalf("break status fixture: %v", err)
			}
		}
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			removeCalls++
			return []byte("must not reach removal"), nil
		}
		return runReapGit(repo, args...)
	}
	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
	err := retireLandedOneWithInspector(root, reapRow{Path: dir, Branch: "landed", Head: head, Class: "landed"}, run, clean)
	if err == nil || !strings.Contains(err.Error(), "status is unknown") {
		t.Fatalf("act-time status error must refuse: %v", err)
	}
	if removeCalls != 0 {
		t.Fatalf("status error reached removal: calls=%d", removeCalls)
	}
	if _, statErr := os.Stat(payload); statErr != nil {
		t.Fatalf("status refusal lost payload: %v", statErr)
	}
}

// A path that cannot be removed must be reported as FAILED, never counted as
// retired. Nothing is worse here than a silent overcount.
func TestRetireLandedReportsAFailureRatherThanClaimingSuccess(t *testing.T) {
	root := t.TempDir()
	exec.Command("git", "-C", root, "init", "-q").Run()
	retired, failed := retireLanded(root, []reapRow{
		{Path: filepath.Join(root, "does-not-exist"), Branch: "nope", Head: strings.Repeat("a", 40), Class: "landed"},
	})
	if len(retired) != 0 {
		t.Fatalf("nothing was removed, so nothing may be reported retired: %v", retired)
	}
	if len(failed) != 1 {
		t.Fatalf("a failure must be reported by exact identity: %v", failed)
	}
	if failed[0]["error"] == "" {
		t.Error("the failure must carry git's own message so it is actionable")
	}
}
