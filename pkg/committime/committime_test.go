package committime

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The rule moved here, so its proof moves here too. A package that owns a
// decision and pins none of it is where the next divergence starts.

func git(t *testing.T, dir string, env []string, args ...string) string {
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

// THE rule. An amended commit keeps its old AUTHOR time and gets a new
// COMMITTER time. Reading the former made a commit look older than the launch
// that produced it, so that lane's receipt was rejected as "recorded after the
// commit" -- and a rejected receipt left the reviewer's asserted family
// unchecked.
func TestAnAmendedCommitReportsItsNewCommitterTime(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q", "-b", "main")
	git(t, dir, []string{"GIT_AUTHOR_DATE=2026-08-27T20:00:00+00:00", "GIT_COMMITTER_DATE=2026-08-27T20:00:00+00:00"},
		"commit", "-q", "--allow-empty", "-m", "original")
	git(t, dir, []string{"GIT_COMMITTER_DATE=2026-08-27T22:00:00+00:00"},
		"commit", "-q", "--amend", "--no-edit", "--allow-empty")
	sha := git(t, dir, nil, "rev-parse", "HEAD")

	// The fixture is only meaningful if git really kept the two stamps apart.
	if author := git(t, dir, nil, "show", "-s", "--format=%aI", sha); !strings.HasPrefix(author, "2026-08-27T20:00") {
		t.Fatalf("fixture invalid: author time %q, expected the ORIGINAL 20:00", author)
	}

	got := Of(dir, sha)
	want := time.Date(2026, 8, 27, 22, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Of = %s, want %s (the COMMITTER time). Reading author time makes an "+
			"amended or cherry-picked commit predate the launch that produced it.",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// An unanswerable question is not a time. Every caller reads zero as "no
// provenance", so a wrong non-zero here becomes a confident wrong attribution.
func TestUnanswerableQuestionsYieldTheZeroTime(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q", "-b", "main")

	for name, got := range map[string]time.Time{
		"unknown sha":  Of(dir, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		"blank sha":    Of(dir, "   "),
		"missing repo": Of(t.TempDir()+"/nope", "HEAD"),
	} {
		if !got.IsZero() {
			t.Fatalf("%s resolved a creation time: %s", name, got)
		}
	}
}

// The root argument is what pkg/candidateindex needs: it asks about worktrees
// other than the process's own directory.
func TestOfAnswersForAnExplicitRoot(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q", "-b", "main")
	git(t, dir, []string{"GIT_COMMITTER_DATE=2026-08-27T21:30:00+00:00"},
		"commit", "-q", "--allow-empty", "-m", "c")
	sha := git(t, dir, nil, "rev-parse", "HEAD")

	// Deliberately not chdir'd into dir: the root argument must carry it.
	if got := Of(dir, sha); !got.Equal(time.Date(2026, 8, 27, 21, 30, 0, 0, time.UTC)) {
		t.Fatalf("Of(root, sha) = %s; the explicit root was not used", got)
	}
}
