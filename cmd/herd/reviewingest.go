package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/harvestmerge"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// runReviewIngest ports bin/herd-review-ingest: validate reviewer verdict
// artifacts and admit only the well-formed ones to the ledger.
//
// A bad verdict is indistinguishable from a good one once it lands, so every
// refusal happens before the write.
func runReviewIngest() {
	fs := flag.NewFlagSet("review-ingest", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "Validate without writing to the ledger")
	ledgerPath := fs.String("ledger", filepath.Join(".herd", "review-ledger.jsonl"), "Ledger path")
	fs.Parse(os.Args[2:])

	files := fs.Args()
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: herd review-ingest <verdict-artifact>... [--dry-run]")
		os.Exit(2)
	}

	coordinators := map[string]struct{}{}
	for k := range reviewledger.DefaultCoordinators {
		coordinators[k] = struct{}{}
	}
	coordinators["herdforge-orchestrator"] = struct{}{}

	commitExists := func(sha string) bool {
		return exec.Command("git", "rev-parse", "--verify", "-q", sha+"^{commit}").Run() == nil
	}

	diffEmpty := func(sha string) (bool, error) {
		out, err := exec.Command("git", "diff", "origin/main..."+sha).Output()
		if err != nil {
			return false, fmt.Errorf("git diff origin/main...%s: %w", sha[:min(12, len(sha))], err)
		}
		return len(strings.TrimSpace(string(out))) == 0, nil
	}

	var ledger *reviewledger.Ledger
	if !*dryRun {
		var err error
		ledger, err = reviewledger.NewReviewLedger(".", *ledgerPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd review-ingest: open ledger: %v\n", err)
			os.Exit(1)
		}
	}

	admitted, refused := 0, 0
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "REFUSED %s: %v\n", filepath.Base(f), err)
			refused++
			continue
		}
		a := reviewingest.Parse(string(body))
		if err := a.Validate(coordinators, commitExists); err != nil {
			fmt.Fprintf(os.Stderr, "REFUSED %s: %v\n", filepath.Base(f), err)
			refused++
			continue
		}
		if err := a.ValidatePassDiff(diffEmpty); err != nil {
			fmt.Fprintf(os.Stderr, "REFUSED %s: %v\n", filepath.Base(f), err)
			refused++
			continue
		}
		if *dryRun {
			fmt.Printf("WOULD_ADMIT %s verdict=%s reviewer=%s sha=%s\n",
				filepath.Base(f), a.Verdict, a.Reviewer, a.SHA[:12])
			admitted++
			continue
		}
		// Retain the validated artifact inside the repository before writing its
		// ledger row. Reviewer panes use ephemeral /tmp worktrees and cleanup
		// may race ingest; a ledger row pointing at a vanished path is not
		// durable review authority.
		retained, retainErr := retainVerdictArtifact(".", f, a.SHA, a.Reviewer)
		if retainErr != nil {
			fmt.Fprintf(os.Stderr, "herd review-ingest: retain artifact FAILED for %s: %v\n", filepath.Base(f), retainErr)
			os.Exit(1)
		}
		// A reviewer artifact is the durable handoff from the supervisor. Make
		// its exact-SHA admission record idempotently before the verdict row so
		// harvest-merge can prove independent provenance even when the ephemeral
		// launch pane has already been cleaned up.
		gate := "independent"
		if strings.EqualFold(a.ReviewerFamily, "mechanical") || strings.EqualFold(a.BuilderFamily, "mechanical") {
			gate = "mechanical"
		}
		recordOpts := reviewledger.RecordOpts{
			SHA: a.SHA, Branch: a.Branch, BuilderFamily: a.BuilderFamily,
			ReviewerFamily: a.ReviewerFamily, Reviewer: a.Reviewer,
			Artifact: retained, Gate: gate, Task: a.Branch,
		}
		verdictOpts := reviewledger.VerdictOpts{
			SHA:            a.SHA,
			Reviewer:       a.Reviewer,
			Verdict:        reviewledger.Verdict(a.Verdict),
			Artifact:       retained,
			ReviewerFamily: a.ReviewerFamily,
			BuilderFamily:  a.BuilderFamily,
			Branch:         a.Branch,
			CandidateSHA:   a.SHA,
		}
		enqueued, err := ledger.Ingest(reviewledger.IngestOpts{Record: recordOpts, Verdict: verdictOpts})
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd review-ingest: admission/verdict write FAILED for %s: %v\n", filepath.Base(f), err)
			os.Exit(1)
		}
		fmt.Printf("ADMITTED %s verdict=%s reviewer=%s sha=%s enqueued=%v\n",
			filepath.Base(f), a.Verdict, a.Reviewer, a.SHA[:12], enqueued)
		admitted++
	}

	fmt.Printf("herd review-ingest: admitted=%d refused=%d\n", admitted, refused)
	// ANY refusal is a non-zero exit. Exiting 0 on a partial batch let
	// `herd review-ingest *.md && <next step>` proceed with silently rejected
	// verdicts, which is precisely the fail-open this gate exists to prevent
	// (CLAUDE.md invariant 2).
	if refused > 0 {
		os.Exit(1)
	}
}

