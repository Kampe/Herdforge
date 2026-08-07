package park

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitExec(t, dir, "init")
	gitExec(t, dir, "config", "user.email", "test@test")
	gitExec(t, dir, "config", "user.name", "Test")
	gitExec(t, dir, "config", "commit.gpgSign", "false")
	gitExec(t, dir, "config", "tag.gpgSign", "false")
	gitExec(t, dir, "config", "gpg.program", "/bin/false")
	gitExec(t, dir, "config", "user.signingkey", "")
	gitExec(t, dir, "checkout", "-b", "main")
	return dir
}

// initRepoWithOrigin sets up a repo with a real bare "origin" remote, so
// Audit/Hygiene (which read origin/main and origin's remote tags) work.
func initRepoWithOrigin(t *testing.T) string {
	t.Helper()
	dir := initRepo(t)
	origin := t.TempDir()
	gitExec(t, origin, "init", "--bare")
	gitExec(t, dir, "remote", "add", "origin", origin)
	return dir
}

func pushMain(t *testing.T, dir string) {
	t.Helper()
	gitExec(t, dir, "push", "origin", "main")
	gitExec(t, dir, "fetch", "origin")
}

func gitExec(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeCommit(t *testing.T, dir, file, content, msg string) string {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	gitExec(t, dir, "add", file)
	gitExec(t, dir, "-c", "user.signingkey=", "commit", "--no-gpg-sign", "-m", msg)
	return gitExec(t, dir, "rev-parse", "HEAD")
}

func shortSHA(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", ref).CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse --short %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func TestPark_ParksAndTags(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	c1 := writeCommit(t, dir, "a.txt", "a", "first")
	writeCommit(t, dir, "b.txt", "b", "second")

	t.Run("park at HEAD", func(t *testing.T) {
		res, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "my-feature", "HEAD", "park the feature")
		if !errors.Is(err, ErrPushFailed) {
			t.Fatalf("expected ErrPushFailed (no remote), got: %v", err)
		}
		if res.Tag != "parked/my-feature" {
			t.Errorf("Tag = %q, want parked/my-feature", res.Tag)
		}
		if res.ShortSHA != shortSHA(t, dir, "HEAD") {
			t.Errorf("ShortSHA = %q, want %q", res.ShortSHA, shortSHA(t, dir, "HEAD"))
		}
	})

	t.Run("park with explicit SHA", func(t *testing.T) {
		res, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "explicit", c1, "park explicit")
		if !errors.Is(err, ErrPushFailed) {
			t.Fatalf("expected ErrPushFailed (no remote), got: %v", err)
		}
		if res.Tag != "parked/explicit" {
			t.Errorf("Tag = %q", res.Tag)
		}
	})

	t.Run("error on non-commit", func(t *testing.T) {
		_, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "bad", "deadbeef", "msg")
		if !errors.Is(err, ErrNotCommit) {
			t.Fatalf("expected ErrNotCommit, got: %v", err)
		}
	})

	t.Run("error on empty message", func(t *testing.T) {
		_, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "feat", "HEAD", "  ")
		if !errors.Is(err, ErrMessageRequired) {
			t.Fatalf("expected ErrMessageRequired, got: %v", err)
		}
	})
}

func TestPark_Slugify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Hello World", "hello-world"},
		{"UPPER_CASE", "upper-case"},
		{"special chars!@#$", "special-chars"},
		{"  trim  ", "trim"},
		{"a", "a"},
	}
	for _, tc := range tests {
		got := Slugify(tc.in)
		if got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPark_PushFailure(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	writeCommit(t, dir, "x", "x", "x")

	// No remote configured -> push should fail.
	res, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "nopush", "HEAD", "will fail push")
	if !errors.Is(err, ErrPushFailed) {
		t.Fatalf("expected ErrPushFailed, got: %v", err)
	}
	if res == nil || res.Tag != "parked/nopush" {
		t.Errorf("push failure should still return the partial result, got: %+v", res)
	}
}

// TestPark_SignFallback exercises the dead-signing-agent path by stubbing
// execCommandContext so the plain (signed) `git tag` invocation fails while
// the `-c tag.gpgSign=false` fallback succeeds for real — the local repo's
// own tag.gpgSign=false config can't exercise this branch on its own, since
// it makes the FIRST attempt succeed unsigned instead of failing.
func TestPark_SignFallback(t *testing.T) {
	ctx := context.Background()
	dir := initRepoWithOrigin(t)
	writeCommit(t, dir, "base.txt", "base", "base")
	pushMain(t, dir)
	c1 := writeCommit(t, dir, "a.txt", "a", "feature work")

	orig := execCommandContext
	defer func() { execCommandContext = orig }()
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		if name == "git" && len(arg) > 0 && arg[0] == "tag" {
			return exec.CommandContext(ctx, "sh", "-c", "echo 'error: gpg failed to sign the data' >&2; echo 'fatal: failed to write tag object' >&2; exit 1")
		}
		return orig(ctx, name, arg...)
	}

	res, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: true}, "deadsigner", c1, "resume note")
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if res.Signed {
		t.Error("Signed should be false after falling back to unsigned")
	}
	if !strings.Contains(res.SignWarning, "could not SIGN") {
		t.Errorf("SignWarning missing WARN text: %q", res.SignWarning)
	}
	if !strings.Contains(res.SignWarning, "signer said:") {
		t.Errorf("SignWarning missing signer output: %q", res.SignWarning)
	}
	wantResign := "git tag -f -a -m 'resume note' parked/deadsigner " + c1 + " && git push --force origin refs/tags/parked/deadsigner"
	if !strings.Contains(res.SignWarning, wantResign) {
		t.Errorf("SignWarning missing exact re-sign command %q, got: %q", wantResign, res.SignWarning)
	}
}

