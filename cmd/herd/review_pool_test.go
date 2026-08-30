package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

func TestPoolReviewClaudeConversationIsFreshAndExplicit(t *testing.T) {
	flags, err := bindPoolReviewConversation("claude", []string{"--model", "claude-opus-4-1"}, "123e4567-e89b-12d3-a456-426614174000")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(flags, " ") != "--model claude-opus-4-1 --session-id 123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("fresh Claude binding missing: %v", flags)
	}
	for _, stale := range [][]string{{"--continue"}, {"--resume", "old"}, {"--session-id", "old"}} {
		if _, err := bindPoolReviewConversation("claude", stale, "123e4567-e89b-12d3-a456-426614174000"); err == nil {
			t.Fatalf("stale conversation option accepted: %v", stale)
		}
	}
}

func TestMutation_PoolReviewRejectsStaleClaudeSessionAfterPacket(t *testing.T) {
	binding := herdr.PromptBinding{
		Name: "review-fac-654-candidate", TabID: "tab-1", PaneID: "pane-1",
		AgentSessionID: "123e4567-e89b-12d3-a456-426614174000", Kind: "claude",
	}
	live := herdr.AgentEntry{
		Name: binding.Name, TabID: binding.TabID, PaneID: binding.PaneID, Kind: binding.Kind,
		Session: herdr.AgentSession{Value: "stale-conversation-id"},
	}
	if err := validatePoolReviewPromptBinding(binding, live); err == nil {
		t.Fatal("mutation: removing exact session comparison accepts stale Claude conversation")
	}
	live.Session.Value = binding.AgentSessionID
	if err := validatePoolReviewPromptBinding(binding, live); err != nil {
		t.Fatalf("exact live session refused: %v", err)
	}
}

