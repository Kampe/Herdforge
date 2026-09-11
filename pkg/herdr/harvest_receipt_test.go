package herdr

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func harvestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// harvestRepo builds a repository with one commit on main.
func harvestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		harvestGit(t, root, args...)
	}
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	harvestGit(t, root, "add", "seed.txt")
	harvestGit(t, root, "commit", "-qm", "seed")
	return root
}

// addHarvestWorktree registers a staging worktree the way the producer does.
func addHarvestWorktree(t *testing.T, root, rel, branch, base string) string {
	t.Helper()
	abs := filepath.Join(root, rel)
	harvestGit(t, root, "worktree", "add", "-B", branch, "--", abs, base)
	return abs
}

// recordHarvestReceipt mints the marker and journals a bound receipt, exactly
// as the producer's success path does.
func recordHarvestReceipt(t *testing.T, root, rel, abs, branch, repoIdentity string) HarvestRetirementReceipt {
	t.Helper()
	marker, registrationID, err := MintHarvestGenerationMarker(abs, rel, time.Now())
	if err != nil {
		t.Fatalf("mint marker: %v", err)
	}
	receipt := NewHarvestRetirementReceipt(time.Now(), HarvestRetirementReceipt{
		Repository:     repoIdentity,
		Worktree:       rel,
		TempBranch:     branch,
		Lane:           "lane",
		BaseSHA:        harvestGit(t, root, "rev-parse", "HEAD"),
		CandidateSHA:   harvestGit(t, abs, "rev-parse", "HEAD"),
		HeadSHA:        harvestGit(t, abs, "rev-parse", "HEAD"),
		RegistrationID: registrationID,
		Generation:     marker.Generation,
	})
	if err := (HarvestRetirementRegistry{Path: HarvestRetirementReceiptsPath(root)}).Record(receipt); err != nil {
		t.Fatalf("record receipt: %v", err)
	}
	return receipt
}

const harvestTestIdentity = "github.com/Kampe/Herdforge"

func TestAuthorizeHarvestRetirement_AuthorizesOwnedRegistration(t *testing.T) {
	root := harvestRepo(t)
	rel := filepath.Join(".herd", "worktrees", "harvest-lane-abc")
	abs := addHarvestWorktree(t, root, rel, "harvest/lane-abc", "HEAD")
	want := recordHarvestReceipt(t, root, rel, abs, "harvest/lane-abc", harvestTestIdentity)

	got, err := AuthorizeHarvestRetirement(HarvestRetirementRequest{
		Root: root, RepositoryIdentity: harvestTestIdentity, WorktreePath: abs,
	})
	if err != nil {
		t.Fatalf("owned harvest registration must authorize: %v", err)
	}
	if got.Generation != want.Generation {
		t.Fatalf("authorized the wrong receipt: got generation %s want %s", got.Generation, want.Generation)
	}
}

// The defect root caught in review: a first reflog line is NOT a generation.
// Reflog timestamps have one-second precision, so a remove/recreate of the same
// path from the same base by the same actor inside one second reproduces a
// byte-identical birth line and git reuses the basename-derived admin ID.
//
// This test forces that collision deterministically -- it overwrites the
// recreated registration's reflog with the ORIGINAL bytes -- so any
// implementation that derives authority from reflog content authorizes a
// worktree it must refuse, and fails here.
func TestAuthorizeHarvestRetirement_RefusesRecreationWithIdenticalReflogBirthLine(t *testing.T) {
	root := harvestRepo(t)
	rel := filepath.Join(".herd", "worktrees", "harvest-lane-abc")
	abs := addHarvestWorktree(t, root, rel, "harvest/lane-abc", "HEAD")
	recordHarvestReceipt(t, root, rel, abs, "harvest/lane-abc", harvestTestIdentity)

	gitDir, err := HarvestRegistrationDir(abs)
	if err != nil {
		t.Fatalf("resolve registration: %v", err)
	}
	originalReflog, err := os.ReadFile(filepath.Join(gitDir, "logs", "HEAD"))
	if err != nil {
		t.Fatalf("read original reflog: %v", err)
	}
	originalID := filepath.Base(gitDir)

	// The registration is removed and a new worktree takes the same path.
	harvestGit(t, root, "worktree", "remove", "--force", abs)
	recreated := addHarvestWorktree(t, root, rel, "harvest/lane-abc", "HEAD")

	recreatedDir, err := HarvestRegistrationDir(recreated)
	if err != nil {
		t.Fatalf("resolve recreated registration: %v", err)
	}
	if filepath.Base(recreatedDir) != originalID {
		t.Fatalf("fixture precondition failed: git did not reuse admin id %s (got %s)", originalID, filepath.Base(recreatedDir))
	}
	// Force the birth line collision a one-second clock makes possible.
	if err := os.WriteFile(filepath.Join(recreatedDir, "logs", "HEAD"), originalReflog, 0o644); err != nil {
		t.Fatalf("forge identical reflog: %v", err)
	}

	if _, err := AuthorizeHarvestRetirement(HarvestRetirementRequest{
		Root: root, RepositoryIdentity: harvestTestIdentity, WorktreePath: recreated,
	}); err == nil {
		t.Fatal("a re-created worktree at a receipted path must NOT be authorized, " +
			"even with a byte-identical reflog birth line and a reused admin id")
	}

	// Sharper still: give the recreation its own valid marker, as a fresh
	// harvest at that path would. Presence of a marker must not authorize;
	// only the generation the receipt is bound to may.
	if _, _, err := MintHarvestGenerationMarker(recreated, rel, time.Now()); err != nil {
		t.Fatalf("mint marker for recreation: %v", err)
	}
	_, err = AuthorizeHarvestRetirement(HarvestRetirementRequest{
		Root: root, RepositoryIdentity: harvestTestIdentity, WorktreePath: recreated,
	})
	if err == nil {
		t.Fatal("a different generation at a receipted path must NOT be authorized")
	}
	if !strings.Contains(err.Error(), "path was reused") {
		t.Fatalf("refusal must name the reused path, got: %v", err)
	}
}