// TestPark_SignFirstFalseSkipsSignedAttempt proves SignFirst is not dead:
// with it false, only the unsigned tag command runs — no signed attempt at
// all — instead of always trying signed first and discarding the option.
func TestPark_SignFirstFalseSkipsSignedAttempt(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	c1 := writeCommit(t, dir, "a.txt", "a", "work")

	orig := execCommandContext
	defer func() { execCommandContext = orig }()
	var tagInvocations [][]string
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		for _, a := range arg {
			if a == "tag" {
				tagInvocations = append(tagInvocations, append([]string(nil), arg...))
				break
			}
		}
		return orig(ctx, name, arg...)
	}

	if _, err := Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "nosign", c1, "resume note"); err != nil && !errors.Is(err, ErrPushFailed) {
		t.Fatalf("Park: %v", err)
	}

	if len(tagInvocations) != 1 {
		t.Fatalf("expected exactly 1 git tag invocation with SignFirst=false, got %d: %v", len(tagInvocations), tagInvocations)
	}
	if tagInvocations[0][0] != "-c" {
		t.Errorf("expected the single invocation to be the unsigned form (-c tag.gpgSign=false ...), got %v", tagInvocations[0])
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	c1 := writeCommit(t, dir, "a", "a", "commit subject one")
	Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "one", c1, "resume note one")
	c2 := writeCommit(t, dir, "b", "b", "commit subject two")
	Park(ctx, ParkOptions{RepoRoot: dir, SignFirst: false}, "two", c2, "resume note two")

	result, err := List(ctx, dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(result.Commits) != 2 {
		t.Fatalf("got %d commits, want 2", len(result.Commits))
	}
	for _, c := range result.Commits {
		if c.Message != "commit subject one" && c.Message != "commit subject two" {
			t.Errorf("Message = %q, want the commit subject (not the park -m resume note)", c.Message)
		}
	}
}

func TestAudit(t *testing.T) {
	ctx := context.Background()
	dir := initRepoWithOrigin(t)
	writeCommit(t, dir, "base.txt", "base", "base")
	pushMain(t, dir)

	// EXPOSED: wip(parked) commit on a branch, reachable only from that branch.
	gitExec(t, dir, "checkout", "-b", "feature-exposed")
	exposedSHA := writeCommit(t, dir, "exposed.txt", "e", "wip(parked): exposed work")

	// DURABLE: wip(parked) commit, tagged AND the tag is pushed (via Park).
	gitExec(t, dir, "checkout", "main")
	gitExec(t, dir, "checkout", "-b", "feature-durable")
	durableSHA := writeCommit(t, dir, "durable.txt", "d", "wip(parked): durable work")
	if _, err := Park(ctx, ParkOptions{RepoRoot: dir}, "durable", durableSHA, "resume note"); err != nil {
		t.Fatalf("Park durable fixture: %v", err)
	}

	// LOCAL-TAG-ONLY: wip(parked) commit, tagged locally, never pushed.
	gitExec(t, dir, "checkout", "main")
	gitExec(t, dir, "checkout", "-b", "feature-local")
	localSHA := writeCommit(t, dir, "local.txt", "l", "wip(parked): local only work")
	gitExec(t, dir, "tag", "-a", "-m", "local park", "parked/local", localSHA)

	result, err := Audit(ctx, dir)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}

	got := map[string]Durability{}
	for _, e := range result.Entries {
		got[e.SHA] = e.Durability
	}
	if got[exposedSHA] != Exposed {
		t.Errorf("exposed commit durability = %v, want Exposed", got[exposedSHA])
	}
	if got[durableSHA] != Durable {
		t.Errorf("durable commit durability = %v, want Durable", got[durableSHA])
	}
	if got[localSHA] != LocalTagOnly {
		t.Errorf("local commit durability = %v, want LocalTagOnly", got[localSHA])
	}
	if result.NotDurable != 2 {
		t.Errorf("NotDurable = %d, want 2 (exposed + local-tag-only both count)", result.NotDurable)
	}
	if VerifyAuditExit(result) {
		t.Error("VerifyAuditExit should be false when exposed/local-only parks exist")
	}
}