func TestPoolReviewSessionBindingJournalFailsClosed(t *testing.T) {
	root := t.TempDir()
	record := poolReviewSessionBinding{
		TaskRef: "FAC-654", CandidateSHA: strings.Repeat("a", 40), LeaseID: "pool-01-1",
		AgentName: "review-fac-654-a", TabID: "tab-1", PaneID: "pane-1", Harness: "claude",
		SessionID: "123e4567-e89b-12d3-a456-426614174000", PacketSHA256: strings.Repeat("b", 64),
	}
	if err := recordPoolReviewSession(root, record); err != nil {
		t.Fatal(err)
	}
	if err := recordPoolReviewSession(root, record); err != nil {
		t.Fatalf("exact retry must be idempotent: %v", err)
	}
	conflict := record
	conflict.CandidateSHA = strings.Repeat("c", 40)
	if err := recordPoolReviewSession(root, conflict); err == nil {
		t.Fatal("one model session accepted for two candidates")
	}
	path := filepath.Join(root, ".herd", "review-session-bindings.jsonl")
	if err := os.WriteFile(path, append(mustRead(t, path), []byte("{partial")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recordPoolReviewSession(root, record); err == nil || !errors.Is(err, errPoolReviewSessionJournal) {
		t.Fatalf("partial journal write did not fail closed: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSafeReviewSurfacePart(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "ticket", ref: "FAC-435", want: "fac-435"},
		{name: "slashes and punctuation", ref: "review/CHA-12#r2", want: "review-cha-12-r2"},
		{name: "repeated separators", ref: "review//standing---nft", want: "review-standing-nft"},
		{name: "empty", ref: "!!!", want: "candidate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeReviewSurfacePart(tt.ref); got != tt.want {
				t.Fatalf("safeReviewSurfacePart(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestReviewNamesAreStableBoundedAndDistinct(t *testing.T) {
	sha := "c3c368c84bb02ea604a2b29f112dbe231b6159a3"
	refs := []string{
		"standing/nft-data-engineer",
		"standing/nft-data-engineer-variant",
	}
	for _, ref := range refs {
		name := reviewAgentName(ref, sha)
		if len(name) > reviewAgentNameLimit || name == "" {
			t.Fatalf("reviewAgentName(%q) = %q, length %d; want 1-%d characters", ref, name, len(name), reviewAgentNameLimit)
		}
		if name != reviewAgentName(ref, sha) {
			t.Fatalf("reviewAgentName(%q) is not stable: %q", ref, name)
		}
		if name[0] == '-' || name[len(name)-1] == '-' || strings.Contains(name, "--") {
			t.Fatalf("reviewAgentName(%q) has invalid dash shape: %q", ref, name)
		}
	}
	if got := reviewAgentName(refs[0], sha); got == reviewAgentName(refs[1], sha) {
		t.Fatalf("long refs collided after truncation: %q", got)
	}
	if got := reviewTabLabel(refs[0], sha); len(got) > reviewAgentNameLimit {
		t.Fatalf("reviewTabLabel length = %d, want <= %d", len(got), reviewAgentNameLimit)
	}
}

func TestReviewPoolMode(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{args: []string{"FAC-1", "--pool"}, want: true},
		{args: []string{"--pool=true", "FAC-1"}, want: true},
		{args: []string{"FAC-1", "--spawn"}, want: false},
	} {
		if got := reviewPoolMode(tt.args); got != tt.want {
			t.Fatalf("reviewPoolMode(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestResolvePoolReviewCandidateTicketAndBranchWorktrees(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-b", "main")
	runGitT(t, root, "config", "user.email", "test@example.invalid")
	runGitT(t, root, "config", "user.name", "Herdforge test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", "README.md")
	runGitT(t, root, "commit", "-m", "fixture")

	ticketWorktree := filepath.Join(root, ".herd", "worktrees", "fac-123")
	runGitT(t, root, "worktree", "add", "--detach", ticketWorktree, "HEAD")
	if got, err := resolvePoolReviewCandidate(root, "FAC-123"); err != nil || got != ticketWorktree {
		t.Fatalf("ticket candidate = %q, %v; want %q", got, err, ticketWorktree)
	}

	runGitT(t, root, "branch", "goal/review")
	branchWorktree := filepath.Join(root, "goal-review-worktree")
	runGitT(t, root, "worktree", "add", branchWorktree, "goal/review")
	got, err := resolvePoolReviewCandidate(root, "goal/review")
	if err != nil {
		t.Fatalf("branch candidate: %v", err)
	}
	gotReal, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	wantReal, err := filepath.EvalSymlinks(branchWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if gotReal != wantReal {
		t.Fatalf("branch candidate = %q, want %q", gotReal, wantReal)
	}
}

func TestParseReviewPoolArgs(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name    string
		args    []string
		wantRef string
		wantSHA string
		wantErr bool
	}{
		{name: "selector after ref", args: []string{"FAC-478", "--pool", "--sha", sha}, wantRef: "FAC-478", wantSHA: sha},
		{name: "unknown flag", args: []string{"FAC-478", "--pool", "--not-a-review-option"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRef, gotSHA, err := parseReviewPoolArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("unknown flag was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotRef != tt.wantRef || gotSHA != tt.wantSHA {
				t.Fatalf("parseReviewPoolArgs(%v) = (%q, %q), want (%q, %q)", tt.args, gotRef, gotSHA, tt.wantRef, tt.wantSHA)
			}
		})
	}
}

// FAC-648: a DETACHED worktree sitting at the exact SHA is a legitimate candidate.
// Pool.Ensure creates slots with `git worktree add --detach` and the remote
// launcher prepares its surface the same way, but resolution accepted only a
// porcelain `branch refs/heads/<ref>` line -- so every branch-style remote review
// failed with "candidate branch is not checked out in a worktree" against a
// surface already at exactly the right commit. Requiring a branch was never the
// safety property: the pool resets --hard to the SHA regardless.
func TestResolvePoolCandidateAcceptsDetachedSurfaceAtExactSHA(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "base")
	run("branch", "feat/deep/candidate")

	shaOut, err := exec.Command("git", "-C", root, "rev-parse", "refs/heads/feat/deep/candidate").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))

	// The launcher's sanitized directory name, checked out DETACHED -- exactly
	// what the remote launcher and Pool.Ensure produce.
	surface := filepath.Join(root, ".herd", "worktrees", safeReviewSurfacePart("feat/deep/candidate"))
	run("worktree", "add", "-q", "--detach", surface, sha)

	got, err := resolvePoolReviewCandidateAt(root, "feat/deep/candidate", sha)
	if err != nil {
		t.Fatalf("a detached surface at the exact SHA must resolve: %v", err)
	}
	if got != surface {
		t.Fatalf("resolved %q, want the prepared surface %q", got, surface)
	}
}

// The SHA is VERIFIED, not assumed: a detached surface at the wrong commit must
// not be accepted just because its directory name matches.
func TestResolvePoolCandidateRejectsDetachedSurfaceAtWrongSHA(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "base")
	first, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "second")
	second, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()

	surface := filepath.Join(root, ".herd", "worktrees", safeReviewSurfacePart("feat/deep/candidate"))
	run("worktree", "add", "-q", "--detach", surface, strings.TrimSpace(string(first)))

	// Ask for the SECOND sha; the existing surface holds the first.
	//
	// FAC-678 strengthened this. Previously resolution refused outright. Now it
	// refuses to USE the wrong-SHA surface and prepares a correct one instead --
	// so the invariant to assert is the one that actually matters: whatever comes
	// back is AT the requested commit. "Must error" was a weaker restatement of
	// that, and would now fail for a strictly better outcome.
	want := strings.TrimSpace(string(second))
	got, err := resolvePoolReviewCandidateAt(root, "feat/deep/candidate", want)
	if err != nil {
		// Refusing is still acceptable; reviewing the wrong commit is not.
		return
	}
	if !headMatchesSHA(got, want) {
		t.Fatalf("resolved %s which is NOT at %s; a reviewer would read the wrong code", got, shortSHA(want))
	}
	if got == surface {
		t.Fatal("the stale surface must never be reused for a different candidate")
	}
}

// FAC-653: a sha too short to verify is a bad ARGUMENT, not a missing worktree.
// The >=12 guard is correct -- an abbreviation could ambiguously match the wrong
// commit -- but the refusal read "no worktree holds candidate at exact sha",
// which sent an operator hunting for a surface that existed with exactly the
// right HEAD. Reproduced live against .herd/worktrees/origin-repair-cha-2797-p2,
// whose HEAD was the requested commit.
func TestResolvePoolCandidateNamesAShortSHAAsTheProblem(t *testing.T) {
	root := t.TempDir()
	_, err := resolvePoolReviewCandidateAt(root, "feat/x", "326da989")
	if err == nil {
		t.Fatal("a short sha must be refused, never guessed")
	}
	if !strings.Contains(err.Error(), "too short to verify") {
		t.Errorf("the refusal must blame the argument, not an absent worktree: %v", err)
	}
	if strings.Contains(err.Error(), "no worktree holds") {
		t.Errorf("a short sha must not be reported as a missing worktree: %v", err)
	}
}

// A full-length sha with genuinely no surface still reports the worktree miss,
// so the new branch cannot swallow the real case.
func TestResolvePoolCandidateStillReportsAGenuineWorktreeMiss(t *testing.T) {
	root := t.TempDir()
	_, err := resolvePoolReviewCandidateAt(root, "feat/x", strings.Repeat("a", 40))
	if err == nil {
		t.Fatal("a full sha with no surface must still fail")
	}
	if !strings.Contains(err.Error(), "no worktree holds") {
		t.Errorf("expected the worktree-miss message: %v", err)
	}
}

// FAC-678: reported as "the wrapper demands a checked-out branch despite exact
// remote head availability". Reproduced: the branch existed locally, the SHA
// resolved, and resolution still refused because no worktree happened to hold
// it -- so every caller had to remember a manual `git worktree add --detach`,
// and two dispatches (#3340, #3339) were rejected for forgetting a step the
// command can do itself.
func TestCandidateSurfaceIsPreparedWhenTheSHAResolves(t *testing.T) {
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
	shaOut, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	sha := strings.TrimSpace(string(shaOut))

	dir, err := prepareCandidateSurface(root, "feat/thing", sha)
	if err != nil {
		t.Fatalf("preparing a surface for a resolvable sha must succeed: %v", err)
	}
	if dir == "" {
		t.Fatal("a resolvable sha must yield a surface")
	}
	if !headMatchesSHA(dir, sha) {
		t.Error("the prepared surface must be AT the candidate")
	}
	// Detached on purpose: checking out the branch would move a ref someone else
	// may be working on. A detached surface at an exact SHA is inert.
	if exec.Command("git", "-C", dir, "symbolic-ref", "-q", "HEAD").Run() == nil {
		t.Error("the surface must be DETACHED so no branch ref is moved")
	}
}

// A sha that is not a commit here is not ours to prepare, and must fall through
// to the normal refusal rather than reporting a preparation failure for a
// candidate that never existed.
func TestNoSurfaceIsPreparedForAnUnresolvableSHA(t *testing.T) {
	root := t.TempDir()
	exec.Command("git", "-C", root, "init", "-q").Run()
	dir, err := prepareCandidateSurface(root, "feat/thing", strings.Repeat("0", 40))
	if err != nil {
		t.Errorf("an unresolvable sha is not an error, it is not ours: %v", err)
	}
	if dir != "" {
		t.Fatal("nothing may be created for a sha that does not exist here")
	}
	// A short sha cannot be verified and must not be prepared either.
	if d, _ := prepareCandidateSurface(root, "feat/thing", "abc123"); d != "" {
		t.Fatal("a sha too short to verify must never create a surface")
	}
}

// FAC-682: a reviewer proving a test non-vacuous swaps a file for its
// parent-commit blob and restores it afterwards. 132 verdicts in the live corpus
// do exactly that, and it is good practice. Done in the WRONG directory it
// rewrites canonical shared main -- which happened on 2026-08-26: the shared
// index and working tree had one path replaced with the candidate's parent blob,
// HEAD unchanged, no MERGE_HEAD, and a coordinator restored it by hand.
func TestDirtySharedCheckoutIsDetected(t *testing.T) {
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
	os.WriteFile(filepath.Join(root, "routes.ts"), []byte("current\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "base")

	t.Setenv("HERD_ALLOW_DIRTY_SHARED_CHECKOUT", "")
	if got := sharedCheckoutDirtyPaths(root); len(got) != 0 {
		t.Fatalf("a clean checkout must report nothing, got %v", got)
	}

	// The exact reported shape: one path replaced, nothing else touched.
	os.WriteFile(filepath.Join(root, "routes.ts"), []byte("parent blob\n"), 0o644)
	got := sharedCheckoutDirtyPaths(root)
	if len(got) == 0 {
		t.Fatal("a modified shared checkout must be detected; this is what went unnoticed until a hand repair")
	}
	if !strings.Contains(strings.Join(got, "\n"), "routes.ts") {
		t.Errorf("the dirty path must be named so it is actionable: %v", got)
	}
}

// An operator who declares the state intentional must be able to proceed. A gate
// that cannot be satisfied gets bypassed wholesale rather than understood.
func TestIntentionalDirtyStateCanBeDeclared(t *testing.T) {
	root := t.TempDir()
	exec.Command("git", "-C", root, "init", "-q").Run()
	os.WriteFile(filepath.Join(root, "x.txt"), []byte("y"), 0o644)
	t.Setenv("HERD_ALLOW_DIRTY_SHARED_CHECKOUT", "1")
	if got := sharedCheckoutDirtyPaths(root); len(got) != 0 {
		t.Fatalf("a declared-intentional state must not block, got %v", got)
	}
}

// An unreadable status is an unanswerable question and must not block a launch.
func TestUnreadableStatusDoesNotBlockLaunch(t *testing.T) {
	t.Setenv("HERD_ALLOW_DIRTY_SHARED_CHECKOUT", "")
	if got := sharedCheckoutDirtyPaths(filepath.Join(t.TempDir(), "not-a-repo")); len(got) != 0 {
		t.Fatalf("an unreadable status must not manufacture a blocker, got %v", got)
	}
}
