// Package mergeadmit is the single compiled merge authority for the
// coordinator. Every production merge path resolves through Gate.Admit before
// the merge and Gate.Complete after it; nothing here reads a verdict out of
// prose, a PR comment, an operator's argv, or a shell pipeline's tail.
//
// WHY (FAC-156). Three live incidents, all the same shape — a merge decision
// made by text rather than by a compiled predicate:
//
//   - FAC-149/PR#65: the coordinator grepped PR comments for the first
//     REVIEWER-VERDICT PASS. It matched the receipt for the OLD candidate.
//   - FAC-162/PR#70: the admission probe was `gh pr view | jq | head`. jq and
//     head exit 0 over an empty stream, so a failed `gh` read as success.
//   - FAC-178/PR#72: GitHub rebase-merged, rewriting the candidate SHA. The
//     ancestry check ran as `candidate_ancestor=ok` — a nonzero `git
//     merge-base` exit was captured into a variable and never gated on, so a
//     failed predicate printed success.
//
// The three fixes are structural, not textual:
//
//  1. Probe (probe.go): every live read carries its producer's own exit
//     status, and an empty value is a refusal, never "no findings".
//  2. Prove (this file): merge mode selects the proof. A rewritten SHA has no
//     ancestry to check, so ancestry is not the question to ask of it.
//  3. Gate (admit.go): the sole entry point, returning a structured Decision
//     that callers gate on. Reason is diagnostic text; it is never authority.
package mergeadmit

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Mode is how the forge published the candidate onto the base. It selects the
// compiled proof, because each mode leaves behind different evidence and
// asking the wrong question of the wrong mode is how FAC-178 passed a gate it
// should have failed.
type Mode string

const (
	// ModeMerge covers a merge commit or a fast-forward: the candidate commit
	// object itself survives onto the base, so EXACT ANCESTRY is the proof.
	ModeMerge Mode = "merge"

	// ModeRebase rewrites every commit in the range to a new SHA. There is no
	// ancestry left to check, so the proof is ORDERED PER-COMMIT PATCH
	// IDENTITY plus exact tree identity.
	ModeRebase Mode = "rebase"

	// ModeSquash collapses the whole range into one commit. Per-commit patch
	// ids do not survive it, so the proof is COMBINED-RANGE PATCH IDENTITY
	// plus exact tree identity.
	ModeSquash Mode = "squash"
)

// ParseMode resolves a declared merge mode. An empty or unknown mode is a hard
// error: absence is not consent to guess, and guessing "merge" for a rebase is
// precisely the FAC-178 failure.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeMerge:
		return ModeMerge, nil
	case ModeRebase:
		return ModeRebase, nil
	case ModeSquash:
		return ModeSquash, nil
	case "":
		return "", fmt.Errorf("merge mode is required (one of merge, rebase, squash); an absent mode is not consent to assume ancestry")
	default:
		return "", fmt.Errorf("unknown merge mode %q (want merge, rebase, or squash)", s)
	}
}

// ProofRequest names the exact three points a proof reasons over. All three
// must resolve to real commits; none may be empty.
type ProofRequest struct {
	Mode Mode
	// BaseSHA is the base the candidate was reviewed against and the forge
	// published onto. The proof compares base..candidate with base..landed, so
	// a base that has since been overtaken by unrelated commits makes the
	// ranges differ and the proof refuses — which is the correct answer.
	BaseSHA string
	// CandidateSHA is the reviewed candidate tip.
	CandidateSHA string
	// LandedSHA is the integration tip actually published (origin/main).
	LandedSHA string
}

// Proof is the compiled, durable statement that the reviewed candidate's
// content is what landed. It is produced only by Prove.
type Proof struct {
	Mode         Mode   `json:"mode"`
	BaseSHA      string `json:"base_sha"`
	CandidateSHA string `json:"candidate_sha"`
	LandedSHA    string `json:"landed_sha"`
	// MergeSHA is the commit on the landed history whose patch the completion
	// receipt binds its content check to. For ModeMerge that is the candidate
	// itself (it survived); for a rewrite it is the landed tip.
	MergeSHA string `json:"merge_sha"`
	// PatchID is git patch-id --stable of MergeSHA, the value the completion
	// receipt carries.
	PatchID string `json:"patch_id"`
	// Method names the predicate that actually proved it, so a receipt reader
	// can tell ancestry from content identity without re-deriving it.
	Method string `json:"method"`
}

