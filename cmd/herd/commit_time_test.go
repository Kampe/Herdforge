package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// FAC-620, fourth review. The temporal guard compared receipts against the
// AUTHOR timestamp. `git commit --amend` and `git cherry-pick` both PRESERVE
// the original author time and stamp a NEW committer time, so a lane launched
// at 21:00 that amends a commit carrying a 20:00 author time had its own
// receipt rejected as "recorded after the commit".
//
// That was not a harmless over-rejection. A rejected receipt lands on
// no-provenance, and no-provenance used to leave the reviewer's asserted family
// unchecked -- so the guard reopened the hole it existed to close, for two of
// the most ordinary git workflows there are.
//
// These drive commitCreationTime against a REAL repository. A unit test over a
// hand-built timestamp string would pass on either format and prove nothing;
// the whole point is what git actually does to the two stamps.

func gitStamped(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"), env...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// THE regression. An amended commit keeps its old author time and gets a new
// committer time; the launch that produced it started in between.
func TestAnAmendedCommitIsDatedByItsCreationNotItsAuthorship(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gitStamped(t, dir, nil, "init", "-q", "-b", "wt/x")

	authored := "2026-08-27T20:00:00+00:00"
	amended := "2026-08-27T22:00:00+00:00"

	gitStamped(t, dir, []string{"GIT_AUTHOR_DATE=" + authored, "GIT_COMMITTER_DATE=" + authored},
		"commit", "-q", "--allow-empty", "-m", "original")
	// The amend: author date preserved by git itself, committer date moves.
	gitStamped(t, dir, []string{"GIT_COMMITTER_DATE=" + amended},
		"commit", "-q", "--amend", "--no-edit", "--allow-empty")

	sha := gitStamped(t, dir, nil, "rev-parse", "HEAD")

	// Sanity: this test is worthless unless git really did keep the two apart.
	if got := gitStamped(t, dir, nil, "show", "-s", "--format=%aI", sha); !strings.HasPrefix(got, "2026-08-27T20:00") {
		t.Fatalf("fixture invalid: author time is %q, expected the ORIGINAL 20:00", got)
	}

	got := commitCreationTime(sha)
	if got.IsZero() {
		t.Fatal("no creation time resolved for an amended commit")
	}

	// The launch that produced this commit started at 21:00 -- AFTER the
	// retained author time, BEFORE the amend. It must still qualify.
	launchedAt := time.Date(2026, 8, 27, 21, 0, 0, 0, time.UTC)
	if launchedAt.After(got) {
		t.Fatalf("creation time %s is before the launch that produced the commit (%s); "+
			"the guard is reading AUTHOR time, so an amend or cherry-pick rejects the "+
			"receipt of the lane that actually wrote the code -- and a rejected receipt "+
			"leaves the reviewer's asserted family unchecked", got.Format(time.RFC3339), launchedAt.Format(time.RFC3339))
	}
}

// Cherry-pick has the same shape: author time travels with the commit, the
// new commit object is created now.
func TestACherryPickedCommitIsDatedByItsCreation(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gitStamped(t, dir, nil, "init", "-q", "-b", "main")

	old := "2026-08-27T20:00:00+00:00"
	gitStamped(t, dir, []string{"GIT_AUTHOR_DATE=" + old, "GIT_COMMITTER_DATE=" + old},
		"commit", "-q", "--allow-empty", "-m", "base")
	gitStamped(t, dir, []string{"GIT_AUTHOR_DATE=" + old, "GIT_COMMITTER_DATE=" + old},
		"commit", "-q", "--allow-empty", "-m", "pick-me")
	pick := gitStamped(t, dir, nil, "rev-parse", "HEAD")

	gitStamped(t, dir, nil, "checkout", "-q", "-b", "wt/harvest", "HEAD~1")
	gitStamped(t, dir, []string{"GIT_COMMITTER_DATE=2026-08-27T22:00:00+00:00"},
		"cherry-pick", "--allow-empty", pick)

	sha := gitStamped(t, dir, nil, "rev-parse", "HEAD")
	got := commitCreationTime(sha)
	if got.IsZero() {
		t.Fatal("no creation time resolved for a cherry-picked commit")
	}
	launchedAt := time.Date(2026, 8, 27, 21, 0, 0, 0, time.UTC)
	if launchedAt.After(got) {
		t.Fatalf("creation time %s predates the harvesting launch at %s; a harvested "+
			"commit would attribute to nothing", got.Format(time.RFC3339), launchedAt.Format(time.RFC3339))
	}
}

// An unknown SHA is not a time. Zero means "no provenance", never "attribute
// on reachability alone".
func TestAnUnknownSHAHasNoCreationTime(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gitStamped(t, dir, nil, "init", "-q", "-b", "main")

	if got := commitCreationTime("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); !got.IsZero() {
		t.Fatalf("an unknown SHA resolved a creation time: %s", got)
	}
	if got := commitCreationTime("   "); !got.IsZero() {
		t.Fatalf("a blank SHA resolved a creation time: %s", got)
	}
}