// A remove/recreate loses the marker: the admin directory goes with the
// registration. Without the marker there is no authority at all.
func TestAuthorizeHarvestRetirement_RemovalLosesTheMarker(t *testing.T) {
	root := harvestRepo(t)
	rel := filepath.Join(".herd", "worktrees", "harvest-lane-abc")
	abs := addHarvestWorktree(t, root, rel, "harvest/lane-abc", "HEAD")
	recordHarvestReceipt(t, root, rel, abs, "harvest/lane-abc", harvestTestIdentity)

	harvestGit(t, root, "worktree", "remove", "--force", abs)
	recreated := addHarvestWorktree(t, root, rel, "harvest/lane-abc", "HEAD")

	if _, _, err := ReadHarvestGenerationMarker(recreated); err == nil {
		t.Fatal("the generation marker must not survive the registration that carried it")
	}
	if _, err := AuthorizeHarvestRetirement(HarvestRetirementRequest{
		Root: root, RepositoryIdentity: harvestTestIdentity, WorktreePath: recreated,
	}); err == nil {
		t.Fatal("an unmarked registration must not be authorized")
	}
}

func TestAuthorizeHarvestRetirement_RefusesUnreceiptedSurface(t *testing.T) {
	root := harvestRepo(t)
	rel := filepath.Join(".herd", "worktrees", "review-surface-abc")
	abs := addHarvestWorktree(t, root, rel, "review/surface-abc", "HEAD")
	// A review surface lives under the same directory and is detached in the
	// live fleet. No receipt names it, so it carries no harvest authority.
	if _, err := AuthorizeHarvestRetirement(HarvestRetirementRequest{
		Root: root, RepositoryIdentity: harvestTestIdentity, WorktreePath: abs,
	}); err == nil {
		t.Fatal("a surface no receipt names must not be authorized")
	}
}

func TestAuthorizeHarvestRetirement_RefusesForeignRepositoryIdentity(t *testing.T) {
	root := harvestRepo(t)
	rel := filepath.Join(".herd", "worktrees", "harvest-lane-abc")
	abs := addHarvestWorktree(t, root, rel, "harvest/lane-abc", "HEAD")
	recordHarvestReceipt(t, root, rel, abs, "harvest/lane-abc", "github.com/someone/else")

	if _, err := AuthorizeHarvestRetirement(HarvestRetirementRequest{
		Root: root, RepositoryIdentity: harvestTestIdentity, WorktreePath: abs,
	}); err == nil {
		t.Fatal("a receipt of another repository must not authorize here")
	}
}

