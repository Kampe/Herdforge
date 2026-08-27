package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Review finding on 827601fadb70, verified and fixed.
//
// ingestTaskIdentityFor collapses an invalid standing/* or wt/* ref to "" via
// CloseableCardRef, so RequireCloseableCardRef received an already-empty string
// and always took its "task is empty" branch. The message that NAMES the
// offending value was unreachable from the shipped path -- which is the entire
// operator diagnostic FAC-578 exists to provide.
//
// The pre-existing bad-value tests call RequireCloseableCardRef DIRECTLY with
// the raw ref, so they passed while the shipped path could not produce that
// message. They were vacuous for the behaviour under test: exercising the
// validator instead of the path only proves the validator works, which was
// never in doubt.
//
// This runs the real binary, because runReviewIngest exits the process and an
// in-process call cannot observe what an operator actually sees.
func TestReviewIngestRefusalNamesTheOffendingBranchValue(t *testing.T) {
	binary := buildHerd(t)
	repo := t.TempDir()

	// A real candidate with a real, non-empty diff against origin/main. Earlier
	// gates refuse an unresolvable sha and an empty diff, and either would hide
	// the gate actually under test.
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "candidate.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "candidate.txt")
	git("commit", "-q", "-m", "candidate")
	shaOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))

	inbox := filepath.Join(repo, ".herd", "review", "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	// A verdict whose task is a lane branch rather than a card ref: the exact
	// shape FAC-578 refuses.
	const offending = "standing/docs-custodian"
	artifact := "sha-review-badtask.md"
	body := "sha: " + sha + "\n" +
		"branch: " + offending + "\n" +
		"task: " + offending + "\n" +
		"reviewer: review-badtask\n" +
		"reviewer-family: anthropic\n" +
		"builder-family: openai\n" +
		"verdict: PASS\n" +
		"reviewed-head: " + sha + "\n---\n" +
		strings.Repeat("Body long enough to clear the minimum-length gate. ", 12) + "\n"
	if err := os.WriteFile(filepath.Join(inbox, artifact), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "review-ingest", "--sweep", "--dry-run")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "HERD_ROOT="+repo, "HERD_REPO_ROOT="+repo)
	out, _ := cmd.CombinedOutput()
	text := string(out)

	if !strings.Contains(text, offending) {
		t.Fatalf("refusal does not name the offending value %q, so an operator cannot see WHICH ref was wrong.\n"+
			"This is the diagnostic FAC-578 exists to provide.\nGot:\n%s", offending, text)
	}
	if strings.Contains(text, "task is empty") {
		t.Fatalf("refusal reported the COLLAPSED value as empty rather than the raw ref; the offending value was lost.\nGot:\n%s", text)
	}
}
