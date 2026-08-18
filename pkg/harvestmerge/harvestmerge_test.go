package harvestmerge

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUnionMergeAppendsBothLanes(t *testing.T) {
	base := "# Plan\n\nExisting\n"
	got, err := UnionMerge(base, base+"Lane A\n", base+"Lane B\n")
	if err != nil {
		t.Fatalf("union merge: %v", err)
	}
	if got != base+"Lane A\nLane B\n" {
		t.Fatalf("merged content = %q", got)
	}
}

func TestUnionMergeRejectsChangedBase(t *testing.T) {
	_, err := UnionMerge("base\n", "changed\n", "base\nlane\n")
	if err == nil {
		t.Fatal("a non-append edit must refuse union merge")
	}
}

func TestUnionMergePathsAreRepoRelativeAndExact(t *testing.T) {
	cfg := UnionMergeConfig{Paths: []string{"docs/MASTER-TEST-PLAN.md"}}
	if !cfg.Enabled("docs/MASTER-TEST-PLAN.md") {
		t.Fatal("configured path must be enabled")
	}
	for _, path := range []string{"./docs/MASTER-TEST-PLAN.md", "../docs/MASTER-TEST-PLAN.md", "/tmp/MASTER-TEST-PLAN.md", "docs/other.md"} {
		if cfg.Enabled(path) {
			t.Fatalf("path %q must not match the configured repo-relative path", path)
		}
	}
}

func TestMatrixIDAllocatorIsUniqueConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matrix-ids")
	const n = 32
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			id, err := AllocateMatrixID(path, "U")
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatal(err)
		case id := <-ids:
			if seen[id] {
				t.Fatalf("duplicate matrix ID %q", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != n {
		t.Fatalf("allocated %d unique IDs, want %d", len(seen), n)
	}
	for id := range seen {
		if !strings.HasPrefix(id, "U-") {
			t.Fatalf("ID %q has wrong prefix", id)
		}
	}
}

// Anything that is not an explicit PASS is not consent to merge.
func TestOnlyExplicitPassAllowsMerge(t *testing.T) {
	if !MergeAllowed(PASS) {
		t.Fatal("PASS must allow merge")
	}
	for _, v := range []Verdict{FAIL, BLOCKED, "", "pass", "LGTM", "APPROVED"} {
		if MergeAllowed(v) {
			t.Fatalf("verdict %q must not allow merge", v)
		}
	}
}

func TestRetiredIsTerminalButNeverMergeConsent(t *testing.T) {
	if !Terminal(RETIRED) {
		t.Fatal("RETIRED must settle the branch for audit/drain projections")
	}
	if MergeAllowed(RETIRED) {
		t.Fatal("RETIRED must never authorize a merge")
	}
}

// A cherry-pick that leaves markers behind produces a structurally broken diff
// that nobody re-reads once it is in a PR.
func TestConflictMarkersInAddedLinesAbort(t *testing.T) {
	diff := strings.Join([]string{
		"+++ b/pkg/router/launch.go",
		"+<<<<<<< HEAD",
		"+	return WorkerShape",
		"+=======",
		"+	return \"implementation\"",
		"+>>>>>>> theirs",
	}, "\n")
	// Three: the head marker, the bare separator, and the tail marker. The
	// separator counts — an earlier version excluded it and let half-resolved
	// conflicts through.
	found := ConflictMarkers(diff)
	if len(found) != 3 {
		t.Fatalf("head, separator and tail must all be caught, got %v", found)
	}
}

// A pre-existing marker in a context line is not this harvest's fault.
func TestUnrelatedMarkersInContextDoNotBlock(t *testing.T) {
	diff := strings.Join([]string{
		" <<<<<<< this is context, not added",
		"-<<<<<<< this line is being REMOVED",
		"+clean added line",
	}, "\n")
	if found := ConflictMarkers(diff); len(found) != 0 {
		t.Fatalf("only ADDED lines count, got %v", found)
	}
}

// A bare seven-'=' separator IS a conflict marker — the reference catches it,
// and a half-resolved conflict where the head/tail markers were stripped but
// the separator survived is a real and common shape.
func TestBareSeparatorIsAConflictMarker(t *testing.T) {
	if found := ConflictMarkers("+ours\n+=======\n"); len(found) != 1 {
		t.Fatalf("a bare ======= separator must be caught, got %v", found)
	}
	// conflict-marker-size=8 emits an 8-char separator; exact-7 missed it.
	if found := ConflictMarkers("+ours\n+========\n"); len(found) != 1 {
		t.Fatalf("an 8-char separator must be caught, got %v", found)
	}
}

// CRLF must not defeat the separator: "=======\r" does not match an unanchored
// `$`, so line endings alone used to slip a half-resolved conflict through.
func TestCRLFDoesNotDefeatTheSeparator(t *testing.T) {
	if found := ConflictMarkers("+ours\r\n+=======\r\n"); len(found) != 1 {
		t.Fatalf("a CRLF separator must be caught, got %v", found)
	}
}

// git emits longer markers under .gitattributes conflict-marker-size=8.
func TestLongerThanSevenMarkersAreCaught(t *testing.T) {
	diff := "+<<<<<<<< HEAD\n+ours\n+>>>>>>>> theirs\n"
	if found := ConflictMarkers(diff); len(found) != 2 {
		t.Fatalf("8-char markers must be caught, got %v", found)
	}
}

// git cherry is patch-based: content already upstream under a different SHA is
// marked '-' and must be skipped, or the pick conflicts or duplicates.
func TestUniqueCommitsSkipsAlreadyUpstreamPatches(t *testing.T) {
	got := UniqueCommits("+ aaa111\n- bbb222\n+ ccc333\n")
	if len(got) != 2 || got[0] != "aaa111" || got[1] != "ccc333" {
		t.Fatalf("unique commits = %v", got)
	}
	if n := len(UniqueCommits("- aaa111\n- bbb222\n")); n != 0 {
		t.Fatalf("a fully-merged branch must yield nothing, got %d", n)
	}
	if n := len(UniqueCommits("")); n != 0 {
		t.Fatalf("empty cherry output must yield nothing, got %d", n)
	}
}

func TestParseCandidateRange(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  CandidateRange
		bad   bool
	}{
		{name: "exact range", input: "origin/main..abc123", want: CandidateRange{Base: "origin/main", SHA: "abc123"}},
		{name: "trim outer whitespace", input: "  base..sha  ", want: CandidateRange{Base: "base", SHA: "sha"}},
		{name: "missing base", input: "..sha", bad: true},
		{name: "missing candidate", input: "base..", bad: true},
		{name: "symmetric range", input: "base...sha", bad: true},
		{name: "embedded whitespace", input: "base..sha value", bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCandidateRange(tt.input)
			if tt.bad {
				if err == nil {
					t.Fatalf("ParseCandidateRange(%q) unexpectedly succeeded: %+v", tt.input, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseCandidateRange(%q) = %+v, %v; want %+v", tt.input, got, err, tt.want)
			}
		})
	}
}

func TestValidateRefusesUnmergeableHarvests(t *testing.T) {
	base := Plan{Lane: "smith", Title: "feat: x", Commits: []string{"aaa"}, Verdict: PASS, Diffstat: " 2 files changed, 10 insertions(+)"}
	if err := base.Validate(); err != nil {
		t.Fatalf("a well-formed plan must validate: %v", err)
	}
	noTitle := base
	noTitle.Title = ""
	if err := noTitle.Validate(); err == nil {
		t.Fatal("an unlabelled PR is unreviewable and must be refused")
	}
	noCommits := base
	noCommits.Commits = nil
	if err := noCommits.Validate(); err == nil {
		t.Fatal("nothing to harvest must be refused, not merged empty")
	}
	// Absent is not consent: omitting --verdict must refuse, not sail through.
	noVerdict := base
	noVerdict.Verdict = ""
	if err := noVerdict.Validate(); err == nil {
		t.Fatal("an absent verdict must refuse the merge")
	}
	failed := base
	failed.Verdict = FAIL
	if err := failed.Validate(); err == nil {
		t.Fatal("a FAIL verdict must refuse the merge")
	}
	blocked := base
	blocked.Verdict = BLOCKED
	if err := blocked.Validate(); err == nil {
		t.Fatal("a BLOCKED verdict must refuse the merge")
	}
	passed := base
	passed.Verdict = PASS
	if err := passed.Validate(); err != nil {
		t.Fatalf("PASS must proceed: %v", err)
	}
}

func TestTempBranchNameIsDeterministicAndRefSafe(t *testing.T) {
	a := TempBranchName("forge/smith lane", "0123456789abcdef")
	b := TempBranchName("forge/smith lane", "0123456789abcdef")
	if a != b {
		t.Fatalf("retry must reuse the same branch: %q vs %q", a, b)
	}
	if strings.ContainsAny(strings.TrimPrefix(a, "harvest/"), "/ ") {
		t.Fatalf("branch name must be ref-safe: %q", a)
	}
}

// An empty diff is not a completed ticket.
//
// PR #151 merged 0 additions, 0 deletions, 0 files. The branch carried only its
// anchor commit, so len(Commits) was 1 and `git cherry` marked it '+' because no
// patch-equivalent existed upstream. The reviewer returned PASS -- an empty diff
// has nothing wrong with it -- and the card was nearly closed as done.
//
// Non-vacuity: `base` differs from `emptyDiff` ONLY in Diffstat, and base is
// asserted to validate immediately above, so this cannot pass by accident.
func TestValidateRefusesABranchThatChangesNoBytes(t *testing.T) {
	base := Plan{Lane: "smith", Branch: "herd/fac-156", Title: "feat: x", Commits: []string{"aaa"}, Verdict: PASS, Diffstat: " 2 files changed, 10 insertions(+)"}
	if err := base.Validate(); err != nil {
		t.Fatalf("a plan with content must validate: %v", err)
	}
	for name, stat := range map[string]string{"absent": "", "whitespace": "   \n"} {
		t.Run(name, func(t *testing.T) {
			empty := base
			empty.Diffstat = stat
			err := empty.Validate()
			if err == nil {
				t.Fatal("a branch that changes no bytes must be refused")
			}
			if !strings.Contains(err.Error(), "changes no bytes") {
				t.Fatalf("error must name the reason, got: %v", err)
			}
		})
	}
}