// retainVerdictArtifact copies a validated verdict into the repo-local review
// inbox using a content-addressed filename. The returned path is relative to
// the repo root so the ledger remains portable across worktrees.
func retainVerdictArtifact(root, source, sha, reviewer string) (string, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(source) == "" || strings.TrimSpace(sha) == "" {
		return "", fmt.Errorf("root, source, and candidate sha are required")
	}
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, in); err != nil {
		return "", fmt.Errorf("hash verdict artifact: %w", err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		return "", fmt.Errorf("rewind verdict artifact: %w", err)
	}
	name := strings.ToLower(strings.TrimSpace(reviewer))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name = strings.Trim(b.String(), "-")
	if name == "" {
		name = "reviewer"
	}
	if len(name) > 32 {
		name = name[:32]
	}
	rel := filepath.Join(".herd", "review", "inbox", fmt.Sprintf("%s-%s-%s.md", strings.ToLower(shortSHA(sha)), name, fmt.Sprintf("%x", digest.Sum(nil))[:16]))
	dst := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("create review inbox: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".verdict-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create retained artifact: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("copy verdict artifact: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("secure retained artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close retained artifact: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", fmt.Errorf("publish retained artifact: %w", err)
	}
	return filepath.ToSlash(rel), nil
}

// runHarvestMerge ports bin/herd-harvest-merge: cherry-pick a lane's unique
// commits onto a FRESH worktree off origin/main, gate them, and leave a
// publishable branch.
//
// Picking onto current main rather than pinning at the lane's older HEAD is
// what makes this safe: conflicts surface locally and the merge-base stays at
// origin/main, so the downstream stale-base gate cannot trip on current work.
//
// FAC-213: --verify-landed runs the single sound "did this merge?" check
// (LandedProof) on the lane's worktree instead of cherry-picking. The
// coordinator uses it after a PR merge to confirm the work is on origin/main.
//
// FAC-379: when LandedProof succeeds for an exact reviewed candidate whose
// equivalent patch already sits on origin/main under a different merge SHA,
// verify-landed also mints/reconciles the sealed task-bound completion
// receipt approve/board-done consume — the same receipt a successful
// merge-complete would write. Binding comes from a recorded merge-admission
// (--ref) or the explicit merge-admit fields.
func runHarvestMerge() {
	fs := flag.NewFlagSet("harvest-merge", flag.ExitOnError)
	branch := fs.String("branch", "", "Lane branch to harvest (required)")
	title := fs.String("title", "", "PR title (required)")
	// FAC-156: --verdict is no longer merge authority. Consent comes from the
	// durable review ledger for the exact branch head; this flag can only
	// REFUSE. The asymmetry is deliberate — an operator saying "don't" is
	// always safe to honour, an operator saying "do" is the human-supplied
	// provenance this card exists to remove.
	verdict := fs.String("verdict", "",
		"Optional operator VETO (FAIL/BLOCKED refuse). PASS is not accepted here: merge consent comes from the review ledger.")
	base := fs.String("base", "origin/main", "Base to harvest onto")
	dryRun := fs.Bool("dry-run", false, "Plan and gate without creating the worktree")
	allowMarkers := fs.Bool("allow-markers", false,
		"Proceed despite conflict markers in the harvested diff (for files whose CONTENT is marker fixtures)")
	verifyLanded := fs.Bool("verify-landed", false,
		"Check whether the lane's work is on origin/main (rebase + empty diff) and mint/reconcile the sealed completion receipt.")
	verifyRef := fs.String("ref", "", "Task ref for --verify-landed receipt reconcile (loads merge-admission or explicit binding flags)")
	verifyTaskID := fs.String("task-id", "", "Provider task id (required with --verify-landed when no merge-admission is on disk)")
	verifyCandidate := fs.String("candidate", "", "Exact reviewed candidate sha (defaults to branch tip before LandedProof)")
	verifyBaseSHA := fs.String("base-sha", "", "Base sha the candidate was reviewed against")
	verifyLease := fs.String("lease", "", "Claim lease token bound into the ledger verdict")
	verifyLeaseGen := fs.Int64("lease-generation", 0, "Claim lease generation bound into the completion receipt")
	verifyPatchID := fs.String("patch-id", "", "Patch identity bound into the ledger verdict")
	verifyAcceptance := fs.String("acceptance-digest", "", "Acceptance digest bound at review time")
	verifyAuthorFamily := fs.String("author-family", "", "Builder model family")
	verifyAuthorIdentity := fs.String("author-identity", "", "Builder session identity")
	verifyProviderRev := fs.String("provider-revision", "", "Board card revision the reviewer bound")

	// Pull the leading positional out BEFORE flag parsing: Go's flag package
	// stops at the first non-flag argument, so `harvest-merge <lane> --branch x`
	// would silently leave --branch unparsed.
	args := os.Args[2:]
	lane := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		lane, args = args[0], args[1:]
	}
	fs.Parse(args)

	if lane == "" || *branch == "" {
		fmt.Fprintln(os.Stderr, "usage: herd harvest-merge <lane> --branch <branch> --title <t> [--verdict PASS]")
		fmt.Fprintln(os.Stderr, "       herd harvest-merge <lane> --branch <branch> --verify-landed --ref <FAC-x> [...]")
		os.Exit(2)
	}

	// FAC-213 + FAC-379: --verify-landed is the post-merge "did this merge?"
	// check, then the sealed completion-receipt reconcile for approve.
	if *verifyLanded {
		binding := verifyLandedBinding{
			Ref: *verifyRef, TaskID: *verifyTaskID, Candidate: *verifyCandidate,
			BaseSHA: *verifyBaseSHA, Lease: *verifyLease, LeaseGeneration: *verifyLeaseGen,
			PatchID: *verifyPatchID, AcceptanceDigest: *verifyAcceptance,
			AuthorFamily: *verifyAuthorFamily, AuthorIdentity: *verifyAuthorIdentity,
			ProviderRevision: *verifyProviderRev,
		}
		if err := runHarvestVerifyLanded(*branch, binding); err != nil {
			fmt.Fprintf(os.Stderr, "herd harvest-merge: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *title == "" {
		fmt.Fprintln(os.Stderr, "usage: herd harvest-merge <lane> --branch <branch> --title <t> [--verdict PASS]")
		fmt.Fprintln(os.Stderr, "       (or use --verify-landed to check if a merge landed)")
		os.Exit(2)
	}

	cherry, err := exec.Command("git", "cherry", *base, *branch).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: git cherry %s %s: %v\n", *base, *branch, err)
		os.Exit(1)
	}
	commits := harvestmerge.UniqueCommits(string(cherry))

	// A failed rev-parse previously left plan.SHA empty and the harvest ran on
	// against a candidate nobody could name — the same swallowed-exit-status
	// shape this card is about.
	head, headErr := exec.Command("git", "rev-parse", *branch).Output()
	if headErr != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: cannot resolve %s: %v\n", *branch, headErr)
		os.Exit(1)
	}
	sha := strings.TrimSpace(string(head))
	if sha == "" {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: %s resolved to no commit\n", *branch)
		os.Exit(1)
	}

	ledgerVerdict, vErr := harvestMergeVerdict(sha, *verdict)
	if vErr != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: %v\n", vErr)
		os.Exit(1)
	}

	// Content evidence for the empty-diff gate. A commit COUNT is not a content
	// check: PR #151 merged 0 additions / 0 deletions / 0 files because the
	// branch carried only its anchor commit. Computed here rather than passed in
	// so the gate cannot be bypassed by a caller that simply omits it.
	diffstatOut, diffErr := exec.Command("git", "diff", "--shortstat", *base+"..."+*branch).Output()
	if diffErr != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: git diff --shortstat %s...%s: %v\n", *base, *branch, diffErr)
		os.Exit(1)
	}
	diffstat := strings.TrimSpace(string(diffstatOut))

	plan := harvestmerge.Plan{
		Diffstat: diffstat,
		Lane:     lane, Branch: *branch, Title: *title,
		SHA:     sha,
		Verdict: ledgerVerdict,
		Commits: commits,
	}
	plan.TempBranch = harvestmerge.TempBranchName(lane, plan.SHA)

	if err := plan.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd harvest-merge: %s has %d unique commit(s) onto %s\n", *branch, len(commits), *base)
	if *dryRun {
		fmt.Printf("herd harvest-merge: DRY_RUN temp_branch=%s\n", plan.TempBranch)
		return
	}

	dir := filepath.Join(".herd", "worktrees", "harvest-"+filepath.Base(plan.TempBranch))

	// The harvest body is a named function returning an error rather than
	// calling os.Exit inline. os.Exit does NOT run deferred functions, so a
	// defer here plus os.Exit on the failure paths would skip cleanup on
	// exactly the conflict and marker paths where a leaked worktree matters
	// most — the very trap chainseer's EXIT trap exists to prevent,
	// reintroduced. Extraction also makes the body testable without
	// subprocess orchestration.
	err = harvestBody(dir, *base, plan.TempBranch, commits, *allowMarkers)

	// Cleanup runs on the FAILURE path only — the worktree is deliberately kept
	// on success so the coordinator can push from it.
	cleanup := func() {
		exec.Command("git", "worktree", "remove", "--force", "--", dir).Run()
		exec.Command("git", "branch", "-D", "--", plan.TempBranch).Run()
	}

	if err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "herd harvest-merge: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("herd harvest-merge: harvested %d commit(s) clean onto %s at %s\n", len(commits), *base, dir)
	fmt.Println("herd harvest-merge: gates passed. Publish and merge is the coordinator's explicit action:")
	fmt.Printf("  git push -u origin %s && gh pr create --title %q\n", plan.TempBranch, *title)
	// The worktree is intentionally KEPT on success: the coordinator pushes
	// from it. Cleanup on success is the caller's, after publishing.
}