func TestAuthorizeHarvestRetirement_RefusesTamperedReceipt(t *testing.T) {
	root := harvestRepo(t)
	rel := filepath.Join(".herd", "worktrees", "harvest-lane-abc")
	abs := addHarvestWorktree(t, root, rel, "harvest/lane-abc", "HEAD")
	recordHarvestReceipt(t, root, rel, abs, "harvest/lane-abc", harvestTestIdentity)

	// Rewrite the journal with a mutated field, leaving the digest stale.
	journal := HarvestRetirementReceiptsPath(root)
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"lane":"lane"`, `"lane":"other"`, 1)
	if tampered == string(data) {
		t.Fatal("fixture precondition failed: nothing was tampered")
	}
	if err := os.WriteFile(journal, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AuthorizeHarvestRetirement(HarvestRetirementRequest{
		Root: root, RepositoryIdentity: harvestTestIdentity, WorktreePath: abs,
	}); err == nil {
		t.Fatal("a receipt whose binding digest no longer covers its fields must not authorize")
	}
}

func TestValidateHarvestRetirementReceipt_RefusesPathsOutsideManagedDirectory(t *testing.T) {
	base := strings.Repeat("a", 40)
	for name, worktree := range map[string]string{
		"absolute":      string(filepath.Separator) + filepath.Join("tmp", "x"),
		"escaping":      filepath.Join("..", "elsewhere"),
		"outside":       filepath.Join(".herd", "pool-x", "pool-01"),
		"bare-relative": "somewhere",
	} {
		t.Run(name, func(t *testing.T) {
			receipt := NewHarvestRetirementReceipt(time.Now(), HarvestRetirementReceipt{
				Repository: harvestTestIdentity, Worktree: worktree, TempBranch: "harvest/x",
				Lane: "lane", BaseSHA: base, CandidateSHA: base, HeadSHA: base,
				RegistrationID: "harvest-x", Generation: "deadbeef",
			})
			if err := ValidateHarvestRetirementReceipt(receipt); err == nil {
				t.Fatalf("worktree %q must be refused by the schema", worktree)
			}
		})
	}
}

func TestReadHarvestGenerationMarker_RefusesMainCheckout(t *testing.T) {
	root := harvestRepo(t)
	if _, _, err := ReadHarvestGenerationMarker(root); err == nil {
		t.Fatal("the main checkout is not a registration and must never carry harvest authority")
	}
}

// A marker's authority is the registration's OWN private file. A symlink
// parked at the marker path names a file the registration does not carry, so
// it must yield no authority -- even when the target holds a byte-perfect
// valid marker. The pre-fix read used Stat+ReadFile, which follow symlinks
// and authorize exactly this forgery.
func TestReadHarvestGenerationMarker_RefusesSymlinkedMarker(t *testing.T) {
	root := harvestRepo(t)
	rel := filepath.Join(".herd", "worktrees", "harvest-lane-abc")
	abs := addHarvestWorktree(t, root, rel, "harvest/lane-abc", "HEAD")
	if _, _, err := MintHarvestGenerationMarker(abs, rel, time.Now()); err != nil {
		t.Fatalf("mint marker: %v", err)
	}
	gitDir, err := HarvestRegistrationDir(abs)
	if err != nil {
		t.Fatalf("resolve registration: %v", err)
	}
	markerPath := filepath.Join(gitDir, HarvestGenerationMarkerFile)
	genuine, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read minted marker: %v", err)
	}
	external := filepath.Join(t.TempDir(), "external-marker.json")
	if err := os.WriteFile(external, genuine, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, markerPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadHarvestGenerationMarker(abs); err == nil {
		t.Fatal("a symlinked marker must yield no authority, even when its target is a byte-perfect marker")
	}
	// And without a readable marker, the whole retirement binding fails.
	if _, err := AuthorizeHarvestRetirement(HarvestRetirementRequest{
		Root: root, RepositoryIdentity: harvestTestIdentity, WorktreePath: abs,
	}); err == nil {
		t.Fatal("a registration whose marker is a symlink must not be authorized")
	}
}

// A record larger than any marker can be is refused by the read itself, not
// only by a size probe: the read consumes at most limit+1 bytes and refuses
// when the file is longer than the limit.
func TestReadHarvestGenerationMarker_RefusesOversizedMarker(t *testing.T) {
	root := harvestRepo(t)
	rel := filepath.Join(".herd", "worktrees", "harvest-lane-abc")
	abs := addHarvestWorktree(t, root, rel, "harvest/lane-abc", "HEAD")
	gitDir, err := HarvestRegistrationDir(abs)
	if err != nil {
		t.Fatalf("resolve registration: %v", err)
	}
	oversized := bytes.Repeat([]byte("x"), maxHarvestMarkerBytes+1)
	if err := os.WriteFile(filepath.Join(gitDir, HarvestGenerationMarkerFile), oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadHarvestGenerationMarker(abs); err == nil {
		t.Fatal("a marker larger than the marker bound must be refused")
	}
}
