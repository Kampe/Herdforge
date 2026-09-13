package sync

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

// FAC-831: a pull-request landing integrates reviewed work with a merge commit
// whose own diff is EMPTY. The patch belongs to the carrier, so a receipt that
// binds content to the merge alone cannot be validated and BoardDone refuses
// work that genuinely landed.
//
// The content gate for a sealed carrier is the producer's own predicate: replay
// the sealed reviewed delta onto the integration commit's first parent and
// require the result to equal its tree. These drive it through real git
// topologies, and the ones that must be refused are here beside the ones that
// must be accepted -- a gate is only as good as what it turns away.

const (
	carrierRef      = "FAC-831"
	carrierTaskID   = "task-fac-831"
	carrierRevision = "provider-rev-831"
	carrierLeaseGen = 4
)

// fixture is a hermetic git repository. No network, no global git config, and
// no assumption about the ambient default branch.
type fixture struct {
	t   *testing.T
	dir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir()}
	f.run("init", "-q", "-b", "main")
	f.run("config", "user.email", "fixture@example.com")
	f.run("config", "user.name", "fixture")
	f.run("config", "commit.gpgsign", "false")
	// RepositoryIdentity reads this config to bind a receipt to its repository.
	// It is a CONFIG read only: nothing here fetches.
	f.run("config", "remote.origin.url", "git@github.com:Kampe/Herdforge-fixture.git")
	return f
}

func (f *fixture) run(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) commit(name, body, msg string) string {
	f.t.Helper()
	writeFileTest(f.t, f.dir, name, body)
	f.run("add", name)
	f.run("commit", "-q", "-m", msg)
	return f.run("rev-parse", "HEAD")
}

func (f *fixture) blob(commit, path string) string {
	f.t.Helper()
	return f.run("rev-parse", commit+":"+path)
}

// publishAsOriginMain is what makes the merge the integration commit the
// consumer will accept as landed.
func (f *fixture) publishAsOriginMain(merge string) {
	f.t.Helper()
	f.run("update-ref", "refs/remotes/origin/main", merge)
}

// carrierReceipt binds a sealed carrier for the CONTENT helper. Nothing here is
// optional: the replay needs the sealed base and candidate, so a receipt that
// omits them is not a shape the gate ever sees.
func carrierReceipt(t *testing.T, dir, base, carrier, candidate, merge string) CompletionReceipt {
	t.Helper()
	patch, err := PatchID(dir, carrier)
	if err != nil {
		t.Fatalf("carrier must have a patch id: %v", err)
	}
	return CompletionReceipt{
		BaseSHA: base, CandidateSHA: candidate, MergeSHA: merge,
		ContentSHA: carrier, PatchID: patch,
	}
}

// publicReceipt builds a receipt complete enough for the PUBLIC gate, so a
// topology is proven through CompletionReceipt.Validate -- the entry point
// `herd approve` actually calls -- rather than through the content helper alone.
// The lifecycle state it returns matches the receipt, so any refusal these tests
// observe comes from the content gate and not from an unrelated binding.
func publicReceipt(t *testing.T, dir, base, carrier, candidate, merge string) (CompletionReceipt, *lifecycle.TaskState) {
	t.Helper()
	repoID, err := toolchild.RepositoryIdentity(dir)
	if err != nil {
		t.Fatalf("repository identity: %v", err)
	}
	r := carrierReceipt(t, dir, base, carrier, candidate, merge)
	r.RepoID = repoID
	r.TaskRef = carrierRef
	r.TaskID = carrierTaskID
	r.ProviderRevision = carrierRevision
	r.LeaseGeneration = carrierLeaseGen
	r.AcceptanceDigest = "acceptance-digest-831"
	r.VerificationDigest = "verification-digest-831"
	r.RiskTier = "R3"
	r.AuthorFamily = "anthropic"
	r.ReviewerFamily = "openai"
	r.Verdict = "PASS"
	r.IntegrationResult = IntegrationMerged
	r.Seal()
	st := &lifecycle.TaskState{
		TaskRef: carrierRef, State: lifecycle.StateIntegrated,
		LeaseGeneration: carrierLeaseGen, CandidateSHA: candidate,
	}
	return r, st
}

// ---------------------------------------------------------------------------
// Topology 1: the plain pull-request landing. The reviewed tip IS the carrier
// and the merge carries no patch of its own.
// ---------------------------------------------------------------------------