func TestAudit_Clean(t *testing.T) {
	ctx := context.Background()
	dir := initRepoWithOrigin(t)
	writeCommit(t, dir, "base.txt", "base", "base")
	pushMain(t, dir)

	gitExec(t, dir, "checkout", "-b", "feature-clean")
	writeCommit(t, dir, "c.txt", "c", "feat: ordinary work")

	result, err := Audit(ctx, dir)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if result.Total != 0 {
		t.Errorf("Total = %d, want 0 (no wip(parked)/wip:/parked: commits)", result.Total)
	}
	if !VerifyAuditExit(result) {
		t.Error("VerifyAuditExit should be true when nothing is parked")
	}
}

func TestHygiene(t *testing.T) {
	ctx := context.Background()
	dir := initRepoWithOrigin(t)
	writeCommit(t, dir, "base.txt", "base", "base")
	pushMain(t, dir)

	// ACTIVE: park/ branch tip whose subject never landed on main.
	gitExec(t, dir, "checkout", "-b", "park/active")
	writeCommit(t, dir, "a.txt", "a", "wip: active work")
	gitExec(t, dir, "checkout", "main")

	// CONTENT_MERGED: parked/ branch tip whose subject already landed on main.
	gitExec(t, dir, "checkout", "-b", "parked/merged")
	writeCommit(t, dir, "m.txt", "m", "base")
	gitExec(t, dir, "checkout", "main")

	// DUP: two branch tips citing the same CHA ticket.
	gitExec(t, dir, "checkout", "-b", "park/dup-one")
	writeCommit(t, dir, "d1.txt", "d1", "wip: dup work CHA-2")
	gitExec(t, dir, "checkout", "main")
	gitExec(t, dir, "checkout", "-b", "park/dup-two")
	writeCommit(t, dir, "d2.txt", "d2", "wip: dup work CHA-2 v2")
	gitExec(t, dir, "checkout", "main")

	result, err := Hygiene(ctx, dir)
	if err != nil {
		t.Fatalf("Hygiene: %v", err)
	}

	byBranch := map[string]HygieneRow{}
	for _, r := range result.Rows {
		byBranch[r.Branch] = r
	}
	if byBranch["park/active"].Flag != "ACTIVE" {
		t.Errorf("park/active flag = %q, want ACTIVE", byBranch["park/active"].Flag)
	}
	if byBranch["parked/merged"].Flag != "CONTENT_MERGED" {
		t.Errorf("parked/merged flag = %q, want CONTENT_MERGED", byBranch["parked/merged"].Flag)
	}
	if result.ContentMerged != 1 {
		t.Errorf("ContentMerged = %d, want 1", result.ContentMerged)
	}
	if result.Dup != 2 {
		t.Errorf("Dup = %d, want 2 (both tips in the CHA-2 cluster)", result.Dup)
	}
	if len(result.DupClusters) != 1 {
		t.Errorf("DupClusters = %v, want 1 cluster", result.DupClusters)
	}
	if VerifyHygieneExit(result) {
		t.Error("VerifyHygieneExit should be false when a dup cluster and a content-merged tip exist")
	}
}

func TestHygiene_Clean(t *testing.T) {
	ctx := context.Background()
	dir := initRepoWithOrigin(t)
	writeCommit(t, dir, "base.txt", "base", "base")
	pushMain(t, dir)

	gitExec(t, dir, "checkout", "-b", "park/solo")
	writeCommit(t, dir, "s.txt", "s", "wip: solo work")

	result, err := Hygiene(ctx, dir)
	if err != nil {
		t.Fatalf("Hygiene: %v", err)
	}
	if !VerifyHygieneExit(result) {
		t.Error("VerifyHygieneExit should be true for a single unmerged, uncontested branch")
	}
}

func TestReap_GCNotFound(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)

	if _, err := Reap(ctx, dir, false); !errors.Is(err, ErrGCNotFound) {
		t.Fatalf("dry-run: expected ErrGCNotFound, got: %v", err)
	}
	if _, err := Reap(ctx, dir, true); !errors.Is(err, ErrGCNotFound) {
		t.Fatalf("apply: expected ErrGCNotFound, got: %v", err)
	}
}

func TestReap_DelegatesToHerdGC(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"args: $*\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "herd-gc"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	res, err := Reap(ctx, dir, false)
	if err != nil {
		t.Fatalf("Reap dry-run: %v", err)
	}
	if res.Applied {
		t.Error("dry-run result should not be Applied")
	}
	if strings.Contains(res.Output, "--apply") {
		t.Errorf("dry-run should not pass --apply: %q", res.Output)
	}

	res, err = Reap(ctx, dir, true)
	if err != nil {
		t.Fatalf("Reap apply: %v", err)
	}
	if !res.Applied {
		t.Error("apply result should be Applied")
	}
	if !strings.Contains(res.Output, "--apply --yes") {
		t.Errorf("apply should pass --apply --yes: %q", res.Output)
	}
}