// Prove establishes, by the predicate the merge mode calls for, that the
// reviewed candidate's content is what is on the landed history.
//
// Every git invocation below is gated on its own exit status. There is no
// pipeline whose tail can mask a failed producer, and no predicate whose
// result is captured into a variable and then printed unconditionally.
func Prove(repoDir string, req ProofRequest) (*Proof, error) {
	mode, err := ParseMode(string(req.Mode))
	if err != nil {
		return nil, err
	}
	base, err := resolveCommit(repoDir, req.BaseSHA, "base")
	if err != nil {
		return nil, err
	}
	candidate, err := resolveCommit(repoDir, req.CandidateSHA, "candidate")
	if err != nil {
		return nil, err
	}
	landed, err := resolveCommit(repoDir, req.LandedSHA, "landed")
	if err != nil {
		return nil, err
	}

	// A candidate that adds nothing to the base has no content to prove
	// landed. This is the FAC-156 empty-branch shape: a branch holding only
	// its worktree anchor merged a zero-line diff and every downstream check
	// passed, because there was nothing there to be wrong.
	candidateCommits, err := rangeCommits(repoDir, base, candidate)
	if err != nil {
		return nil, err
	}
	if len(candidateCommits) == 0 {
		return nil, fmt.Errorf("candidate %s adds no commits over base %s: an empty candidate has no content to prove landed",
			short(candidate), short(base))
	}

	p := &Proof{Mode: mode, BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed}

	switch mode {
	case ModeMerge:
		// The candidate object survived, so ancestry is the whole question.
		// This exit status IS the gate — capturing it and reporting success
		// anyway is the FAC-178 bug.
		if err := runGit(repoDir, "merge-base", "--is-ancestor", candidate, landed); err != nil {
			return nil, fmt.Errorf("merge-mode proof failed: candidate %s is not an ancestor of landed %s",
				short(candidate), short(landed))
		}
		p.MergeSHA = candidate
		p.Method = "exact-ancestry"

	case ModeRebase:
		// The SHAs were rewritten. Ordered per-commit patch identity proves
		// the same changes landed in the same order; tree identity proves the
		// result is byte-for-byte the reviewed tree.
		landedCommits, err := rangeCommits(repoDir, base, landed)
		if err != nil {
			return nil, err
		}
		want, err := patchIDs(repoDir, candidateCommits)
		if err != nil {
			return nil, err
		}
		got, err := patchIDs(repoDir, landedCommits)
		if err != nil {
			return nil, err
		}
		if err := sameOrderedPatches(want, got); err != nil {
			return nil, fmt.Errorf("rebase-mode proof failed: %w", err)
		}
		if err := sameTree(repoDir, candidate, landed); err != nil {
			return nil, fmt.Errorf("rebase-mode proof failed: %w", err)
		}
		p.MergeSHA = landed
		p.Method = "ordered-patch-identity+tree-identity"

	case ModeSquash:
		// One commit replaces the range, so per-commit ids are gone. The
		// combined range diff and the resulting tree are what survive.
		landedCommits, err := rangeCommits(repoDir, base, landed)
		if err != nil {
			return nil, err
		}
		if len(landedCommits) != 1 {
			return nil, fmt.Errorf("squash-mode proof failed: base..landed holds %d commits, not the single squashed commit "+
				"(the base was overtaken by unrelated work, so this range is not the squash)", len(landedCommits))
		}
		want, err := rangePatchID(repoDir, base, candidate)
		if err != nil {
			return nil, err
		}
		got, err := rangePatchID(repoDir, base, landed)
		if err != nil {
			return nil, err
		}
		if want != got {
			return nil, fmt.Errorf("squash-mode proof failed: candidate range carries patch %s, landed range carries %s",
				short(want), short(got))
		}
		if err := sameTree(repoDir, candidate, landed); err != nil {
			return nil, fmt.Errorf("squash-mode proof failed: %w", err)
		}
		p.MergeSHA = landed
		p.Method = "combined-patch-identity+tree-identity"
	}

	// The receipt binds content to MergeSHA's own patch, so compute it here
	// rather than leaving the caller to re-derive (and possibly re-derive it
	// from a different commit than the one that was proved).
	pid, err := commitPatchID(repoDir, p.MergeSHA)
	if err != nil {
		return nil, fmt.Errorf("patch id for proved merge commit %s: %w", short(p.MergeSHA), err)
	}
	p.PatchID = pid
	return p, nil
}