func prReceiptRepo(t *testing.T) (dir, baseSHA, carrierSHA, mergeSHA string) {
	t.Helper()
	f := newFixture(t)
	f.commit("a.txt", "root\n", "root")
	baseSHA = f.commit("base.txt", "reviewed base\n", "reviewed base")

	f.run("checkout", "-q", "-b", "pr")
	carrierSHA = f.commit("reviewed.txt", "reviewed content\n", "reviewed candidate work")

	f.run("checkout", "-q", "main")
	f.run("merge", "-q", "--no-ff", "-m", "Merge pull request #831", "pr")
	mergeSHA = f.run("rev-parse", "HEAD")
	f.publishAsOriginMain(mergeSHA)

	if parents := f.run("rev-list", "--parents", "-n", "1", mergeSHA); len(strings.Fields(parents)) != 3 {
		t.Fatalf("fixture invalid: %s is not a two-parent merge (%s)", mergeSHA[:12], parents)
	}
	if _, err := PatchID(f.dir, mergeSHA); err == nil {
		t.Fatal("fixture invalid: the merge has a patch id, so the old merge-bound gate would have passed")
	}
	return f.dir, baseSHA, carrierSHA, mergeSHA
}

func TestValidateAcceptsSealedCarrierForAPullRequestLanding(t *testing.T) {
	dir, base, carrier, merge := prReceiptRepo(t)
	r := carrierReceipt(t, dir, base, carrier, carrier, merge)
	if err := r.validateContentBindingFresh(dir); err != nil {
		t.Fatalf("a pull-request landing must validate against its sealed carrier: %v", err)
	}
}

// The legacy path, unchanged: with no ContentSHA the gate is exactly the
// merge-bound check it always was, an empty merge still fails it, and no replay
// runs at all.
func TestValidateStillBindsContentToTheMergeWhenNoCarrierIsSealed(t *testing.T) {
	dir, base, _, merge := prReceiptRepo(t)
	r := CompletionReceipt{MergeSHA: merge, PatchID: "whatever", BaseSHA: base}
	err := r.validateContentBindingFresh(dir)
	if err == nil {
		t.Fatal("a merge with no patch of its own must still be refused when no carrier is sealed")
	}
	if !strings.Contains(err.Error(), "empty commit") {
		t.Fatalf("legacy refusal changed shape: %v", err)
	}
}

// The two refusals that do not depend on the replay at all: a carrier the merge
// never took, and a carrier whose patch is not the sealed one.
func TestValidateRefusesForgedAndPatchMismatchedCarriers(t *testing.T) {
	t.Run("carrier that is not in the merge", func(t *testing.T) {
		dir, base, carrier, merge := prReceiptRepo(t)
		f := &fixture{t: t, dir: dir}
		// A commit with the SAME patch, on a line the merge never took.
		f.run("checkout", "-q", "-b", "forged", base)
		forged := f.commit("reviewed.txt", "reviewed content\n", "forged carrier")
		f.run("checkout", "-q", "main")
		if forged == carrier {
			t.Fatal("fixture invalid: the forged carrier is the real one")
		}
		r := carrierReceipt(t, dir, base, forged, carrier, merge)
		err := r.validateContentBindingFresh(dir)
		if err == nil {
			t.Fatal("a carrier the merge does not contain was accepted; patch equality alone is not landing")
		}
		if !strings.Contains(err.Error(), "is not an ancestor of merge sha") {
			t.Fatalf("refusal did not name the ancestry rule: %v", err)
		}
	})

	t.Run("carrier whose patch does not match the sealed one", func(t *testing.T) {
		dir, base, carrier, merge := prReceiptRepo(t)
		r := carrierReceipt(t, dir, base, carrier, carrier, merge)
		r.PatchID = strings.Repeat("0", 40)
		err := r.validateContentBindingFresh(dir)
		if err == nil {
			t.Fatal("a receipt whose sealed patch id does not match the carrier was accepted")
		}
		if !strings.Contains(err.Error(), "not the accepted candidate's patch") {
			t.Fatalf("refusal did not name the patch rule: %v", err)
		}
	})
}

// TestSealedCarrierIsCoveredByTheDigest proves the carrier cannot be added,
// removed or swapped without invalidating the receipt, and that a receipt
// WITHOUT one keeps the digest it had before the field existed.
func TestSealedCarrierIsCoveredByTheDigest(t *testing.T) {
	base := CompletionReceipt{Version: 1, RepoID: "r", TaskRef: "FAC-831", MergeSHA: "m", PatchID: "p"}
	legacy := base.ComputeDigest()

	withCarrier := base
	withCarrier.ContentSHA = "c"
	if withCarrier.ComputeDigest() == legacy {
		t.Fatal("adding a carrier did not change the digest; it is not sealed")
	}

	swapped := withCarrier
	swapped.ContentSHA = "c2"
	if swapped.ComputeDigest() == withCarrier.ComputeDigest() {
		t.Fatal("swapping the carrier did not change the digest")
	}

	dropped := withCarrier
	dropped.ContentSHA = ""
	if dropped.ComputeDigest() != legacy {
		t.Fatal("a receipt with no carrier must digest exactly as it did before the field existed")
	}
}