// harvestBody cherry-picks commits into a fresh worktree off base and gates
// the result. It returns an error rather than calling os.Exit so the caller
// can run cleanup via a deferred function — os.Exit skips defers, which would
// leak the worktree on exactly the failure paths where it matters most.
func harvestBody(dir, base, tempBranch string, commits []string, allowMarkers bool) error {
	if out, addErr := exec.Command("git", "worktree", "add", "-B", tempBranch, "--", dir, base).CombinedOutput(); addErr != nil {
		return fmt.Errorf("worktree add: %v: %s", addErr, out)
	}
	for _, c := range commits {
		out, pickErr := exec.Command("git", "-C", dir, "cherry-pick", "--", c).CombinedOutput()
		if pickErr == nil {
			continue
		}
		// A commit whose content is already upstream applies to nothing and
		// cherry-pick stops with "nothing to commit". That is NOT a
		// conflict — the fleet opens every worktree with a reap-safe anchor
		// commit (FAC-106) that is frequently redundant, so aborting here
		// made every fleet branch unharvestable. Distinguish by asking git
		// whether the tree is actually clean rather than by matching text.
		status, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
		if len(strings.TrimSpace(string(status))) == 0 {
			if skipOut, skipErr := exec.Command("git", "-C", dir, "cherry-pick", "--skip").CombinedOutput(); skipErr != nil {
				exec.Command("git", "-C", dir, "cherry-pick", "--abort").Run()
				return fmt.Errorf("redundant commit %s could not be skipped:\n%s", c[:min(9, len(c))], skipOut)
			}
			fmt.Fprintf(os.Stderr, "herd harvest-merge: skipped %s (already upstream, no content to apply)\n", c[:min(9, len(c))])
			continue
		}
		exec.Command("git", "-C", dir, "cherry-pick", "--abort").Run()
		return fmt.Errorf("cherry-pick %s conflicted, aborting before any PR:\n%s", c[:min(9, len(c))], out)
	}
	// Hard stage gate: a marker in the harvested ADDED lines means the pick
	// produced a structurally broken diff, and once it is in a PR body
	// nobody re-reads it.
	// A hard gate must never pass because its input failed to load: an
	// empty diff from a failed git call would yield zero markers.
	staged, diffErr := exec.Command("git", "-C", dir, "diff", base+"...HEAD").Output()
	if diffErr != nil {
		return fmt.Errorf("cannot read the harvested diff to gate it: %w", diffErr)
	}
	if len(strings.TrimSpace(string(staged))) == 0 {
		return fmt.Errorf("REFUSED — the harvested diff against %s is empty; "+
			"a merge that changes no bytes is not a completed ticket (FAC-212)", base)
	}
	if markers := harvestmerge.ConflictMarkers(string(staged)); len(markers) > 0 {
		if !allowMarkers {
			return fmt.Errorf("REFUSED — %d conflict marker(s) in the harvested diff:\n  %s\n"+
				"  If these are fixture CONTENT rather than a broken pick (e.g. a marker parser's\n"+
				"  own test data), re-run with --allow-markers. There was previously no escape\n"+
				"  hatch at all, which made pkg/conflict/conflict_test.go unharvestable by this tool.",
				len(markers), strings.Join(markers, "\n  "))
		}
		// Loud and durable in the output: an override on a merge gate must
		// never be quiet, and the operator must own it explicitly.
		fmt.Fprintf(os.Stderr, "herd harvest-merge: WARNING --allow-markers OVERRIDE: proceeding despite %d conflict marker(s):\n  %s\n",
			len(markers), strings.Join(markers, "\n  "))
	}
	return nil
}

