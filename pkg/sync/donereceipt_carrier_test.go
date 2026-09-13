package sync

import (
	"os/exec"
	"strings"
	"testing"
)

// FAC-831: a pull-request landing integrates reviewed work with a merge commit
// whose own diff is EMPTY. The patch belongs to the carrier, so a receipt that
// binds content to the merge alone cannot be validated and BoardDone refuses
// work that genuinely landed.
//
// These drive the shipped consumer path -- CompletionReceipt.validateContentBinding,
// which Validate calls and nothing else does -- against a repo shaped like an
// actual pull request: a reviewed base on main, a carrier on a
// side line, and a two-parent merge whose tree holds the reviewed bytes.

// prReceiptRepo builds that shape and returns the repo plus the three
// identities a receipt binds. The merge is asserted to have two parents and an
// empty diff of its own, so a test that passed because the merge happened to
// carry the patch would fail here instead of quietly proving nothing.
func prReceiptRepo(t *testing.T) (dir, baseSHA, carrierSHA, mergeSHA string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "fixture@example.com")
	run("config", "user.name", "fixture")

	writeFileTest(t, dir, "a.txt", "root\n")
	run("add", "a.txt")
	run("commit", "-q", "-m", "root")

	writeFileTest(t, dir, "base.txt", "reviewed base\n")
	run("add", "base.txt")
	run("commit", "-q", "-m", "reviewed base")
	baseSHA = run("rev-parse", "HEAD")

	// The carrier: the reviewed content, on its own line off the base.
	run("checkout", "-q", "-b", "pr")
	writeFileTest(t, dir, "reviewed.txt", "reviewed content\n")
	run("add", "reviewed.txt")
	run("commit", "-q", "-m", "reviewed candidate work")
	carrierSHA = run("rev-parse", "HEAD")

	// The integration: a real merge commit, never a fast-forward.
	run("checkout", "-q", "main")
	run("merge", "-q", "--no-ff", "-m", "Merge pull request #831", "pr")
	mergeSHA = run("rev-parse", "HEAD")
	run("update-ref", "refs/remotes/origin/main", mergeSHA)

	if parents := run("rev-list", "--parents", "-n", "1", mergeSHA); len(strings.Fields(parents)) != 3 {
		t.Fatalf("fixture invalid: %s is not a two-parent merge (%s)", mergeSHA[:12], parents)
	}
	if out := run("diff-tree", "-p", "--no-color", mergeSHA); strings.TrimSpace(out) != "" {
		t.Fatalf("fixture invalid: the merge carries its own diff, so it would not exercise the carrier path")
	}
	if _, err := PatchID(dir, mergeSHA); err == nil {
		t.Fatalf("fixture invalid: the merge has a patch id, so the old merge-bound gate would have passed")
	}
	return dir, baseSHA, carrierSHA, mergeSHA
}

// TestValidateAcceptsSealedCarrierForAPullRequestLanding is the end-to-end case
// the review required: a PR-shaped landing must mint a receipt the ACTUAL
// consumer accepts, not merely a proof object.
func TestValidateAcceptsSealedCarrierForAPullRequestLanding(t *testing.T) {
	dir, base, carrier, merge := prReceiptRepo(t)
	patch, err := PatchID(dir, carrier)
	if err != nil {
		t.Fatalf("carrier must have a patch id: %v", err)
	}
	r := CompletionReceipt{
		MergeSHA:   merge,
		ContentSHA: carrier,
		PatchID:    patch,
		BaseSHA:    base,
	}
	if err := r.validateContentBinding(dir); err != nil {
		t.Fatalf("a PR-shaped landing must validate against its sealed carrier: %v", err)
	}
}

// TestValidateStillBindsContentToTheMergeWhenNoCarrierIsSealed is the legacy
// path, unchanged: with no ContentSHA the gate is exactly the merge-bound check
// it always was, and an empty merge still fails it.
func TestValidateStillBindsContentToTheMergeWhenNoCarrierIsSealed(t *testing.T) {
	dir, base, _, merge := prReceiptRepo(t)
	r := CompletionReceipt{MergeSHA: merge, PatchID: "whatever", BaseSHA: base}
	err := r.validateContentBinding(dir)
	if err == nil {
		t.Fatal("a merge with no patch of its own must still be refused when no carrier is sealed")
	}
	if !strings.Contains(err.Error(), "empty commit") {
		t.Fatalf("legacy refusal changed shape: %v", err)
	}
}