// ---------------------------------------------------------------------------
// Topology 2: the iterated lane. The carrier is an INTERMEDIATE commit whose own
// path is revised again before the landing -- the shape this repository produces
// constantly, and the one a byte-identity check refused.
// ---------------------------------------------------------------------------

func iteratedPRRepo(t *testing.T, discarded bool) (dir, base, carrier, candidate, merge string) {
	t.Helper()
	f := newFixture(t)
	f.commit("a.txt", "root\n", "root")
	// The reviewed path already exists on main, so an `ours` merge can keep
	// main's bytes at it rather than simply dropping a file.
	base = f.commit("shared.txt", "base\n", "reviewed base")

	f.run("checkout", "-q", "-b", "pr")
	carrier = f.commit("shared.txt", "reviewed v1\n", "reviewed candidate work")
	candidate = f.commit("shared.txt", "reviewed v2\n", "revise the same path inside the pull request")

	f.run("checkout", "-q", "main")
	if discarded {
		mainRevision := f.commit("shared.txt", "main revision\n", "main revises the same path")
		f.run("merge", "-q", "--no-ff", "-s", "ours", "-m", "Merge pull request #831 (ours)", "pr")
		merge = f.run("rev-parse", "HEAD")
		if f.run("rev-parse", merge+"^{tree}") != f.run("rev-parse", mainRevision+"^{tree}") {
			t.Fatal("fixture invalid: the ours merge did not keep main's tree")
		}
	} else {
		f.run("merge", "-q", "--no-ff", "-m", "Merge pull request #831", "pr")
		merge = f.run("rev-parse", "HEAD")
		if f.blob(merge, "shared.txt") != f.blob(candidate, "shared.txt") {
			t.Fatal("fixture invalid: the merge does not hold the reviewed line's last revision")
		}
	}
	f.publishAsOriginMain(merge)

	// Non-vacuous either way: the carrier's OWN bytes are not what the merge
	// holds, so a check that compared only those would be answering a different
	// question than the one under test.
	if f.blob(merge, "shared.txt") == f.blob(carrier, "shared.txt") {
		t.Fatal("fixture invalid: the merge still holds the carrier's own bytes")
	}
	return f.dir, base, carrier, candidate, merge
}

// An honest intermediate carrier: the reviewed line revised its own path before
// the landing. This must be ACCEPTED, or every iterated lane is refused a
// receipt for work that genuinely landed.
func TestValidateAcceptsALaterRevisionOfTheCarriersOwnPath(t *testing.T) {
	dir, base, carrier, candidate, merge := iteratedPRRepo(t, false)
	r := carrierReceipt(t, dir, base, carrier, candidate, merge)
	if err := r.validateContentBindingFresh(dir); err != nil {
		t.Fatalf("an honest landing whose carrier was revised again inside the pull request was refused: %v", err)
	}
}