// harvestMergeVerdict resolves the merge verdict for an exact candidate sha
// from the durable review ledger. This is the FAC-156 rule at this call site:
// merge CONSENT is only ever read out of compiled, structured, exact-sha
// ledger state, never out of an operator's argv.
//
// operatorVeto is honoured in one direction only. FAIL or BLOCKED refuses even
// when the ledger would admit — a human who says stop is always obeyed. PASS
// is rejected as an input, because that is the human-supplied provenance the
// card removes; a coordinator who believes the work passed must get that
// verdict into the ledger through `herd review-ingest`.
func harvestMergeVerdict(sha, operatorVeto string) (harvestmerge.Verdict, error) {
	switch v := harvestmerge.Verdict(strings.ToUpper(strings.TrimSpace(operatorVeto))); v {
	case "":
		// No operator opinion; the ledger decides on its own.
	case harvestmerge.Verdict("FAIL"), harvestmerge.Verdict("BLOCKED"):
		return v, fmt.Errorf("operator veto %s refuses the merge", v)
	default:
		return "", fmt.Errorf("--verdict %s is not accepted: merge consent comes from the review ledger, "+
			"not the command line. Land the reviewer's verdict with `herd review-ingest` and re-run. "+
			"Only FAIL/BLOCKED may be supplied here, and only to refuse", v)
	}

	ledger, err := reviewledger.NewReviewLedger(".", filepath.Join(".herd", "review-ledger.jsonl"))
	if err != nil {
		return "", fmt.Errorf("open review ledger: %w", err)
	}
	// Empty builder family is the STRICT form: only a cross-family PASS with a
	// provable launch record counts, and any unsuperseded FAIL/BLOCKED or an
	// already-consumed admission refuses.
	eligible, err := ledger.Eligible(sha, "")
	if err != nil {
		return "", fmt.Errorf("review ledger refuses %s: %w", sha[:min(12, len(sha))], err)
	}
	if !eligible {
		return "", fmt.Errorf("review ledger holds no admissible independent PASS for exact candidate %s; "+
			"a verdict for another sha, a superseded verdict, or an already-consumed admission is not consent",
			sha[:min(12, len(sha))])
	}
	return harvestmerge.Verdict("PASS"), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// verifyLandedBinding carries the task-bound fields FAC-379 needs to mint the
// sealed completion receipt after LandedProof. Prefer a recorded merge-admission
// for --ref; otherwise every field must be supplied explicitly.
type verifyLandedBinding struct {
	Ref, TaskID, Candidate, BaseSHA  string
	Lease, PatchID, AcceptanceDigest string
	AuthorFamily, AuthorIdentity     string
	ProviderRevision                 string
	LeaseGeneration                  int64
}

// runHarvestVerifyLanded proves the branch content is on origin/main, then
// mints/reconciles the sealed completion receipt through mergeadmit so
// approve/board-done have the same closing authority as a normal harvest.
func runHarvestVerifyLanded(branch string, binding verifyLandedBinding) error {
	wtDir := worktreeForBranch(branch)
	if wtDir == "" {
		return fmt.Errorf("no worktree found for branch %s", branch)
	}

	// Capture the reviewed tip BEFORE LandedProof rebases the worktree onto
	// origin/main (after which HEAD is no longer the candidate object).
	candidate := strings.TrimSpace(binding.Candidate)
	if candidate == "" {
		out, err := exec.Command("git", "-C", wtDir, "rev-parse", "HEAD").Output()
		if err != nil {
			return fmt.Errorf("resolve candidate tip on %s: %w", branch, err)
		}
		candidate = strings.TrimSpace(string(out))
	}
	if candidate == "" {
		return fmt.Errorf("candidate sha is required for --verify-landed receipt reconcile")
	}

	if err := hsync.LandedProof(wtDir); err != nil {
		return fmt.Errorf("NOT LANDED — %v", err)
	}
	fmt.Printf("herd harvest-merge: LANDED — %s worktree is on origin/main (rebase + empty diff)\n", branch)

	req, err := resolveVerifyLandedRequest(binding, candidate)
	if err != nil {
		return err
	}

	gate, err := buildMergeGate(req.Ref, req.TaskID, 0)
	if err != nil {
		return fmt.Errorf("receipt reconcile: %w", err)
	}
	receipt, err := gate.ReconcileLanded(req)
	if err != nil {
		return fmt.Errorf("receipt reconcile: %w", err)
	}
	fmt.Printf("herd harvest-merge: RECEIPT — %s candidate %s landed as %s\n  receipt %s at %s\n",
		receipt.TaskRef, shortSHA12(receipt.CandidateSHA), shortSHA12(receipt.MergeSHA),
		shortSHA12(receipt.Digest), hsync.ReceiptPath(".", receipt.TaskRef))
	fmt.Println("  close the card with: herd approve " + receipt.TaskRef)
	return nil
}

// resolveVerifyLandedRequest prefers a durable merge-admission record for the
// ref (the same handoff merge-complete consumes). When none exists it requires
// the full explicit binding — never invents lease, patch, or family provenance.
func resolveVerifyLandedRequest(binding verifyLandedBinding, candidate string) (mergeadmit.Request, error) {
	ref := strings.TrimSpace(binding.Ref)
	if ref == "" {
		return mergeadmit.Request{}, fmt.Errorf("NOT RECONCILED — --ref is required to mint the sealed completion receipt\n" +
			"durable action: re-run with --verify-landed --ref <FAC-x> after `herd merge-admit`, or supply the full binding flags\n" +
			"(--task-id --base-sha --lease --lease-generation --patch-id --acceptance-digest --author-family --author-identity --provider-revision)")
	}

	if rec, err := readAdmissionRecord(".", ref); err == nil {
		req := rec.Request
		if strings.TrimSpace(req.CandidateSHA) == "" {
			req.CandidateSHA = candidate
		}
		if strings.TrimSpace(binding.Candidate) != "" {
			req.CandidateSHA = strings.TrimSpace(binding.Candidate)
		}
		return req, nil
	}

	req := mergeadmit.Request{
		Ref: ref, TaskID: binding.TaskID, ProviderRevision: binding.ProviderRevision,
		AcceptanceDigest: binding.AcceptanceDigest, CandidateSHA: candidate,
		BaseSHA: binding.BaseSHA, Lease: binding.Lease, LeaseGeneration: binding.LeaseGeneration,
		PatchURL: binding.PatchID, AuthorFamily: binding.AuthorFamily,
		AuthorIdentity: binding.AuthorIdentity, Mode: mergeadmit.ModeRebase,
	}
	for _, f := range []struct{ name, val string }{
		{"task-id", req.TaskID},
		{"base-sha", req.BaseSHA},
		{"lease", req.Lease},
		{"patch-id", req.PatchURL},
		{"acceptance-digest", req.AcceptanceDigest},
		{"author-family", req.AuthorFamily},
		{"author-identity", req.AuthorIdentity},
		{"provider-revision", req.ProviderRevision},
	} {
		if strings.TrimSpace(f.val) == "" {
			return mergeadmit.Request{}, fmt.Errorf("NOT RECONCILED — no merge-admission at %s and --%s is missing\n"+
				"durable action: run `herd merge-admit --ref %s ...` before merge, or re-run --verify-landed with the full binding flags",
				admissionRecordPath(".", ref), f.name, ref)
		}
	}
	if req.LeaseGeneration <= 0 {
		return mergeadmit.Request{}, fmt.Errorf("NOT RECONCILED — no merge-admission at %s and --lease-generation is missing/invalid\n"+
			"durable action: re-run --verify-landed with --lease-generation <n>", admissionRecordPath(".", ref))
	}
	return req, nil
}

// worktreeForBranch finds the worktree directory whose checked-out branch
// matches branch. Returns "" when no worktree holds the branch. Used by
// harvest-merge --verify-landed to locate the lane's worktree for LandedProof.
func worktreeForBranch(branch string) string {
	out, err := exec.Command("git", "worktree", "list", "--porcelain").Output()
	if err != nil {
		return ""
	}
	var dir string
	for _, ln := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(ln, "worktree "):
			dir = strings.TrimSpace(strings.TrimPrefix(ln, "worktree "))
		case strings.HasPrefix(ln, "branch "):
			b := strings.TrimSpace(strings.TrimPrefix(ln, "branch "))
			if b == "refs/heads/"+branch || b == branch {
				return dir
			}
		}
	}
	return ""
}