// sameOrderedPatches compares two patch-id sequences positionally. Order is
// part of the claim: the same patches applied in a different order are a
// different history, and for a rebase they are a different result.
func sameOrderedPatches(want, got []string) error {
	if len(want) != len(got) {
		return fmt.Errorf("candidate range holds %d commit(s), landed range holds %d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("commit %d of the range differs: candidate patch %s, landed patch %s",
				i+1, short(want[i]), short(got[i]))
		}
	}
	return nil
}

// sameTree proves the two commits resolve to the identical tree object. This
// is the check that made the FAC-178 rebase provably safe after the fact, and
// it is the one that catches a patch-identical range that nonetheless produced
// a different result (a conflict resolved differently, a dropped commit
// re-added, a stray file left behind).
func sameTree(repoDir, a, b string) error {
	ta, err := gitOut(repoDir, "rev-parse", "--verify", "-q", a+"^{tree}")
	if err != nil {
		return fmt.Errorf("resolve tree of %s: %w", short(a), err)
	}
	tb, err := gitOut(repoDir, "rev-parse", "--verify", "-q", b+"^{tree}")
	if err != nil {
		return fmt.Errorf("resolve tree of %s: %w", short(b), err)
	}
	if ta != tb {
		return fmt.Errorf("tree identity: candidate %s has tree %s, landed %s has tree %s",
			short(a), short(ta), short(b), short(tb))
	}
	return nil
}

// rangeCommits lists base..tip oldest-first. An error is an error; it is never
// flattened into an empty range, because "no commits" and "could not tell" are
// the same value to a length check and only one of them is safe.
func rangeCommits(repoDir, base, tip string) ([]string, error) {
	out, err := gitOut(repoDir, "rev-list", "--reverse", base+".."+tip)
	if err != nil {
		return nil, fmt.Errorf("rev-list %s..%s: %w", short(base), short(tip), err)
	}
	var commits []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			commits = append(commits, s)
		}
	}
	return commits, nil
}

func patchIDs(repoDir string, commits []string) ([]string, error) {
	out := make([]string, 0, len(commits))
	for _, c := range commits {
		pid, err := commitPatchID(repoDir, c)
		if err != nil {
			return nil, fmt.Errorf("patch id for %s: %w", short(c), err)
		}
		out = append(out, pid)
	}
	return out, nil
}

// commitPatchID is the stable patch id of a single commit's diff. An empty
// commit is an error rather than an empty id — an empty id would compare equal
// to another empty id and let two contentless commits "prove" each other.
func commitPatchID(repoDir, sha string) (string, error) {
	diff, err := gitOutBytes(repoDir, "diff-tree", "-p", "--no-color", sha)
	if err != nil {
		return "", fmt.Errorf("git diff-tree: %w", err)
	}
	return stablePatchID(repoDir, diff)
}

// rangePatchID is the stable patch id of the whole base..tip diff, which is
// what a squash collapses to.
func rangePatchID(repoDir, base, tip string) (string, error) {
	diff, err := gitOutBytes(repoDir, "diff", "--no-color", base, tip)
	if err != nil {
		return "", fmt.Errorf("git diff %s %s: %w", short(base), short(tip), err)
	}
	return stablePatchID(repoDir, diff)
}

func stablePatchID(repoDir string, diff []byte) (string, error) {
	if len(bytes.TrimSpace(diff)) == 0 {
		return "", fmt.Errorf("no patch content (empty diff)")
	}
	cmd := exec.Command("git", "patch-id", "--stable")
	cmd.Dir = repoDir
	cmd.Stdin = bytes.NewReader(diff)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git patch-id: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("git patch-id produced no id")
	}
	return fields[0], nil
}

// resolveCommit turns a caller-supplied revision into a full object id, or
// fails. An unresolvable revision never degrades to the empty string.
func resolveCommit(repoDir, rev, role string) (string, error) {
	if strings.TrimSpace(rev) == "" {
		return "", fmt.Errorf("%s revision is required", role)
	}
	out, err := gitOut(repoDir, "rev-parse", "--verify", "-q", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s revision %q does not resolve to a commit in %s", role, rev, repoDir)
	}
	if out == "" {
		return "", fmt.Errorf("%s revision %q resolved to nothing", role, rev)
	}
	return out, nil
}

// -- git helpers: every one of these surfaces the process exit status --

func runGit(repoDir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	return cmd.Run()
}

func gitOut(repoDir string, args ...string) (string, error) {
	out, err := gitOutBytes(repoDir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitOutBytes(repoDir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
