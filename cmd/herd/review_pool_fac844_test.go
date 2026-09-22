package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/dispatch"
)

// FAC-844: review candidate preparation with a FAC-number selector used to
// ignore the exclusive attached author worktree and `git worktree add --detach`
// a blank carrier. TASK-CONTEXT.json is gitignored, so the new surface had no
// authenticated SHA/base/lease identity.

func fac844AuthorHome(t *testing.T) (root, author, base, sha string) {
	t.Helper()
	root = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main", ".")
	git("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("TASK-CONTEXT.json\n.herd/worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	signer := fixtureSigner(t, keyDir, root)
	git("add", ".")
	git("add", "-f", ".herd/receipt.pub")
	git("commit", "-qm", "base")
	base = git("rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "b.txt")
	git("commit", "-qm", "candidate")
	sha = git("rev-parse", "HEAD")

	author = filepath.Join(filepath.Dir(root), "author-"+filepath.Base(root))
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "-b", "fix/fac-844-author", author, sha).CombinedOutput(); err != nil {
		t.Fatalf("author worktree: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", root, "worktree", "remove", "--force", author).Run()
		_ = os.RemoveAll(author)
	})
	git("reset", "--hard", base)

	tc := dispatch.TaskContext{
		ProviderType:    "kaneo",
		ProjectID:       "proj-x",
		Repository:      dispatch.RepositoryIdentityOrName(root, "herdforge-test"),
		Role:            dispatch.RoleWorker,
		TaskRef:         "FAC-844",
		TaskID:          "ie7vkjphot5xqniqpfhihpa3",
		Branch:          "fix/fac-844-author",
		BaseSHA:         base,
		CandidateSHA:    sha,
		LeaseID:         "lease-fac-844",
		LeaseGeneration: 3,
		LeaseTaskRef:    "FAC-844",
		SessionID:       "worker-fac-844",
		AllowedOps:      dispatch.WorkerOps,
		ExpiresAt:       time.Now().Add(time.Hour),
	}
	signed, err := signer.Issue(tc)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(author, signed); err != nil {
		t.Fatal(err)
	}
	return root, author, base, sha
}

func TestFAC844FACNumberRefReusesAttachedAuthorHome(t *testing.T) {
	root, author, _, sha := fac844AuthorHome(t)
	before, err := os.ReadFile(filepath.Join(author, dispatch.TaskContextFile))
	if err != nil {
		t.Fatal(err)
	}

	dir, err := resolvePoolReviewCandidateAtFor(root, "FAC-844", sha, true)
	if err != nil {
		t.Fatalf("FAC-number resolution must reuse the authenticated author home: %v", err)
	}
	if !sameDir(dir, author) {
		t.Fatalf("resolved %q, want attached author home %q", dir, author)
	}
	if got := managedWorktrees(t, root); len(got) != 0 {
		t.Fatalf("prepared a blank carrier over an authenticated home: %v", got)
	}
	after, err := os.ReadFile(filepath.Join(author, dispatch.TaskContextFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("review preparation mutated the existing TASK-CONTEXT")
	}
}

func TestFAC844AuthenticatedBaseIsUsedWithoutExplicitPin(t *testing.T) {
	root, author, base, sha := fac844AuthorHome(t)
	got, err := resolveReviewBase(root, author, "FAC-844", sha, "")
	if err != nil {
		t.Fatalf("authenticated base must resolve without --base: %v", err)
	}
	if got != base {
		t.Fatalf("resolved base %q, want authenticated %q", got, base)
	}
}

func TestFAC844TamperedContextRefusesReplacementSurface(t *testing.T) {
	root, author, _, sha := fac844AuthorHome(t)
	path := filepath.Join(author, dispatch.TaskContextFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte{}, raw...)
	if idx := strings.Index(string(tampered), "lease-fac-844"); idx >= 0 {
		tampered[idx] = 'X'
	} else {
		tampered[len(tampered)/2] ^= 0xff
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := resolvePoolReviewCandidateAtFor(root, "FAC-844", sha, true); err == nil {
		t.Fatal("an unverified TASK-CONTEXT must refuse rather than prepare a replacement surface")
	}
	if got := managedWorktrees(t, root); len(got) != 0 {
		t.Fatalf("unverified context left a replacement carrier: %v", got)
	}
}

func TestFAC844PoolNoLaunchWithoutBaseReusesAuthorHome(t *testing.T) {
	root, sha, _ := entryFixture(t)
	entryConfig(t, root)
	base := strings.TrimSpace(entryGitOutput(t, root, "rev-parse", sha+"^"))
	author := filepath.Join(filepath.Dir(root), "author-"+filepath.Base(root))
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "-b", "fix/fac-9320-author", author, sha).CombinedOutput(); err != nil {
		t.Fatalf("author worktree: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", root, "worktree", "remove", "--force", author).Run()
		_ = os.RemoveAll(author)
	})
	keyDir := t.TempDir()
	signer := fixtureSigner(t, keyDir, root)
	signed, err := signer.Issue(dispatch.TaskContext{
		ProviderType:    "memory",
		ProjectID:       entryProjectID,
		Repository:      dispatch.RepositoryIdentityOrName(root, "fixture"),
		Role:            dispatch.RoleWorker,
		TaskRef:         entryRef,
		TaskID:          "fixture-1",
		Branch:          "fix/fac-9320-author",
		BaseSHA:         base,
		CandidateSHA:    sha,
		LeaseID:         "lease-fac-9320",
		LeaseGeneration: 1,
		LeaseTaskRef:    entryRef,
		SessionID:       "worker-fac-9320",
		AllowedOps:      dispatch.WorkerOps,
		ExpiresAt:       time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(author, signed); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(author, dispatch.TaskContextFile))
	if err != nil {
		t.Fatal(err)
	}
	// The fixture mints receipt.pub after the shared checkout is already
	// clean. That file is verification material for the existing home, not
	// candidate source; the dirty guard would otherwise refuse before the
	// identity path runs.
	t.Setenv("HERD_ALLOW_DIRTY_SHARED_CHECKOUT", "1")

	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	os.Args = []string{"herd", "review", entryRef, "--pool", "--no-launch", "--sha", sha, "--builder-family", "anthropic"}
	if err := runPoolReview(entryRef); err != nil {
		t.Fatalf("native pool review without --base must reuse the authenticated FAC-number home: %v", err)
	}
	if got := entryCarriers(t, root); len(got) != 0 {
		t.Fatalf("native FAC-number review prepared a blank carrier: %v", got)
	}
	after, err := os.ReadFile(filepath.Join(author, dispatch.TaskContextFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("native review reissued or copied TASK-CONTEXT")
	}
}

func TestFAC844MissingHomeStillPreparesBlankCarrier(t *testing.T) {
	root, sha := carrierRepo(t)
	dir, err := resolvePoolReviewCandidateAtFor(root, "FAC-844", sha, true)
	if err != nil {
		t.Fatalf("no authenticated home must still prepare: %v", err)
	}
	if dir == "" {
		t.Fatal("expected a prepared carrier when no TASK-CONTEXT home exists")
	}
	if _, err := os.Stat(filepath.Join(dir, dispatch.TaskContextFile)); !os.IsNotExist(err) {
		t.Fatalf("blank carrier must not invent TASK-CONTEXT, stat err=%v", err)
	}
}
