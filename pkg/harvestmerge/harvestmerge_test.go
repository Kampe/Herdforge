package harvestmerge

import (
	"strings"
	"testing"
)

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
	if found := ConflictMarkers("+Heading\n+=======\n"); len(found) != 1 {
		t.Fatalf("a bare ======= separator must be caught, got %v", found)
	}
	// A markdown setext underline is sized to its heading, so a longer run is
	// documentation rather than a separator.
	if found := ConflictMarkers("+Heading\n+===========\n"); len(found) != 0 {
		t.Fatalf("a longer underline is documentation, got %v", found)
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

func TestValidateRefusesUnmergeableHarvests(t *testing.T) {
	base := Plan{Lane: "smith", Title: "feat: x", Commits: []string{"aaa"}, Verdict: PASS}
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