// The ours merge, through the PUBLIC gate: the carrier stays an ancestor and the
// reviewed hunks are gone. Ancestry says yes and the tree says no.
func TestValidateRefusesAMergeThatDiscardedTheReviewedContent(t *testing.T) {
	dir, base, carrier, candidate, merge := iteratedPRRepo(t, true)
	r, st := publicReceipt(t, dir, base, carrier, candidate, merge)
	err := r.Validate(dir, carrierRef, st)
	if err == nil {
		t.Fatal("a merge that discarded every reviewed hunk was accepted")
	}
	if !strings.Contains(err.Error(), "does not preserve the content of") {
		t.Fatalf("refusal did not name the content rule: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Topology 3: the reviewed line MOVES the carrier's file. The old name is then
// absent in both the reviewed tip and the merge, and a path-by-path comparison
// reads that shared absence as agreement -- which is how an altered destination
// used to validate.
// ---------------------------------------------------------------------------

func renamedPRRepo(t *testing.T, altered, edited bool) (dir, base, carrier, candidate, merge string) {
	t.Helper()
	f := newFixture(t)
	f.commit("a.txt", "root\n", "root")
	base = f.commit("reviewed.txt", "base\n", "reviewed base")

	f.run("checkout", "-q", "-b", "pr")
	carrier = f.commit("reviewed.txt", "reviewed v1\n", "reviewed candidate work")

	f.run("mv", "reviewed.txt", "renamed.txt")
	if edited {
		writeFileTest(t, f.dir, "renamed.txt", "reviewed v2\n")
		f.run("add", "renamed.txt")
	}
	f.run("commit", "-q", "-m", "move the reviewed file inside the pull request")
	candidate = f.run("rev-parse", "HEAD")

	f.run("checkout", "-q", "main")
	f.run("merge", "-q", "--no-ff", "-m", "Merge pull request #831", "pr")
	if altered {
		writeFileTest(t, f.dir, "renamed.txt", "SUBSTITUTED content\n")
		f.run("add", "renamed.txt")
		f.run("commit", "-q", "--amend", "--no-edit")
	}
	merge = f.run("rev-parse", "HEAD")
	f.publishAsOriginMain(merge)

	// Non-vacuity, asserted rather than assumed.
	if strings.Contains(f.run("ls-tree", "--name-only", "-r", merge), "reviewed.txt") {
		t.Fatal("fixture invalid: the merge still holds the old name, so no rename is under test")
	}
	if !strings.Contains(f.run("ls-tree", "--name-only", "-r", merge), "renamed.txt") {
		t.Fatal("fixture invalid: the merge does not hold the destination")
	}
	if edited && f.blob(candidate, "renamed.txt") == f.blob(carrier, "reviewed.txt") {
		t.Fatal("fixture invalid: the rename was supposed to edit the content too")
	}
	if !edited && f.blob(candidate, "renamed.txt") != f.blob(carrier, "reviewed.txt") {
		t.Fatal("fixture invalid: the rename changed the content, so it is not a pure move")
	}
	if altered && f.blob(merge, "renamed.txt") == f.blob(candidate, "renamed.txt") {
		t.Fatal("fixture invalid: the amend did not substitute the destination's bytes")
	}
	return f.dir, base, carrier, candidate, merge
}

// A pure move of the carrier's file, through the PUBLIC gate: ACCEPTED.
func TestValidateFollowsAReviewedRenameToItsDestination(t *testing.T) {
	dir, base, carrier, candidate, merge := renamedPRRepo(t, false, false)
	r, st := publicReceipt(t, dir, base, carrier, candidate, merge)
	if err := r.Validate(dir, carrierRef, st); err != nil {
		t.Fatalf("an honest reviewed rename was refused: %v", err)
	}
}

// A move that EDITS the file in the same reviewed commit. No comparison of the
// old path, by name or by blob, can establish where that content went -- the
// bytes exist nowhere in the repository under the old name. Replaying the whole
// reviewed delta reproduces the rename and the edit together, so this is
// ACCEPTED rather than refused for being unfollowable.
func TestValidateAcceptsARenameThatEditsInTheSameReviewedCommit(t *testing.T) {
	dir, base, carrier, candidate, merge := renamedPRRepo(t, false, true)
	r, st := publicReceipt(t, dir, base, carrier, candidate, merge)
	if err := r.Validate(dir, carrierRef, st); err != nil {
		t.Fatalf("an honest rename that edited in the same reviewed commit was refused: %v", err)
	}
}

// The attack the move enables: the destination holds different bytes in the
// merge. The old name is equally absent on both sides, so only a whole-result
// claim catches it. REFUSED.
func TestValidateRefusesAnAlteredRenameDestination(t *testing.T) {
	dir, base, carrier, candidate, merge := renamedPRRepo(t, true, false)
	r, st := publicReceipt(t, dir, base, carrier, candidate, merge)
	err := r.Validate(dir, carrierRef, st)
	if err == nil {
		t.Fatal("an altered rename destination was accepted: a shared absence of the old path is not proof of what landed at the new one")
	}
	if !strings.Contains(err.Error(), "does not preserve the content of") {
		t.Fatalf("refusal did not name the content rule: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Topology 4: main and the reviewed line change DIFFERENT HUNKS of one file.
// The merged file equals neither side's, which is ordinary and honest -- and
// which every path-by-path or blob-by-blob comparison refuses.
// ---------------------------------------------------------------------------

const hunkedFile = "top\nmiddle 1\nmiddle 2\nmiddle 3\nmiddle 4\nmiddle 5\nbottom\n"

func independentHunkRepo(t *testing.T) (dir, base, carrier, candidate, merge string) {
	t.Helper()
	f := newFixture(t)
	f.commit("a.txt", "root\n", "root")
	base = f.commit("file.txt", hunkedFile, "reviewed base")

	f.run("checkout", "-q", "-b", "pr")
	carrier = f.commit("file.txt", strings.Replace(hunkedFile, "top\n", "top reviewed\n", 1),
		"reviewed candidate work: the top of the file")
	candidate = f.commit("other.txt", "more reviewed work\n", "reviewed candidate work: a second file")

	f.run("checkout", "-q", "main")
	mainRevision := f.commit("file.txt", strings.Replace(hunkedFile, "bottom\n", "bottom main\n", 1),
		"main revises the other end of the same file")
	f.run("merge", "-q", "--no-ff", "-m", "Merge pull request #831", "pr")
	merge = f.run("rev-parse", "HEAD")
	f.publishAsOriginMain(merge)

	// The point of the fixture: the merged file is NEITHER side's.
	if f.blob(merge, "file.txt") == f.blob(carrier, "file.txt") {
		t.Fatal("fixture invalid: the merged file is the carrier's, so main's hunk is not represented")
	}
	if f.blob(merge, "file.txt") == f.blob(mainRevision, "file.txt") {
		t.Fatal("fixture invalid: the merged file is main's, so the reviewed hunk is not represented")
	}
	merged := f.run("show", merge+":file.txt")
	if !strings.Contains(merged, "top reviewed") || !strings.Contains(merged, "bottom main") {
		t.Fatalf("fixture invalid: the merged file does not carry both hunks:\n%s", merged)
	}
	return f.dir, base, carrier, candidate, merge
}

// Both sides edited one file, in different places, and both survived. This is an
// honest landing and must be ACCEPTED: refusing it is the false refusal a
// path-scoped comparison could not avoid.
func TestValidateAcceptsIndependentMainAndReviewedHunks(t *testing.T) {
	dir, base, carrier, candidate, merge := independentHunkRepo(t)
	r, st := publicReceipt(t, dir, base, carrier, candidate, merge)
	if err := r.Validate(dir, carrierRef, st); err != nil {
		t.Fatalf("an honest landing where main and the reviewed line edited different hunks was refused: %v", err)
	}
}

// The sealed candidate is what gets replayed, so swapping it for another commit
// on the same line -- here the carrier, dropping the rest of the reviewed work --
// replays a different delta and lands a different tree. The lifecycle state is
// built to MATCH the substitution, so the refusal can only come from the content
// gate and not from an unrelated binding.
func TestValidateRefusesASubstitutedSealedCandidate(t *testing.T) {
	dir, base, carrier, candidate, merge := independentHunkRepo(t)
	if carrier == candidate {
		t.Fatal("fixture invalid: the substitution is not a different commit")
	}
	r, st := publicReceipt(t, dir, base, carrier, carrier, merge)
	err := r.Validate(dir, carrierRef, st)
	if err == nil {
		t.Fatal("a receipt whose sealed candidate was swapped for another commit was accepted")
	}
	if !strings.Contains(err.Error(), "does not preserve the content of") {
		t.Fatalf("refusal did not name the content rule: %v", err)
	}
}

// The degenerate substitution: naming the integration commit as its own reviewed
// candidate. Replaying a commit onto its own parent reproduces its tree, so the
// claim would prove itself. REFUSED before any replay runs.
func TestValidateRefusesTheIntegrationCommitAsItsOwnCandidate(t *testing.T) {
	dir, base, carrier, _, merge := independentHunkRepo(t)
	r, st := publicReceipt(t, dir, base, carrier, merge, merge)
	err := r.Validate(dir, carrierRef, st)
	if err == nil {
		t.Fatal("the integration commit was accepted as its own reviewed candidate")
	}
	if !strings.Contains(err.Error(), "no reviewed delta to replay") {
		t.Fatalf("refusal did not name the self-proving rule: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The budget. Proven through an INJECTED command, so these assert the guard and
// never the speed of the machine or the size of the repository that ran them.
// ---------------------------------------------------------------------------

func stubContentProofCommand(t *testing.T, fn func(context.Context, string, []byte, ...string) (string, error)) {
	t.Helper()
	prev := contentProofCommand
	contentProofCommand = fn
	t.Cleanup(func() { contentProofCommand = prev })
}

func TestContentProofRefusesAfterItsDeadline(t *testing.T) {
	calls := 0
	stubContentProofCommand(t, func(context.Context, string, []byte, ...string) (string, error) {
		calls++
		return "", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	proof := &contentProof{repoDir: t.TempDir(), ctx: ctx, maxCommands: contentProofMaxCommands}
	_, err := proof.run("rev-parse", "HEAD")
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("an expired deadline must stop the proof before it starts a process: %v", err)
	}
	if calls != 0 {
		t.Fatalf("the proof started %d process(es) after its deadline was gone", calls)
	}
}

func TestContentProofRefusesWhenTheCommandBudgetIsSpent(t *testing.T) {
	calls := 0
	stubContentProofCommand(t, func(context.Context, string, []byte, ...string) (string, error) {
		calls++
		return "", nil
	})
	proof := &contentProof{repoDir: t.TempDir(), ctx: context.Background(), maxCommands: contentProofMaxCommands}
	for i := 0; i < contentProofMaxCommands; i++ {
		if _, err := proof.run("rev-parse", "HEAD"); err != nil {
			t.Fatalf("command %d was refused while the budget still had room: %v", i+1, err)
		}
	}
	_, err := proof.run("rev-parse", "HEAD")
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("the command budget did not stop the proof after %d commands: %v", contentProofMaxCommands, err)
	}
	if calls != contentProofMaxCommands {
		t.Fatalf("the proof ran %d processes, not the %d its budget allows", calls, contentProofMaxCommands)
	}
}

func TestContentProofRefusesOversizeCommandOutput(t *testing.T) {
	stubContentProofCommand(t, func(context.Context, string, []byte, ...string) (string, error) {
		return strings.Repeat("x", contentProofMaxOutputBytes+1), nil
	})
	proof := &contentProof{repoDir: t.TempDir(), ctx: context.Background(), maxCommands: contentProofMaxCommands}
	_, err := proof.run("ls-tree", "-r", "HEAD")
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("output larger than the proof budget was accepted: %v", err)
	}
}

// The seam test above proves the LOGICAL cap. This proves the PHYSICAL one: the
// writer exec hands to git refuses to grow, so an adversarial history is stopped
// while it is still being written rather than measured once it is all in memory.
func TestBoundedOutputRefusesToGrowPastItsCap(t *testing.T) {
	w := &boundedOutput{max: 8}
	if n, err := w.Write([]byte("12345678")); n != 8 || err != nil {
		t.Fatalf("a write within the cap was refused: n=%d err=%v", n, err)
	}
	n, err := w.Write([]byte("9"))
	if err == nil {
		t.Fatalf("a write past the cap was accepted: n=%d", n)
	}
	if !w.overflowed {
		t.Fatal("the writer did not record the overflow, so a truncated read would be reported as a complete one")
	}
}

// ---------------------------------------------------------------------------
// The PUBLIC entry's budget. Validate makes git calls in five stages -- identity,
// integration ancestry, carrier ancestry, replay, patch -- and bounding only the
// interesting one bounds a fragment of the validation and leaves the rest as the
// real limit. These drive the ACTUAL public entry with a finite injected
// allowance and name the stage that spends it.
// ---------------------------------------------------------------------------

// recordCommands wraps the real bounded runner and records every argv, so a test
// can say WHICH stage spent the last command rather than only that one did.
func recordCommands(t *testing.T) *[][]string {
	t.Helper()
	seen := &[][]string{}
	prev := contentProofCommand
	contentProofCommand = func(ctx context.Context, repoDir string, stdin []byte, args ...string) (string, error) {
		*seen = append(*seen, append([]string(nil), args...))
		return prev(ctx, repoDir, stdin, args...)
	}
	t.Cleanup(func() { contentProofCommand = prev })
	return seen
}

// withCommandAllowance injects a finite cumulative allowance into the real
// public entry. Production never writes this.
func withCommandAllowance(t *testing.T, n int) {
	t.Helper()
	prev := contentProofMaxCommands
	contentProofMaxCommands = n
	t.Cleanup(func() { contentProofMaxCommands = prev })
}

func ranCommand(seen [][]string, marker string) bool {
	for _, args := range seen {
		for _, a := range args {
			if a == marker {
				return true
			}
		}
	}
	return false
}

// A carrier landing that validates cleanly, used by every stage test below so
// the only variable is the allowance.
func budgetFixture(t *testing.T) (dir string, r CompletionReceipt, st *lifecycle.TaskState) {
	t.Helper()
	dir, base, carrier, merge := prReceiptRepo(t)
	r, st = publicReceipt(t, dir, base, carrier, carrier, merge)
	return dir, r, st
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// exhaustAtStage runs the SAME validation twice: once with the default allowance
// to observe the real command sequence, then with an allowance that ends exactly
// where the named stage begins.
//
// The boundary is DERIVED from that observation rather than hardcoded, so these
// tests pin the stage and not a command count that a change in any other stage
// -- or in the shared identity reader, which is not this package's code -- could
// silently shift underneath them.
func exhaustAtStage(t *testing.T, stage string, match func(CompletionReceipt, []string) bool) (error, [][]string) {
	t.Helper()
	dir, r, st := budgetFixture(t)
	seen := recordCommands(t)
	if err := r.Validate(dir, carrierRef, st); err != nil {
		t.Fatalf("the fixture must validate cleanly before any budget is injected: %v", err)
	}
	observed := append([][]string(nil), (*seen)...)
	at := -1
	for i, args := range observed {
		if match(r, args) {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatalf("no command in the observed validation belongs to the %s stage: %v", stage, observed)
	}
	*seen = nil
	withCommandAllowance(t, at)
	return r.Validate(dir, carrierRef, st), *seen
}

func TestValidateStopsInTheIdentityStageWhenTheBudgetIsGone(t *testing.T) {
	err, seen := exhaustAtStage(t, "identity", func(_ CompletionReceipt, a []string) bool { return hasArg(a, "config") })
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("an exhausted budget in the identity stage must reach the caller as the budget sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot resolve repository identity") {
		t.Fatalf("the identity read did not spend the validation budget: %v", err)
	}
	if ranCommand(seen, "config") {
		t.Fatalf("the identity read ran outside the allowance: %v", seen)
	}
}

func TestValidateStopsInTheIntegrationAncestryStageWhenTheBudgetIsGone(t *testing.T) {
	err, seen := exhaustAtStage(t, "integration ancestry", func(_ CompletionReceipt, a []string) bool {
		return len(a) > 0 && a[0] == "rev-parse" && hasArg(a, "origin/main")
	})
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("an exhausted budget in the integration ancestry stage must reach the caller as the budget sentinel: %v", err)
	}
	if strings.Contains(err.Error(), "no origin/main") {
		t.Fatalf("an exhausted budget was reported as a missing origin/main: %v", err)
	}
	if ranCommand(seen, "origin/main") {
		t.Fatalf("the integration probes ran outside the allowance: %v", seen)
	}
}

func TestValidateStopsInTheCarrierAncestryStageWhenTheBudgetIsGone(t *testing.T) {
	var carrier string
	err, seen := exhaustAtStage(t, "carrier ancestry", func(r CompletionReceipt, a []string) bool {
		carrier = r.ContentSHA
		return len(a) > 0 && a[0] == "merge-base" && hasArg(a, r.ContentSHA)
	})
	if carrier == "" {
		t.Fatal("fixture invalid: no carrier is sealed, so there is no carrier ancestry stage")
	}
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("an exhausted budget in the carrier ancestry stage must reach the caller as the budget sentinel: %v", err)
	}
	if strings.Contains(err.Error(), "is not an ancestor of merge sha") {
		t.Fatalf("an exhausted budget was reported as a carrier that is not an ancestor: %v", err)
	}
	if ranCommand(seen, carrier) {
		t.Fatalf("the carrier ancestry probe ran outside the allowance: %v", seen)
	}
}

func TestValidateStopsInTheReplayStageWhenTheBudgetIsGone(t *testing.T) {
	err, seen := exhaustAtStage(t, "replay", func(_ CompletionReceipt, a []string) bool {
		for _, s := range a {
			if strings.HasSuffix(s, "^1") {
				return true
			}
		}
		return false
	})
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("an exhausted budget in the replay stage must reach the caller as the budget sentinel: %v", err)
	}
	if strings.Contains(err.Error(), "does not preserve the content of") {
		t.Fatalf("an exhausted budget was reported as content that did not land: %v", err)
	}
	if ranCommand(seen, "merge-tree") {
		t.Fatalf("the replay ran outside the allowance: %v", seen)
	}
}

func TestValidateStopsInThePatchStageWhenTheBudgetIsGone(t *testing.T) {
	err, seen := exhaustAtStage(t, "patch", func(_ CompletionReceipt, a []string) bool { return len(a) > 0 && a[0] == "diff-tree" })
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("an exhausted budget in the patch stage must reach the caller as the budget sentinel: %v", err)
	}
	if ranCommand(seen, "patch-id") || ranCommand(seen, "diff-tree") {
		t.Fatalf("the patch pipeline ran outside the allowance: %v", seen)
	}
}

func TestValidateSpendsOneSharedBudgetAndNeverResetsIt(t *testing.T) {
	dir, r, st := budgetFixture(t)
	seen := recordCommands(t)
	if err := r.Validate(dir, carrierRef, st); err != nil {
		t.Fatalf("the honest landing must validate under the default allowance: %v", err)
	}
	total := len(*seen)
	if total == 0 {
		t.Fatal("the validation ran no git commands at all, so it proves nothing about a budget")
	}
	if total > contentProofMaxCommands {
		t.Fatalf("one validation spends %d commands, more than the %d allowance", total, contentProofMaxCommands)
	}

	*seen = nil
	withCommandAllowance(t, total-1)
	err := r.Validate(dir, carrierRef, st)
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("an allowance one command short of the whole validation was not exhausted, so a stage re-minted the budget: %v", err)
	}
	if len(*seen) != total-1 {
		t.Fatalf("the validation ran %d commands on an allowance of %d", len(*seen), total-1)
	}
}

// The patch pipeline is bounded at both ends. Its OUTPUT cap stops an oversize
// diff before it is ever fed anywhere.
func TestPatchIDRefusesAnOversizeDiff(t *testing.T) {
	fed := false
	stubContentProofCommand(t, func(_ context.Context, _ string, stdin []byte, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "patch-id" {
			fed = true
			return "", nil
		}
		return strings.Repeat("x", contentProofMaxOutputBytes+1), nil
	})
	proof := &contentProof{repoDir: t.TempDir(), ctx: context.Background(), maxCommands: contentProofMaxCommands}
	_, err := proof.patchID("0000000000000000000000000000000000000000")
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("an oversize diff was accepted into the patch pipeline: %v", err)
	}
	if fed {
		t.Fatal("git patch-id was fed a diff that had already exceeded the output cap")
	}
}

// And its INPUT cap refuses to hand a process more bytes than the budget allows,
// before starting it. patchID cannot reach this today because the diff it feeds
// is already capped by the output limit above; the guard is what keeps that true
// if another caller ever passes stdin.
func TestContentProofRefusesAnOversizePatchInput(t *testing.T) {
	started := false
	stubContentProofCommand(t, func(context.Context, string, []byte, ...string) (string, error) {
		started = true
		return "", nil
	})
	proof := &contentProof{repoDir: t.TempDir(), ctx: context.Background(), maxCommands: contentProofMaxCommands}
	_, err := proof.runRaw(make([]byte, contentProofMaxOutputBytes+1), "patch-id", "--stable")
	if !errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("oversize input was accepted into a process: %v", err)
	}
	if started {
		t.Fatal("the process was started before its input was measured")
	}
}

// The owned-group wiring, asserted STRUCTURALLY: the command this package hands
// git is built by procsignal.CommandContext, so a cancelled deadline kills the
// group and a git that spawned a helper cannot keep running with the pipe open.
//
// This deliberately starts NO process and spawns no tree. The behaviour of the
// group kill itself is already proven once, at the primitive, by
// procsignal.TestCommandContextKillsDescendantOnTimeout; re-running that here
// would be a second copy of one claim. What is NOT proven there, and is proven
// here, is that THIS package's route actually goes through it rather than bare
// exec.CommandContext.
func TestBoundedGitIsBuiltForAnOwnedProcessGroup(t *testing.T) {
	dir := t.TempDir()
	cmd, stdout, stderr := newBoundedCommand(context.Background(), dir, []byte("patch bytes"), "patch-id", "--stable")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("git is not started in an owned process group, so a cancelled deadline would leave descendants running")
	}
	if cmd.Cancel == nil {
		t.Fatal("the command has no cancel, so the deadline could not tear the group down at all")
	}
	if cmd.Dir != dir {
		t.Fatalf("the command runs in %q, not the repository %q", cmd.Dir, dir)
	}
	if cmd.Stdin == nil {
		t.Fatal("the bounded input was not wired to the process")
	}
	if stdout.max != contentProofMaxOutputBytes || stderr.max != contentProofMaxStderrBytes {
		t.Fatalf("output caps are %d/%d, not %d/%d",
			stdout.max, stderr.max, contentProofMaxOutputBytes, contentProofMaxStderrBytes)
	}
}

// CI 34743907915. A receipt naming a commit this repository does not have makes
// git exit 128 from `merge-base --is-ancestor`, not 1, and the gate surfaced the
// probe's exit status instead of its own refusal. Both statuses are the SAME
// answer here -- a commit that is not present did not land here -- while a
// budget refusal remains no answer at all. This pins all three claims at once.
func TestValidateRefusesACarrierThisRepositoryDoesNotHave(t *testing.T) {
	dir, base, carrier, merge := prReceiptRepo(t)
	r, st := publicReceipt(t, dir, base, carrier, carrier, merge)
	r.ContentSHA = strings.Repeat("c", 40)
	r.Seal()

	err := r.Validate(dir, carrierRef, st)
	if err == nil {
		t.Fatal("a carrier this repository does not have was accepted")
	}
	if !strings.Contains(err.Error(), "is not an ancestor of merge sha") {
		t.Fatalf("the gate reported the probe instead of its own refusal: %v", err)
	}
	if errors.Is(err, ErrContentProofBudget) {
		t.Fatalf("an ordinary missing object was reported as a stopped proof: %v", err)
	}
}

// A probe that never ANSWERED must never be reported as a proved non-ancestor.
// Exit status 1 is git saying no; a tool that never ran, a cancellation that
// arrived from the killed child rather than from our own context, and a spent
// allowance are all stopping causes, and each must reach the caller as itself.
// Inventing a content verdict out of a stopped run is the inversion this gate
// exists to avoid.
func TestValidateDoesNotReportAStoppedProbeAsANonAncestor(t *testing.T) {
	dir, base, carrier, merge := prReceiptRepo(t)
	r, st := publicReceipt(t, dir, base, carrier, carrier, merge)

	for _, cause := range []error{
		errors.New(`exec: "git": executable file not found in $PATH`),
		context.Canceled,
		context.DeadlineExceeded,
	} {
		prev := contentProofCommand
		contentProofCommand = func(ctx context.Context, repoDir string, stdin []byte, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "merge-base" {
				return "", cause
			}
			return prev(ctx, repoDir, stdin, args...)
		}
		err := r.Validate(dir, carrierRef, st)
		contentProofCommand = prev

		if err == nil {
			t.Fatalf("a probe that never answered (%v) was accepted as proof", cause)
		}
		if strings.Contains(err.Error(), "is not an ancestor") {
			t.Fatalf("a probe that never answered was reported as a proved non-ancestor: cause %v, error %v", cause, err)
		}
		if !errors.Is(err, cause) {
			t.Fatalf("the stopping cause %v did not survive to the caller: %v", cause, err)
		}
	}
}