// The negative regressions. Each is a receipt an attacker or a broken producer
// could mint, and each must be refused for its OWN reason.
func TestValidateRefusesForgedDiscardedAndAlteredCarriers(t *testing.T) {
	t.Run("carrier that is not in the merge", func(t *testing.T) {
		dir, base, carrier, merge := prReceiptRepo(t)
		run := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		// A commit with the SAME patch, on a line the merge never took.
		run("checkout", "-q", "-b", "forged", base)
		writeFileTest(t, dir, "reviewed.txt", "reviewed content\n")
		run("add", "reviewed.txt")
		run("commit", "-q", "-m", "forged carrier")
		forged := run("rev-parse", "HEAD")
		run("checkout", "-q", "main")
		if forged == carrier {
			t.Fatal("fixture invalid: the forged carrier is the real one")
		}
		patch, err := PatchID(dir, forged)
		if err != nil {
			t.Fatal(err)
		}
		r := CompletionReceipt{MergeSHA: merge, ContentSHA: forged, PatchID: patch, BaseSHA: base}
		err = r.validateContentBinding(dir)
		if err == nil {
			t.Fatal("a carrier the merge does not contain was accepted; patch equality alone is not landing")
		}
		if !strings.Contains(err.Error(), "is not an ancestor of merge sha") {
			t.Fatalf("refusal did not name the ancestry rule: %v", err)
		}
	})

	t.Run("merge that discarded the reviewed content", func(t *testing.T) {
		dir, base, carrier, _ := prReceiptRepo(t)
		run := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		// An ours merge: the carrier stays an ancestor, the content does not
		// land. Ancestry says yes and the tree says no.
		run("checkout", "-q", "-B", "ours", base)
		run("merge", "-q", "--no-ff", "-s", "ours", "-m", "Merge pull request #831 (ours)", carrier)
		discarded := run("rev-parse", "HEAD")
		run("update-ref", "refs/remotes/origin/main", discarded)
		if out := run("ls-tree", "--name-only", discarded); strings.Contains(out, "reviewed.txt") {
			t.Fatal("fixture invalid: the ours merge kept the reviewed file")
		}
		patch, err := PatchID(dir, carrier)
		if err != nil {
			t.Fatal(err)
		}
		r := CompletionReceipt{MergeSHA: discarded, ContentSHA: carrier, PatchID: patch, BaseSHA: base}
		err = r.validateContentBinding(dir)
		if err == nil {
			t.Fatal("a merge that discarded every reviewed hunk was accepted")
		}
		if !strings.Contains(err.Error(), "does not preserve the content of") {
			t.Fatalf("refusal did not name the content rule: %v", err)
		}
	})

	t.Run("merge that substituted different bytes", func(t *testing.T) {
		dir, base, carrier, merge := prReceiptRepo(t)
		run := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		writeFileTest(t, dir, "reviewed.txt", "SUBSTITUTED content\n")
		run("add", "reviewed.txt")
		run("commit", "-q", "--amend", "--no-edit")
		altered := run("rev-parse", "HEAD")
		run("update-ref", "refs/remotes/origin/main", altered)
		if altered == merge {
			t.Fatal("fixture invalid: the amend produced no new commit")
		}
		patch, err := PatchID(dir, carrier)
		if err != nil {
			t.Fatal(err)
		}
		r := CompletionReceipt{MergeSHA: altered, ContentSHA: carrier, PatchID: patch, BaseSHA: base}
		err = r.validateContentBinding(dir)
		if err == nil {
			t.Fatal("a merge whose tree holds different bytes than the reviewed result was accepted")
		}
		if !strings.Contains(err.Error(), "does not preserve the content of") {
			t.Fatalf("refusal did not name the content rule: %v", err)
		}
	})

	t.Run("carrier whose patch does not match the sealed one", func(t *testing.T) {
		dir, base, carrier, merge := prReceiptRepo(t)
		r := CompletionReceipt{MergeSHA: merge, ContentSHA: carrier, PatchID: "0000000000000000000000000000000000000000", BaseSHA: base}
		err := r.validateContentBinding(dir)
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

// iteratedPRRepo builds the shape an iterated lane actually produces: the
// carrier is an INTERMEDIATE commit on the pull request's line, and the same
// path is revised again before the landing.
//
//	root ─── base ────────────────── merge          (main, and origin/main)
//	          └── carrier ── tip ──────┘            (the pull request's line)
//
// discarded selects the adversarial twin of the same shape: main revises the
// path too and the landing is an `ours` merge, so the reviewed hunks are gone
// while the carrier is still an ancestor and the path was still revised later.
// The allowance for a later revision must not admit that.
func iteratedPRRepo(t *testing.T, discarded bool) (dir, baseSHA, carrierSHA, mergeSHA string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(name, body, msg string) string {
		t.Helper()
		writeFileTest(t, dir, name, body)
		run("add", name)
		run("commit", "-q", "-m", msg)
		return run("rev-parse", "HEAD")
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "fixture@example.com")
	run("config", "user.name", "fixture")

	commit("a.txt", "root\n", "root")
	// The reviewed path already exists on main, so `ours` can keep main's
	// bytes at it rather than simply dropping a file.
	baseSHA = commit("shared.txt", "base\n", "reviewed base")

	run("checkout", "-q", "-b", "pr")
	carrierSHA = commit("shared.txt", "reviewed v1\n", "reviewed candidate work")
	tip := commit("shared.txt", "reviewed v2\n", "revise the same path inside the pull request")

	run("checkout", "-q", "main")
	if discarded {
		mainRevision := commit("shared.txt", "main revision\n", "main revises the same path")
		run("merge", "-q", "--no-ff", "-s", "ours", "-m", "Merge pull request #831 (ours)", "pr")
		mergeSHA = run("rev-parse", "HEAD")
		if run("rev-parse", mergeSHA+"^{tree}") != run("rev-parse", mainRevision+"^{tree}") {
			t.Fatal("fixture invalid: the ours merge did not keep main's tree")
		}
	} else {
		run("merge", "-q", "--no-ff", "-m", "Merge pull request #831", "pr")
		mergeSHA = run("rev-parse", "HEAD")
		if run("rev-parse", mergeSHA+":shared.txt") != run("rev-parse", tip+":shared.txt") {
			t.Fatal("fixture invalid: the merge does not hold the reviewed line's last revision")
		}
	}
	run("update-ref", "refs/remotes/origin/main", mergeSHA)

	// Both shapes must be NON-VACUOUS: the carrier's own bytes must differ from
	// the merged tree, or the fast byte-identity path would answer and the case
	// under test would never be reached.
	if run("rev-parse", mergeSHA+":shared.txt") == run("rev-parse", carrierSHA+":shared.txt") {
		t.Fatal("fixture invalid: the merge still holds the carrier's own bytes, so no later revision is under test")
	}
	return dir, baseSHA, carrierSHA, mergeSHA
}

// TestValidateAcceptsALaterRevisionOfTheCarriersOwnPath is the FAC-831
// follow-up regression: requiring the carrier's paths to be byte-identical in
// the merge refused honest landings, because the producer seals the commit
// matching the candidate's LAST patch and that is routinely an intermediate
// commit whose files are revised again inside the same pull request. The
// allowance is narrow, and the second case is what keeps it narrow.
func TestValidateAcceptsALaterRevisionOfTheCarriersOwnPath(t *testing.T) {
	t.Run("the reviewed line revised its own path before the merge", func(t *testing.T) {
		dir, base, carrier, merge := iteratedPRRepo(t, false)
		patch, err := PatchID(dir, carrier)
		if err != nil {
			t.Fatalf("carrier must have a patch id: %v", err)
		}
		r := CompletionReceipt{MergeSHA: merge, ContentSHA: carrier, PatchID: patch, BaseSHA: base}
		if err := r.validateContentBinding(dir); err != nil {
			t.Fatalf("an honest landing whose carrier was revised again inside the pull request was refused: %v", err)
		}
	})

	t.Run("an ours merge on a path the reviewed line also revised", func(t *testing.T) {
		dir, base, carrier, merge := iteratedPRRepo(t, true)
		patch, err := PatchID(dir, carrier)
		if err != nil {
			t.Fatalf("carrier must have a patch id: %v", err)
		}
		r := CompletionReceipt{MergeSHA: merge, ContentSHA: carrier, PatchID: patch, BaseSHA: base}
		err = r.validateContentBinding(dir)
		if err == nil {
			t.Fatal("an ours merge that discarded the reviewed hunks was accepted because the path was revised later")
		}
		if !strings.Contains(err.Error(), "does not preserve the content of") {
			t.Fatalf("refusal did not name the content rule: %v", err)
		}
	})
}
