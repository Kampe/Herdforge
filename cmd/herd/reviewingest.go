package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/committime"
	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/harvestmerge"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/reviewack"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/reviewroot"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// runReviewIngest ports bin/herd-review-ingest: validate reviewer verdict
// artifacts and admit only the well-formed ones to the ledger.
//
// A bad verdict is indistinguishable from a good one once it lands, so every
// refusal happens before the write.
func runReviewIngest() {
	repoRoot, err := filepath.Abs(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd review-ingest: resolve repository root: %v\n", err)
		os.Exit(1)
	}
	// FAC-625: the receipt log must be read from the worktree-INVARIANT project
	// root, never from the process cwd. Ingest run from a lane worktree has its
	// own (empty) .herd/launch-receipts.jsonl, so a cwd-relative read found no
	// receipt reaching the candidate and recorded provenance_unrecorded on a
	// candidate whose provenance existed all along -- at the project root, one
	// hop away. HERD_ROOT names the lane, not the project, so it must not be
	// trusted here; gitroot.ProjectRoot is the one function in the tree that
	// answers this without depending on which worktree asked.
	projectRoot, _, err := gitroot.ProjectRoot(context.Background(), repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd review-ingest: resolve project root: %v\n", err)
		os.Exit(1)
	}
	receiptPath := launch.ReceiptPathFor(projectRoot)
	parsed, err := parseReviewIngestArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd review-ingest: %v\n", err)
		os.Exit(2)
	}

	// Resolve the review corpus ONCE, before any branch, and say which one it
	// is. A review tool that does not name its corpus makes "ingested"
	// unattributable, which is exactly how two roots diverged unnoticed.
	reviewRoot := resolvedReviewRoot(".")
	if !parsed.asJSON {
		fmt.Println("herd review-ingest: " + reviewRoot.Paths.Describe())
	}

	files := parsed.files
	if parsed.audit {
		if len(files) != 0 || parsed.dryRun {
			fmt.Fprintln(os.Stderr, "usage: herd review-ingest --audit [--audit-root <dir>]")
			os.Exit(2)
		}
		ledger, err := reviewledger.NewReviewLedger(".", parsed.ledgerPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd review-ingest: open ledger: %v\n", err)
			os.Exit(1)
		}
		rows, err := ledger.AllRows()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd review-ingest: read ledger for audit: %v\n", err)
			os.Exit(1)
		}
		findings, err := reviewingest.AuditIngested(parsed.auditRoot, rows)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd review-ingest: audit failed: %v\n", err)
			os.Exit(1)
		}
		for _, finding := range findings {
			fmt.Printf("STRANDED path=%s sha=%s reason=%s\n", finding.Path, finding.SHA, finding.Reason)
		}
		fmt.Printf("herd review-ingest: audit findings=%d\n", len(findings))
		if len(findings) > 0 {
			os.Exit(1)
		}
		return
	}
	if len(files) == 0 {
		// An empty --sweep is a CLEAN INBOX, which is the healthy steady state and
		// the whole point of running it on a schedule. Reporting it as a usage
		// error with exit 2 would make every quiet run look like a broken command
		// and train the operator (or a cron) to ignore the one signal that says
		// the queue is drained.
		if parsed.sweep {
			fmt.Println("herd review-ingest: sweep found no uningested verdicts (inbox is drained)")
			return
		}
		fmt.Fprintln(os.Stderr, "usage: herd review-ingest (<verdict-artifact>... | --sweep) [--dry-run]")
		os.Exit(2)
	}

	coordinators := map[string]struct{}{}
	for k := range reviewledger.DefaultCoordinators {
		coordinators[k] = struct{}{}
	}
	coordinators["herdforge-orchestrator"] = struct{}{}

	commitExists := func(sha string) bool {
		cmd := exec.Command("git", "rev-parse", "--verify", "-q", sha+"^{commit}")
		cmd.Dir = repoRoot
		return cmd.Run() == nil
	}

	diffEmpty := func(sha string) (bool, error) {
		cmd := exec.Command("git", "diff", "origin/main..."+sha)
		cmd.Dir = repoRoot
		out, err := cmd.Output()
		if err != nil {
			return false, fmt.Errorf("git diff origin/main...%s: %w", sha[:min(12, len(sha))], err)
		}
		return len(strings.TrimSpace(string(out))) == 0, nil
	}

	var ledger reviewIngestLedger
	if parsed.dryRun {
		ledger, err = reviewledger.NewReadOnlyReviewLedger(".", parsed.ledgerPath)
	} else {
		ledger, err = reviewledger.NewReviewLedger(".", parsed.ledgerPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd review-ingest: open ledger: %v\n", err)
		os.Exit(1)
	}

	emit := &reviewIngestEmitter{asJSON: parsed.asJSON}
	admitted, refused := 0, 0
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			emit.refused(f, err)
			refused++
			continue
		}
		a := reviewingest.Parse(string(body))
		// FAC-620: reconcile the CLAIMED builder family against recorded launch
		// provenance BEFORE Validate and before anything reaches the ledger.
		//
		// A previous attempt added this reconciliation and never called it from
		// here -- it existed only in its own package and test, so builder family
		// still arrived as whatever a reviewer typed. That is the helper-only
		// defect this card has now been FAILed for twice; the call belongs on
		// the shipped path or it does nothing.
		//
		// Only a receipt whose branch git-REACHES this exact SHA counts. Branch
		// text alone is not evidence: branches are reused, relaunched and
		// rebased, and a confidently wrong family is worse than none because
		// independence is computed against it.
		proven, err := reviewingest.ReconcileBuilderFamilyForSHA(&a, receiptPath, a.SHA,
			commitCreationTime(a.SHA), branchReachesSHA)
		if err != nil {
			emit.refused(f, err)
			refused++
			continue
		}
		if err := a.Validate(coordinators, commitExists); err != nil {
			emit.refused(f, err)
			refused++
			continue
		}
		if err := a.ValidatePassDiff(diffEmpty); err != nil {
			emit.refused(f, err)
			refused++
			continue
		}
		// FAC-620: an asserted builder-family with no launch receipt reaching the
		// commit and no ledger record proving it is DOWNGRADED to unrecorded, so
		// it cannot be used to compute reviewer/builder independence. Before the
		// gate assignment below, which is what routes it to provenance-unrecorded.
		//
		// FAC-625: skipped when the reconciliation above already PROVED the
		// family from a receipt reaching this exact SHA. The ledger record this
		// gate would check for is created BY INGESTING A VERDICT FOR THIS SHA, so
		// running it unconditionally downgraded every first review of every
		// candidate -- including ones this card had already proven -- to
		// unrecorded.
		if !proven {
			if err := reviewingest.RequireCorroboratedFamily(&a, a.SHA, reviewledger.FamilyUnrecorded,
				ledger.ProvenBuilderFamily, artifactIsCandidAboutFamily); err != nil {
				emit.refused(f, err)
				refused++
				continue
			}
		}
		artifactName := reviewingest.RetainedArtifactName(a.SHA, a.Reviewer, body)
		gate := "independent"
		if strings.EqualFold(a.ReviewerFamily, "mechanical") || strings.EqualFold(a.BuilderFamily, "mechanical") {
			gate = "mechanical"
		}
		// FAC-628: FAC-627 taught the LEDGER to accept honestly-unrecorded
		// provenance, but nothing taught INGEST to route a verdict there, so 69
		// completed reviews were still refused with `unknown builder family`. Same
		// shape as FAC-604, where the human summary was fixed and the JSON surface
		// left reporting a false green: the write path was fixed and the read path
		// was not.
		//
		// Reviewers write what they can prove. On this fleet that is genuinely
		// nothing -- commits are authored under the shared human identity with no
		// trailers -- so they write "unknown", or prose explaining why. Those
		// reviews are real and must not be destroyed for being candid.
		if family, honest := honestlyUnrecordedFamily(a.BuilderFamily); honest {
			gate = reviewledger.GateProvenanceUnrecorded
			a.BuilderFamily = family
		}
		// FAC-657: the record and verdict rows must name the SAME task, because
		// Ledger.Admit compares them for equality.
		//
		// They were written from different sources: record.Task from the BRANCH
		// and verdict.Task from the CARD REF. Measured on the live ledger, 0 of
		// 1027 SHAs had them equal, and 726 verdict rows had none at all. The
		// comparison could therefore never succeed, which is one of the four
		// reasons harvest admission had never admitted a candidate.
		//
		// FAC-578: the card ref is the only closable task identity. Falling back
		// to the branch made both rows agree (FAC-657) while filling the live
		// ledger with standing/* and wt/* names board-done can never close. Both
		// rows still share one identity; that identity is now card-ref or empty,
		// and empty is refused below by name rather than recorded as a branch.
		// Validate the RAW value, before collapse.
		//
		// Review finding on 827601fadb70: ingestTaskIdentityFor collapses an
		// invalid standing/* or wt/* ref to "" via CloseableCardRef, so
		// RequireCloseableCardRef received an already-empty string and always
		// took its "task is empty" branch. The message that NAMES the offending
		// value was unreachable from the shipped path -- which is the entire
		// operator diagnostic this change exists to provide.
		//
		// The existing bad-value tests call RequireCloseableCardRef directly
		// with the raw ref, so they passed while the shipped path could not
		// produce that message. They were vacuous for the behaviour under test:
		// a test that exercises the validator instead of the path is testing
		// that the validator works, which was never in doubt.
		if a.Verdict != "RETIRED" {
			// Validate the RAW header, not the normalized one. TaskRef is ""
			// for any non-card value, so passing it here reports "empty" and
			// loses the offending ref -- the defect this gate exists to avoid.
			rawTask := a.RawTaskRef
			if rawTask == "" {
				rawTask = a.TaskRef
			}
			if err := reviewledger.RequireCloseableCardRef(rawTask, "artifact "+filepath.Base(f)+" task"); err != nil {
				emit.refused(f, err)
				refused++
				continue
			}
		}
		taskIdentity := ingestTaskIdentityFor(a.TaskRef, a.Branch)
		recordOpts := reviewledger.RecordOpts{
			SHA: a.SHA, Branch: a.Branch, BuilderFamily: a.BuilderFamily,
			ReviewerFamily: a.ReviewerFamily, Reviewer: a.Reviewer,
			Artifact: artifactName, Gate: gate, Task: taskIdentity,
		}
		verdictOpts := reviewledger.VerdictOpts{
			SHA: a.SHA, Reviewer: a.Reviewer, Verdict: reviewledger.Verdict(a.Verdict),
			Artifact: artifactName, ReviewerFamily: a.ReviewerFamily,
			BuilderFamily: a.BuilderFamily, Branch: a.Branch,
			// FAC-578: without the card ref a verdict row carries only
			// sha+verdict, so no verdict can be tied back to a card and a
			// corrupted board cannot be rebuilt from review history. Empty when
			// the artifact declares no card — unattributed beats misattributed.
			Task: taskIdentity,
			// FAC-658: the last of Admit's four bindings. Digests ONLY the
			// verification the reviewer states it ran, so a reviewer that records
			// nothing gets no digest and stays inadmissible -- which is correct,
			// not a bug. Synthesising one would certify verification that never
			// happened.
			VfyDigest:    a.VerificationDigest(),
			CandidateSHA: a.SHA, RetryOf: a.RetryOf,
		}
		opts := reviewledger.IngestOpts{Record: recordOpts, Verdict: verdictOpts}
		if a.Verdict == "RETIRED" {
			opts = reviewledger.IngestOpts{Retired: &reviewledger.RetireOpts{
				SHA: a.SHA, Branch: a.Branch, Authority: a.Authority, Reason: a.Body,
				Artifact: artifactName,
			}}
		}
		decision, err := reviewIngestAdmissionDecision(ledger, opts, f, artifactName)
		if err != nil {
			// FAC-580: refuse the ARTIFACT, not the batch.
			//
			// This exited the process, so a single malformed artifact aborted
			// the whole run and nothing after it was ingested. One file
			// declaring an unprovable builder family stalled 99 good verdicts
			// behind it, which is invisible from the outside: the command
			// reports a failure for one name and silently never reaches the
			// rest.
			//
			// Still fail-closed. The bad artifact is refused, not admitted, and
			// the run exits non-zero at the end because refused > 0 — the same
			// guarantee, without letting one file hold up a backlog.
			emit.refused(f, err)
			refused++
			continue
		}
		base := reviewIngestOutcome{
			Artifact: filepath.Base(f), Path: f, SHA: a.SHA, Branch: a.Branch,
			Reviewer: a.Reviewer, ReviewerFamily: a.ReviewerFamily,
			BuilderFamily: a.BuilderFamily, Verdict: a.Verdict, Gate: gate,
		}
		if parsed.dryRun {
			if decision == reviewIngestSkipDuplicate {
				o := base
				o.Disposition, o.Reason = "would_skip", "duplicate"
				emit.record(o, fmt.Sprintf("WOULD_SKIP %s reason=duplicate verdict=%s reviewer=%s sha=%s\n",
					filepath.Base(f), a.Verdict, a.Reviewer, a.SHA[:12]), false)
			} else {
				o := base
				o.Disposition = "would_admit"
				emit.record(o, fmt.Sprintf("WOULD_ADMIT %s verdict=%s reviewer=%s sha=%s\n",
					filepath.Base(f), a.Verdict, a.Reviewer, a.SHA[:12]), false)
			}
			admitted++
			continue
		}
		if decision == reviewIngestSkipDuplicate {
			o := base
			o.Disposition, o.Enqueued = dispositionDuplicate, boolPtr(false)
			emit.record(o, fmt.Sprintf("DUPLICATE %s verdict=%s reviewer=%s sha=%s enqueued=false\n",
				filepath.Base(f), a.Verdict, a.Reviewer, a.SHA[:12]), false)
			admitted++
			continue
		}
		// Retain the validated artifact inside the repository before writing its
		// ledger row. Reviewer panes use ephemeral /tmp worktrees and cleanup
		// may race ingest; a ledger row pointing at a vanished path is not
		// durable review authority. Native review-ingest is the only PASS
		// admission path and prints ADMITTED only after this retain (FAC-373).
		// FAC-572: retain into the CANONICAL project root, not the cwd. Passing
		// "." here is what let a supervisor in a worktree write its artifacts
		// into a corpus nobody else read: 255 artifacts in one root, 63 in the
		// other, and the ledger pointing at whichever the writer happened to
		// resolve.
		retained, retainErr := reviewingest.RetainArtifact(reviewRoot.RepoRoot, f, a.SHA, a.Reviewer)
		if retainErr != nil {
			// FAC-580: per-artifact, not per-batch. See the admission refusal
			// above: aborting here stalled every later verdict behind one file.
			emit.refused(f, fmt.Errorf("retain artifact: %w", retainErr))
			refused++
			continue
		}
		if a.Verdict == "RETIRED" {
			if _, err := ledger.Ingest(reviewledger.IngestOpts{Retired: &reviewledger.RetireOpts{
				SHA: a.SHA, Branch: a.Branch, Authority: a.Authority,
				Reason: a.Body, Artifact: retained,
			}}); err != nil {
				emit.refused(f, fmt.Errorf("retirement write: %w", err))
				refused++
				continue
			}
			o := base
			o.Disposition, o.Authority = "retired", a.Authority
			emit.record(o, fmt.Sprintf("RETIRED %s authority=%s sha=%s\n", filepath.Base(f), a.Authority, a.SHA[:12]), false)
			admitted++
			continue
		}
		// A reviewer artifact is the durable handoff from the supervisor. Make
		// its exact-SHA admission record idempotently before the verdict row so
		// harvest-merge can prove independent provenance even when the ephemeral
		// launch pane has already been cleaned up.
		recordOpts.Artifact = retained
		verdictOpts.Artifact = retained
		enqueued, err := admitVerdictAndMove(ledger, reviewledger.IngestOpts{Record: recordOpts, Verdict: verdictOpts}, f, artifactName)
		if err != nil {
			// A genuinely broken ledger will refuse EVERY artifact loudly,
			// which is strictly more informative than truncating the batch at
			// the first one.
			emit.refused(f, fmt.Errorf("admission/verdict write: %w", err))
			refused++
			continue
		}
		if ingestDisposition(enqueued, a.Verdict) == dispositionDuplicate {
			o := base
			o.Disposition, o.Enqueued = dispositionDuplicate, boolPtr(false)
			emit.record(o, fmt.Sprintf("DUPLICATE %s verdict=%s reviewer=%s sha=%s enqueued=false\n",
				filepath.Base(f), a.Verdict, a.Reviewer, a.SHA[:12]), false)
		} else {
			o := base
			o.Disposition, o.Enqueued = dispositionAdmitted, boolPtr(enqueued)
			emit.record(o, fmt.Sprintf("ADMITTED %s verdict=%s reviewer=%s sha=%s enqueued=%v\n",
				filepath.Base(f), a.Verdict, a.Reviewer, a.SHA[:12], enqueued), false)
			// FAC-586: durable ack that canonical ingest admitted this artifact.
			// Remote-ref transport and ledger admission are distinct; review hosts
			// must not retire residents on transport alone.
			if ackErr := reviewack.Emit(reviewRoot.RepoRoot, reviewack.Ack{
				SHA: a.SHA, Reviewer: a.Reviewer, ArtifactDigest: reviewack.ArtifactDigest(body),
				LaunchIdentity: a.Reviewer,
			}); ackErr != nil {
				fmt.Fprintf(os.Stderr, "review-ingest: ADMITTED %s but ingest ack emit failed: %v\n", a.SHA[:12], ackErr)
			}
			postReviewCompleteCallback(a.SHA, a.Branch, a.Reviewer, a.Verdict)
			reclaimReviewPoolSlotFor(a.SHA)
		}
		admitted++
	}

	if err := emit.summary(admitted, refused); err != nil {
		fmt.Fprintf(os.Stderr, "herd review-ingest: encode: %v\n", err)
		os.Exit(1)
	}
	// ANY refusal is a non-zero exit. Exiting 0 on a partial batch let
	// `herd review-ingest *.md && <next step>` proceed with silently rejected
	// verdicts, which is precisely the fail-open this gate exists to prevent
	// (CLAUDE.md invariant 2).
	if refused > 0 {
		os.Exit(1)
	}
}

type reviewIngestArgs struct {
	dryRun bool
	// sweep discovers the uningested artifacts itself instead of requiring the
	// caller to enumerate them (FAC-606).
	sweep      bool
	audit      bool
	auditRoot  string
	ledgerPath string
	files      []string
	// asJSON emits structured outcomes instead of prose (FAC-556).
	asJSON bool
}

// parseReviewIngestArgs parses flags independently of positional artifacts.
// flag.FlagSet stops at the first positional argument, so using it directly
// made `artifact --dry-run` treat the flag as another filename. Parsing the
// complete argument list first keeps malformed invocations side-effect free.
func parseReviewIngestArgs(args []string) (reviewIngestArgs, error) {
	parsed := reviewIngestArgs{
		// FAC-572: resolved through the ONE review-root resolver, so this
		// command cannot audit a different corpus than the queue refers to.
		auditRoot:  reviewroot.Resolve(".").Root,
		ledgerPath: reviewledger.DefaultPath(""),
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			parsed.files = append(parsed.files, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			parsed.files = append(parsed.files, arg)
			continue
		}
		switch {
		case arg == "--json" || arg == "-json":
			parsed.asJSON = true
		case arg == "--dry-run" || arg == "-dry-run":
			parsed.dryRun = true
		case arg == "--sweep" || arg == "-sweep":
			parsed.sweep = true
		case arg == "--audit" || arg == "-audit":
			parsed.audit = true
		case strings.HasPrefix(arg, "--audit-root="):
			parsed.auditRoot = strings.TrimPrefix(arg, "--audit-root=")
		case strings.HasPrefix(arg, "--ledger="):
			parsed.ledgerPath = strings.TrimPrefix(arg, "--ledger=")
		case arg == "--audit-root" || arg == "-audit-root":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return reviewIngestArgs{}, fmt.Errorf("%s requires a value", arg)
			}
			i++
			parsed.auditRoot = args[i]
		case arg == "--ledger" || arg == "-ledger":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return reviewIngestArgs{}, fmt.Errorf("%s requires a value", arg)
			}
			i++
			parsed.ledgerPath = args[i]
		default:
			return reviewIngestArgs{}, fmt.Errorf("unknown flag %q", arg)
		}
	}
	if parsed.sweep && parsed.audit {
		return reviewIngestArgs{}, fmt.Errorf("--sweep and --audit are mutually exclusive")
	}
	if parsed.sweep && len(parsed.files) != 0 {
		return reviewIngestArgs{}, fmt.Errorf("--sweep discovers artifacts itself; do not also name them")
	}
	if parsed.sweep {
		found, err := sweepUningestedArtifacts(parsed.auditRoot, parsed.ledgerPath)
		if err != nil {
			return reviewIngestArgs{}, err
		}
		// An empty sweep is a legitimate clean result, not a usage error. The
		// files==0 check below would otherwise print the usage line and exit
		// non-zero on a healthy inbox, which reads as a broken command.
		parsed.files = found
		return parsed, nil
	}
	if parsed.audit && parsed.dryRun {
		return reviewIngestArgs{}, fmt.Errorf("--audit and --dry-run are mutually exclusive")
	}
	if parsed.audit && len(parsed.files) != 0 {
		return reviewIngestArgs{}, fmt.Errorf("--audit cannot be combined with verdict artifacts")
	}
	if !parsed.audit && len(parsed.files) == 0 {
		return reviewIngestArgs{}, fmt.Errorf("usage: herd review-ingest (<verdict-artifact>... | --sweep) [--dry-run]")
	}
	return parsed, nil
}

// sweepUningestedArtifacts lists inbox verdicts that the ledger has never
// recorded.
//
// FAC-606: review-ingest required EXPLICIT paths and never scanned, so a verdict
// landing in the inbox was inert until a human or agent enumerated it by hand.
// 89 finished reviews were found sitting there, unowned -- reviews that had
// really run, against real candidates, producing nothing. Instructing the fleet
// to enumerate the inbox every beat did not hold: the pile rebuilt to the same 89
// within the hour. A discovery step that must be remembered is not a discovery
// step.
//
// Membership is decided on the artifact BASENAME, because the ledger records the
// path as it was at ingest time and reviewers on a second host write under a
// different absolute prefix. Comparing full paths would re-ingest every remote
// verdict on every sweep.
func sweepUningestedArtifacts(reviewRoot, ledgerPath string) ([]string, error) {
	inbox := filepath.Join(reviewRoot, "inbox")
	entries, err := os.ReadDir(inbox)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read review inbox %s: %w", inbox, err)
	}
	seen, err := ledgerArtifactBasenames(ledgerPath)
	if err != nil {
		// Fail closed. An unreadable ledger means every artifact looks unseen, and
		// sweeping on that assumption would re-ingest the entire corpus.
		return nil, fmt.Errorf("read review ledger %s: %w", ledgerPath, err)
	}
	// FAC-647: an artifact is ingested if its VERDICT is recorded, not if its
	// FILENAME is.
	//
	// This matched inbox basenames against the ledger's artifact field while
	// ingestion dedups on sha+reviewer. A verdict admitted under a different
	// artifact filename -- a retry, a re-push from another host, a rename in
	// transport -- therefore counted as un-ingested forever. Measured live: of 599
	// inbox files, 596 had a verdict row for their SHA, only 300 matched by
	// basename, and 296 were reported as a backlog that did not exist. Re-running
	// the sweep printed "admitted=296" while every line said DUPLICATE
	// enqueued=false and inbox_uningested never moved.
	//
	// That is the mirror of the absence-as-negative defects: a phantom POSITIVE.
	// It sent an operator hunting for 299 lost reviews that were already recorded,
	// and I reported the same false number upstream myself. So the sweep now uses
	// the ledger's own admission identity.
	verdicted, err := ledgerVerdictedSHAs(ledgerPath)
	if err != nil {
		return nil, fmt.Errorf("read review ledger verdicts %s: %w", ledgerPath, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		if sha := artifactSHAPrefix(name); sha != "" {
			if _, ok := verdicted[sha]; ok {
				continue
			}
		}
		out = append(out, filepath.Join(inbox, name))
	}
	sort.Strings(out)
	return out, nil
}

// artifactSHAPrefix reads the candidate SHA an inbox artifact is named for.
// Transport names every verdict "<sha12>-<reviewer>.md", so the prefix is the
// candidate identity even when the rest of the filename differs between hosts.
// Returns "" when the leading field is not hex, so an unparseable name is never
// silently treated as some candidate's verdict.
func artifactSHAPrefix(name string) string {
	base := strings.TrimSuffix(name, ".md")
	head := base
	if i := strings.Index(base, "-"); i > 0 {
		head = base[:i]
	}
	if len(head) < 12 {
		return ""
	}
	head = strings.ToLower(head[:12])
	for _, r := range head {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return ""
		}
	}
	return head
}

// ledgerVerdictedSHAs returns the 12-hex prefix of every SHA that already has a
// verdict row, which is the identity ingestion itself dedups on.
func ledgerVerdictedSHAs(ledgerPath string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	f, err := os.Open(ledgerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct {
			Event string `json:"event"`
			SHA   string `json:"sha"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if row.Event != string(reviewledger.EventVerdict) {
			continue
		}
		if sha := strings.ToLower(strings.TrimSpace(row.SHA)); len(sha) >= 12 {
			out[sha[:12]] = struct{}{}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func ledgerArtifactBasenames(ledgerPath string) (map[string]struct{}, error) {
	seen := map[string]struct{}{}
	f, err := os.Open(ledgerPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No ledger yet is a real, readable state: nothing has been ingested.
			return seen, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct {
			Artifact string `json:"artifact"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if a := strings.TrimSpace(row.Artifact); a != "" {
			seen[filepath.Base(a)] = struct{}{}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return seen, nil
}

type reviewIngestLedger interface {
	Ingest(reviewledger.IngestOpts) (bool, error)
	Validate(reviewledger.IngestOpts) error
	VerdictFor(string) (reviewledger.LedgerRow, bool, error)
	VerdictForReviewer(string, string) (reviewledger.LedgerRow, bool, error)
	// FAC-620: the corroboration an asserted builder-family needs when no
	// launch receipt reaches the commit.
	ProvenBuilderFamily(string) (string, error)
}

type reviewIngestDecision string

const (
	reviewIngestAdmit         reviewIngestDecision = "admit"
	reviewIngestSkipDuplicate reviewIngestDecision = "skip-duplicate"

	dispositionDuplicate = "duplicate"
	dispositionAdmitted  = "admitted"
)

// ingestDisposition reports how an artifact whose ledger write returned
// enqueued should be dispositioned. enqueued == false means the verdict row
// already existed, which is a duplicate regardless of verdict polarity.
//
// FAC-581: this used to read `!enqueued && verdict == "PASS"`, so a re-ingested
// FAIL was reported as freshly ADMITTED. A bulk replay of the historical inbox
// therefore re-applied days-old FAIL transitions and reverted current cards to
// to-do. Duplicate suppression must not depend on which way the verdict went.
// verdict is accepted so that every disposition rule lives here, where the
// polarity-independence test can hold it, rather than back at the call site.
// honestlyUnrecordedFamily reports whether a builder-family value is a candid
// "I could not determine this", and normalises it.
//
// It deliberately does NOT accept a near-miss of a real family. A reviewer that
// typos "anthropc" is asserting authorship it did not verify, and FAC-590 exists
// to refuse exactly that. Only an explicit unknown -- optionally followed by the
// reviewer's explanation, which several write at length -- is honest.
// artifactIsCandidAboutFamily reports that the artifact declined to claim a
// family it could not prove -- an honest "unknown", or a hedged claim. Those
// stay admissible and route to provenance-unrecorded; only a bare unhedged
// assertion needs corroborating.
func artifactIsCandidAboutFamily(raw string) bool {
	_, candid := honestlyUnrecordedFamily(raw)
	return candid
}

func honestlyUnrecordedFamily(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return "", false
	}
	for _, sentinel := range []string{"unknown", "unrecorded", "unspecified", "unproven", "none", "n/a"} {
		if v == sentinel || strings.HasPrefix(v, sentinel+" ") || strings.HasPrefix(v, sentinel+"(") ||
			strings.HasPrefix(v, sentinel+":") || strings.HasPrefix(v, sentinel+",") {
			return reviewledger.FamilyUnrecorded, true
		}
	}
	// FAC-647: a HEDGED family claim is honest provenance, not a broken field.
	//
	// A reviewer wrote: `openai — inferred, not fabricated: both new describe-block
	// additions name their tests security-sentinel ... No stronger per-commit
	// attribution exists (commits are authored as the shared human identity, no
	// trailers, per repo policy)`. FAC-628 taught this parser to accept a leading
	// "unknown"-style sentinel with trailing prose, but not a leading REAL family
	// with trailing prose, so that whole review was refused as unprovable.
	//
	// It is routed to `unrecorded` rather than accepted as `openai`, deliberately.
	// The reviewer inferred the family and said so; the family gate exists to PROVE
	// reviewer/builder disjointness, and an inference is not a proof. Recording it
	// as proven would overstate the evidence. So the review is kept and the
	// provenance claim is not upgraded -- exactly what FAC-627's sentinel is for.
	//
	// An exact bare family (no hedging prose) is untouched and still resolves
	// normally, so this cannot launder a clean claim into an unrecorded one.
	for _, hedge := range []string{"inferred", "not fabricated", "no stronger", "cannot prove", "unprovable", "best guess", "presumed", "likely"} {
		if strings.Contains(v, hedge) {
			return reviewledger.FamilyUnrecorded, true
		}
	}
	return "", false
}

// postReviewCompleteCallback announces that a verdict is now ADMITTED, so the
// next stage runs on an event instead of a poll.
//
// FAC-651: the callback bus already existed -- mail.Callback, CallbackComplete,
// PostCallback with effect-level dedupe, and pulse's ConsumeCallback/ack loop --
// and `herd shot` used all of it. The review path used none of it, so every
// stage after a verdict was discovered by someone polling: the supervisor slept,
// woke, harvested, swept, and only then noticed a candidate had become
// mergeable. A verdict admitted at 21:04 could sit until the next beat for no
// reason other than that nothing said it had happened.
//
// Admission is the right event to announce, not reviewer exit. A reviewer that
// has written its artifact has not necessarily produced an admissible verdict --
// it may be refused for an empty diff, an unprovable family, or a missing
// digest. Admission is the first moment the fact is durable and authoritative,
// and it is where merge eligibility actually changes.
//
// Best-effort by construction: the ledger write already succeeded and IS the
// durable record. A failed announcement must never turn an admitted verdict into
// a refusal, so this reports to stderr and returns. Losing the event costs one
// polling interval, which is exactly the behaviour that existed before.
//
// DedupeID is sha+reviewer, the same identity the ledger dedups on, so the
// idempotent re-ingest sweep cannot post the same completion twice.

// reclaimReviewPoolSlotFor releases the pool slot a completed review was holding.
//
// FAC-675: a slot was leased at launch and released only when the launching
// command returned. A reviewer that outlived its launcher -- which is the normal
// case, since the launcher exits as soon as the agent starts -- left the lease
// held with nothing running behind it. The pool then reported itself saturated
// while slots sat idle, and dispatch waited on capacity that existed.
//
// The verdict's ADMISSION is the right moment to reclaim. Not reviewer exit: a
// reviewer can exit having written nothing, and reclaiming then would free a
// slot whose work is lost. Admission is the first point at which the review is
// durably finished, which is exactly why FAC-651 chose it for the completion
// event too.
//
// FAC-656 records the lease id on the launch row, so the slot is identified
// EXACTLY rather than guessed from the surface path -- a guess would risk
// releasing a slot another reviewer had since taken.
//
// Best-effort by construction: the verdict is already admitted and that is the
// durable outcome. A failed reclaim costs one slot until the reaper notices,
// which is strictly better than failing an admitted verdict over housekeeping.
func reclaimReviewPoolSlotFor(sha string) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return
	}
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	l, err := reviewledger.NewReviewLedger(root, reviewLedgerPath())
	if err != nil {
		return
	}
	rows, err := l.AllRows()
	if err != nil {
		return
	}
	lease := ""
	for i := range rows {
		if rows[i].Event == string(reviewledger.EventRecord) &&
			l.NormalizeSHA(rows[i].SHA) == l.NormalizeSHA(sha) &&
			strings.TrimSpace(rows[i].Lease) != "" {
			lease = strings.TrimSpace(rows[i].Lease)
		}
	}
	if lease == "" {
		// No recorded lease: pre-FAC-656 launches, or a review that never held a
		// pool slot. Nothing to reclaim and nothing to report.
		return
	}
	p := worktree.NewPool(root, "", 2)
	if err := p.Release(context.Background(), lease); err != nil {
		fmt.Fprintf(os.Stderr, "review-ingest: verdict for %s admitted but pool lease %s could not be released (%v); "+
			"the slot stays held until the pool reclaims it\n", sha[:minInt(12, len(sha))], lease, err)
	}
}

func postReviewCompleteCallback(sha, branch, reviewer, verdict string) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return
	}
	ref := strings.TrimSpace(branch)
	if ref == "" {
		ref = sha
	}
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	cb := mail.Callback{
		Ref:      ref,
		Kind:     mail.CallbackComplete,
		SHA:      sha,
		Detail:   fmt.Sprintf("review verdict %s admitted by %s", strings.TrimSpace(verdict), strings.TrimSpace(reviewer)),
		DedupeID: "review-admitted:" + sha + ":" + strings.ToLower(strings.TrimSpace(reviewer)),
	}
	if _, err := mail.NewMailbox(mail.CallbackMailPath(root)).PostCallback("review-ingest", cb); err != nil {
		fmt.Fprintf(os.Stderr, "review-ingest: verdict for %s was ADMITTED but its completion callback could not be posted (%v); the next stage will fall back to polling\n", sha[:minInt(12, len(sha))], err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ingestTaskIdentityFor is the single source both ledger rows take their task
// from, so they cannot disagree by construction (FAC-657).
//
// FAC-578 multi-ref decision: one verdict carries exactly one closeable card
// ref. A lane branch covering N cards needs N reviews. The branch argument is
// retained so call sites stay explicit about the location they considered and
// discarded; it is never returned as a task identity.
func ingestTaskIdentityFor(taskRef, branch string) string {
	_ = branch
	return reviewledger.CloseableCardRef(taskRef)
}

func ingestDisposition(enqueued bool, verdict string) string {
	_ = verdict
	if !enqueued {
		return dispositionDuplicate
	}
	return dispositionAdmitted
}

// reviewIngestAdmissionDecision is the read-only gate shared by dry-run and
// real ingestion. Keeping duplicate and destination checks here ensures both
// modes report the same admission outcome.
func reviewIngestAdmissionDecision(ledger reviewIngestLedger, opts reviewledger.IngestOpts, source, destinationName string) (reviewIngestDecision, error) {
	sha := opts.Verdict.SHA
	if sha == "" {
		sha = destinationName
	}
	if err := ledger.Validate(opts); err != nil {
		return "", err
	}
	if opts.Verdict.Reviewer != "" {
		if _, found, err := ledger.VerdictForReviewer(sha, opts.Verdict.Reviewer); err != nil {
			return "", fmt.Errorf("read existing ledger verdict: %w", err)
		} else if found {
			return reviewIngestSkipDuplicate, nil
		}
	}
	if err := reviewingest.CheckMoveToIngested(source, destinationName); err != nil {
		return "", fmt.Errorf("preflight artifact move: %w", err)
	}
	return reviewIngestAdmit, nil
}

// admitVerdictAndMove makes the ledger row observable before moving the source
// artifact. An append acknowledgement alone is not sufficient admission
// evidence: a failed read-back must leave the artifact available for retry.
func admitVerdictAndMove(ledger reviewIngestLedger, opts reviewledger.IngestOpts, source, destinationName string) (bool, error) {
	sha := opts.Verdict.SHA
	if sha == "" {
		sha = destinationName
	}
	decision, err := reviewIngestAdmissionDecision(ledger, opts, source, destinationName)
	if err != nil {
		return false, err
	}
	if decision == reviewIngestSkipDuplicate {
		// A duplicate handoff is a durable no-op. Leave the source available
		// for inspection/retry; it must not be consumed as ingested evidence.
		return false, nil
	}
	destination, err := reviewingest.MoveToIngestedNamed(source, destinationName)
	if err != nil {
		return false, fmt.Errorf("move admitted artifact: %w", err)
	}
	enqueued, err := ledger.Ingest(opts)
	if err != nil {
		if destination != source {
			_ = os.Rename(destination, source)
		}
		return false, err
	}
	if _, found, err := ledger.VerdictFor(sha); err != nil {
		if destination != source {
			_ = os.Rename(destination, source)
		}
		return false, fmt.Errorf("read back ledger verdict: %w", err)
	} else if !found {
		if destination != source {
			_ = os.Rename(destination, source)
		}
		return false, fmt.Errorf("read back ledger verdict: sha %s not found", sha)
	}
	return enqueued, nil
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
	repoRoot, err := filepath.Abs(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: resolve repository root: %v\n", err)
		os.Exit(1)
	}
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
	// FAC-670: the merge-side half of FAC-668. readiness could express the
	// operator's decision and harvest-merge -- the only supported local-evidence
	// merge path -- could not receive it, so an accepted candidate still could
	// not land. Per-candidate on purpose; never a queue-wide switch.
	allowUnrecorded := fs.Bool("allow-unrecorded-provenance", false,
		"accept THIS candidate's PASS even though its builder family was never recorded; "+
			"cross-family independence is not claimed. Cannot override FAIL, BLOCKED, split, or a missing verdict")
	candidate := fs.String("candidate", "", "Exact reviewed candidate SHA to harvest (required when the branch tip moved)")
	reconstructedFrom := fs.String("reconstructed-from", "", "Harvest identity reconstructed from the reviewed candidate (requires --content-proof)")
	contentProof := fs.String("content-proof", "", "Operator attestation that reviewed and reconstructed identities are content-equivalent")
	candidateRange := fs.String("candidate-range", "", "Exact reviewed range to harvest (<base>..<sha>); limits cherry-picks to that range")
	dryRun := fs.Bool("dry-run", false, "Plan and gate without creating the worktree")
	allowMarkers := fs.Bool("allow-markers", false,
		"Proceed despite conflict markers in the harvested diff (for files whose CONTENT is marker fixtures)")
	var unionMergePaths []string
	fs.Func("union-merge-path", "Append-only repository-relative path to union when lanes conflict (repeatable)", func(path string) error {
		unionMergePaths = append(unionMergePaths, path)
		return nil
	})
	verifyLanded := fs.Bool("verify-landed", false,
		"Check whether the lane's work is on origin/main (rebase + empty diff) and mint/reconcile the sealed completion receipt.")
	verifyRef := fs.String("ref", "", "Task ref for --verify-landed receipt reconcile (loads merge-admission or explicit binding flags)")
	verifyTaskID := fs.String("task-id", "", "Provider task id (required with --verify-landed when no merge-admission is on disk)")
	// --candidate is shared with the harvest path (registered above); a second
	// fs.String here panics with "flag redefined" on every invocation.
	verifyBaseSHA := fs.String("base-sha", "", "Base sha the candidate was reviewed against")
	verifyLease := fs.String("lease", "", "Claim lease token bound into the ledger verdict")
	verifyLeaseGen := fs.Int64("lease-generation", 0, "Claim lease generation bound into the completion receipt")
	verifyPatchID := fs.String("patch-id", "", "Patch identity bound into the ledger verdict")
	verifyAcceptance := fs.String("acceptance-digest", "", "Acceptance digest bound at review time")
	verifyAuthorFamily := fs.String("author-family", "", "Builder model family")
	verifyAuthorIdentity := fs.String("author-identity", "", "Builder session identity")
	verifyProviderRev := fs.String("provider-revision", "", "Board card revision the reviewer bound")
	verifyPR := fs.Int("pr", 0, "Pull request number for reduced-provenance verify-landed reconciliation")

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
		fmt.Fprintln(os.Stderr, "usage: herd harvest-merge <lane> --branch <branch> --title <t> [--candidate-range <base>..<sha>] [--verdict PASS]")
		fmt.Fprintln(os.Stderr, "       herd harvest-merge <lane> --branch <branch> --verify-landed --ref <FAC-x> [...]")
		os.Exit(2)
	}

	// FAC-213 + FAC-379: --verify-landed is the post-merge "did this merge?"
	// check, then the sealed completion-receipt reconcile for approve.
	if *verifyLanded {
		binding := verifyLandedBinding{
			Ref: *verifyRef, TaskID: *verifyTaskID, Candidate: *candidate,
			BaseSHA: *verifyBaseSHA, Lease: *verifyLease, LeaseGeneration: *verifyLeaseGen,
			PatchID: *verifyPatchID, AcceptanceDigest: *verifyAcceptance,
			AuthorFamily: *verifyAuthorFamily, AuthorIdentity: *verifyAuthorIdentity,
			ProviderRevision: *verifyProviderRev,
			PullRequest:      *verifyPR,
		}
		if err := runHarvestVerifyLanded(*branch, binding); err != nil {
			fmt.Fprintf(os.Stderr, "herd harvest-merge: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *title == "" {
		fmt.Fprintln(os.Stderr, "usage: herd harvest-merge <lane> --branch <branch> --title <t> [--candidate-range <base>..<sha>] [--verdict PASS]")
		fmt.Fprintln(os.Stderr, "       (or use --verify-landed to check if a merge landed)")
		os.Exit(2)
	}

	rangeSpec := harvestmerge.CandidateRange{}
	if strings.TrimSpace(*candidateRange) != "" {
		parsed, parseErr := harvestmerge.ParseCandidateRange(*candidateRange)
		if parseErr != nil {
			fmt.Fprintln(os.Stderr, parseErr)
			os.Exit(2)
		}
		rangeSpec = parsed
		if strings.TrimSpace(*candidate) != "" && strings.TrimSpace(*candidate) != rangeSpec.SHA {
			fmt.Fprintf(os.Stderr, "herd harvest-merge: --candidate %s disagrees with --candidate-range endpoint %s\n", *candidate, rangeSpec.SHA)
			os.Exit(2)
		}
		*candidate = rangeSpec.SHA
	}
	report, err := resolveHarvestCandidateWithReconstructionAt(repoRoot, *branch, *candidate, *reconstructedFrom, *contentProof, *allowUnrecorded)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: %v\n", err)
		os.Exit(1)
	}
	sha := report.Pin.SHA
	fmt.Printf("herd harvest-merge: tip=%s last_pass_sha=%s eligible=%t\n",
		shortSHA12(report.Tip), shortSHA12(report.LastPassSHA), report.Eligible)
	// Never filter silently: a candidate an operator expected must be visible
	// along with the reason it was not selected.
	for _, other := range report.OffBranchQueued {
		fmt.Printf("herd harvest-merge: queued candidate %s belongs to another branch; not selected for %s\n",
			shortSHA12(other), *branch)
	}
	if report.Retired {
		fmt.Printf("herd harvest-merge: branch %s is settled as RETIRED at %s; no merge is authorized\n", *branch, shortSHA12(report.Pin.SHA))
		return
	}
	if !report.Eligible {
		if *dryRun {
			return
		}
		// FAC-685: this used to print one message for two different situations,
		// and for the commoner one it named a remedy that could not be followed.
		//
		// Reported live: `herd review-ledger readiness` said ready=true for
		// CHA-2265 40b9006a (1 PASS, no dissent, provenance_unrecorded=0) while
		// harvest refused with "last_pass_sha empty", which reads as "there is
		// no PASS". There was a PASS; no ref reached that commit, so it was not
		// selectable FOR THIS BRANCH. Readiness and harvest were never
		// contradicting -- they answer different questions, and harvest did not
		// say which one it had answered.
		//
		// Telling the operator to pass `--candidate <last_pass_sha>` when
		// last_pass_sha is empty is a correct refusal that names no remedy,
		// which stops work exactly as effectively as a wrong one.
		switch {
		case report.LastPassSHA == "" && len(report.OffBranchQueued) > 0:
			fmt.Fprintf(os.Stderr, "herd harvest-merge: %d PASSed candidate(s) exist but NONE is reachable from %s, so none is selectable for this branch.\n",
				len(report.OffBranchQueued), *branch)
			fmt.Fprintf(os.Stderr, "  This is not the same as having no verdict: `herd review-ledger readiness %s` can legitimately report ready=true.\n",
				shortSHA12(report.OffBranchQueued[0]))
			fmt.Fprintf(os.Stderr, "  Harvest asks a narrower question -- is a reviewed candidate reachable from THIS branch.\n")
			fmt.Fprintf(os.Stderr, "  Remedy: harvest the exact candidate with --candidate %s, or push a ref that reaches it.\n",
				report.OffBranchQueued[0])
		case report.LastPassSHA == "":
			fmt.Fprintf(os.Stderr, "herd harvest-merge: no PASSed candidate is queued for %s at all; this branch needs a review, not a different flag\n", *branch)
		default:
			fmt.Fprintf(os.Stderr, "herd harvest-merge: branch tip %s has drifted past the reviewed candidate; harvest it exactly with --candidate %s, or obtain a new PASS at the tip\n",
				shortSHA12(report.Tip), report.LastPassSHA)
		}
		os.Exit(1)
	}
	// Resolve the exact eligible identity before selecting any commits. A
	// standing branch may have advanced past the reviewed candidate; allowing
	// harvestCommits to see that branch tip would make the eligibility decision
	// and the harvested set describe different histories.
	selectionRange := rangeSpec
	if selectionRange.Base == "" {
		selectionRange.Base = *base
	}
	if report.ReconstructedSHA != "" {
		selectionRange.SHA = report.ReconstructedSHA
	} else {
		selectionRange.SHA = report.Pin.SHA
	}
	if selectionRange.Base == "" || selectionRange.SHA == "" {
		fmt.Fprintln(os.Stderr, "herd harvest-merge: eligible candidate cannot be bounded to an exact base and candidate")
		os.Exit(1)
	}
	commits, err := harvestCommits(selectionRange.Base, *branch, selectionRange)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: %v\n", err)
		os.Exit(1)
	}
	if err := verifyHarvestSelection(repoRoot, selectionRange.Base, selectionRange.SHA, commits); err != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: %v\n", err)
		os.Exit(1)
	}
	ledgerVerdict, vErr := harvestMergeVerdict(sha, *verdict, *allowUnrecorded)
	if vErr != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: %v\n", vErr)
		os.Exit(1)
	}

	// Content evidence for the empty-diff gate. A commit COUNT is not a content
	// check: PR #151 merged 0 additions / 0 deletions / 0 files because the
	// branch carried only its anchor commit. Computed here rather than passed in
	// so the gate cannot be bypassed by a caller that simply omits it.
	diffTarget := *branch
	if rangeSpec.SHA != "" {
		diffTarget = rangeSpec.SHA
	}
	diffstatCmd := exec.Command("git", "diff", "--shortstat", *base+"..."+diffTarget)
	diffstatCmd.Dir = repoRoot
	diffstatOut, diffErr := diffstatCmd.Output()
	if diffErr != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: git diff --shortstat %s...%s: %v\n", *base, diffTarget, diffErr)
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
	err = harvestBody(dir, *base, plan.TempBranch, commits, *allowMarkers, unionMergePaths)

	// Cleanup runs on the FAILURE path only — the worktree is deliberately kept
	// on success so the coordinator can push from it.
	cleanup := func() {
		// dir is a self-managed harvest-merge staging worktree created
		// directly via `git worktree add` above, never through
		// pkg/claim.Acquire -- it will never have lease history there.
		if guardErr := worktree.RefuseRemovalWithoutLeaseHistoryCheck(context.Background(), ".", dir); guardErr != nil {
			fmt.Fprintf(os.Stderr, "herd harvest-merge: cleanup refused: %v\n", guardErr)
			return
		}
		removeCmd := exec.Command("git", "worktree", "remove", "--force", "--", dir)
		removeCmd.Dir = repoRoot
		_ = removeCmd.Run()
		branchCmd := exec.Command("git", "branch", "-D", "--", plan.TempBranch)
		branchCmd.Dir = repoRoot
		_ = branchCmd.Run()
	}

	if err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "herd harvest-merge: %v\n", err)
		os.Exit(1)
	}
	if report.ReconstructedSHA != "" {
		ledger, ledgerErr := reviewledger.NewReviewLedger(".", reviewledger.DefaultPath(""))
		if ledgerErr != nil {
			fmt.Fprintf(os.Stderr, "herd harvest-merge: open review ledger for reconstruction: %v\n", ledgerErr)
			os.Exit(1)
		}
		if ledgerErr = ledger.Reconstruction(reviewledger.ReconstructionOpts{
			SHA: report.ReconstructedSHA, CandidateSHA: report.Pin.SHA,
			Branch: *branch, ContentProof: strings.TrimSpace(*contentProof),
		}); ledgerErr != nil {
			fmt.Fprintf(os.Stderr, "herd harvest-merge: record reconstruction: %v\n", ledgerErr)
			os.Exit(1)
		}
	}

	fmt.Printf("herd harvest-merge: harvested %d commit(s) clean onto %s at %s\n", len(commits), *base, dir)
	fmt.Println("herd harvest-merge: gates passed. Publish and merge is the coordinator's explicit action:")
	fmt.Printf("  git push -u origin %s && gh pr create --title %q\n", plan.TempBranch, *title)
	// The worktree is intentionally KEPT on success: the coordinator pushes
	// from it. Cleanup on success is the caller's, after publishing.
}

// harvestCommits selects only the commits represented by the reviewed range.
// An empty range is retained for callers that intentionally need the legacy
// branch-wide conflict self-abort behavior, but the harvest command always
// supplies an exact range after resolving eligibility.
func harvestCommits(base, branch string, reviewed harvestmerge.CandidateRange) ([]string, error) {
	repoRoot, err := filepath.Abs(".")
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	cherryBase, target := base, branch
	if reviewed.SHA != "" {
		if strings.TrimSpace(reviewed.Base) == "" {
			return nil, fmt.Errorf("harvest-merge: pinned candidate requires an exact base")
		}
		cherryBase, target = reviewed.Base, reviewed.SHA
	}
	cmd := exec.Command("git", "cherry", cherryBase, target)
	cmd.Dir = repoRoot
	cherry, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cherry %s %s: %w", cherryBase, target, err)
	}
	return harvestmerge.UniqueCommits(string(cherry)), nil
}

// verifyHarvestSelection repeats the exact immutable-range query after
// selection. The harvest command must never write a worktree from a list that
// differs from git cherry <base> <candidate>, even if selection code changes
// later or a caller accidentally widens the target back to a branch tip.
func verifyHarvestSelection(repoRoot, base, candidate string, selected []string) error {
	base = strings.TrimSpace(base)
	candidate = strings.TrimSpace(candidate)
	if base == "" || candidate == "" {
		return fmt.Errorf("harvest-merge: pinned candidate selection requires an exact base and candidate")
	}
	cmd := exec.Command("git", "cherry", base, candidate)
	cmd.Dir = repoRoot
	cherry, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git cherry %s %s for selection verification: %w", base, candidate, err)
	}
	expected := harvestmerge.UniqueCommits(string(cherry))
	if strings.Join(expected, "\n") != strings.Join(selected, "\n") {
		return fmt.Errorf("harvest-merge: selected commit set does not match pinned candidate %s", shortSHA12(candidate))
	}
	return nil
}

// harvestCandidateReport is the provenance-first decision made before any
// cherry-pick. Tip is observational; LastPassSHA comes from the durable queue.
type harvestCandidateReport struct {
	Pin         harvestmerge.CandidatePin
	Tip         string
	LastPassSHA string
	// OffBranchQueued are queued candidates that belong to other branches.
	// Reported so a suppressed candidate is visible rather than silently
	// filtered: an operator who expected one of these needs to see why it was
	// not selected.
	OffBranchQueued  []string
	Eligible         bool
	ReconstructedSHA string
	Retired          bool
}

// resolveHarvestCandidate keeps a moving standing branch from silently
// replacing the reviewed candidate. Without an explicit pin, only a branch
// whose current tip is the latest queued PASS may proceed. An explicit pin
// permits an older reviewed ancestor, but still requires exact queue and
// ledger evidence for that SHA.
func resolveHarvestCandidate(branch, requested string) (harvestCandidateReport, error) {
	repoRoot, err := filepath.Abs(".")
	if err != nil {
		return harvestCandidateReport{}, fmt.Errorf("resolve repository root: %w", err)
	}
	return resolveHarvestCandidateWithReconstructionAt(repoRoot, branch, requested, "", "")
}

// resolveHarvestCandidateWithReconstruction preserves the normal ancestry
// gate. When both reconstruction fields are supplied, the reviewed candidate
// remains the identity whose PASS is required, while reconstructedSHA must be
// a real commit reachable from the harvest branch. The substitution is only
// accepted with a non-empty operator content-equality proof.
func resolveHarvestCandidateWithReconstruction(branch, requested, reconstructedSHA, contentProof string) (harvestCandidateReport, error) {
	repoRoot, err := filepath.Abs(".")
	if err != nil {
		return harvestCandidateReport{}, fmt.Errorf("resolve repository root: %w", err)
	}
	return resolveHarvestCandidateWithReconstructionAt(repoRoot, branch, requested, reconstructedSHA, contentProof)
}

// resolveHarvestCandidateWithReconstructionAt resolves the exact candidate and
// decides its eligibility.
//
// FAC-671: this decides eligibility BEFORE harvestMergeVerdict runs, and rejects
// on report.Eligible, so FAC-670's acceptance -- threaded only into the later
// gate -- was unreachable for the class it was written for. Reported by the
// orchestrator on the same SHA FAC-670 was verified against: the run exited at
// "exact reviewed candidate is not eligible" without ever reaching the flag.
//
// I fixed one of TWO Eligible call sites and verified the one I had changed.
// That is the second time in this sequence I confirmed a fix on the path I was
// looking at rather than the path that runs.
//
// allowUnrecorded is variadic so every existing caller keeps the strict default
// and only the explicit operator path opts in.
func resolveHarvestCandidateWithReconstructionAt(repoRoot, branch, requested, reconstructedSHA, contentProof string, allowUnrecorded ...bool) (harvestCandidateReport, error) {
	accept := len(allowUnrecorded) > 0 && allowUnrecorded[0]
	var err error
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return harvestCandidateReport{}, fmt.Errorf("branch is required")
	}
	headCmd := exec.Command("git", "rev-parse", "--verify", branch+"^{commit}")
	headCmd.Dir = repoRoot
	head, err := headCmd.Output()
	if err != nil {
		return harvestCandidateReport{}, fmt.Errorf("cannot resolve %s: %w", branch, err)
	}
	report := harvestCandidateReport{Tip: strings.TrimSpace(string(head))}
	if report.Tip == "" {
		return harvestCandidateReport{}, fmt.Errorf("%s resolved to no commit", branch)
	}

	ledger, err := reviewledger.NewReviewLedger(repoRoot, reviewledger.DefaultPath(repoRoot))
	if err != nil {
		return harvestCandidateReport{}, fmt.Errorf("open review ledger: %w", err)
	}
	rows, err := ledger.AllRows()
	if err != nil {
		return harvestCandidateReport{}, fmt.Errorf("read review ledger: %w", err)
	}
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if row.Event != string(reviewledger.EventRetired) || row.Branch != branch {
			continue
		}
		if requested != "" && requested != row.SHA {
			break
		}
		report.Pin = harvestmerge.CandidatePin{SHA: row.SHA, Branch: branch}
		report.Retired = true
		return report, nil
	}
	// FAC-671: the queue lookup runs BEFORE the eligibility gates, so the
	// acceptance has to reach it or the candidate is never even a candidate.
	queueOf := ledger.Queued
	if accept {
		queueOf = ledger.QueuedAllowingUnrecordedProvenance
	}
	queued, err := queueOf()
	if err != nil {
		return harvestCandidateReport{}, fmt.Errorf("read harvest queue: %w", err)
	}
	var latest reviewledger.LedgerRow
	var offBranch []string
	queuedBySHA := make(map[string]reviewledger.LedgerRow)
	for _, row := range queued {
		// The queue branch is the reviewer task that produced the verdict, not
		// necessarily the standing builder branch being harvested. CandidateSHA
		// and the exact ancestry/eligibility checks below are the provenance
		// boundary between those two branches.
		queuedBySHA[row.SHA] = row

		// FAC-561: last-PASS selection used to be latest-wins across the WHOLE
		// queue, with no branch scoping. With two candidates admitted together
		// it reported the other ref's candidate: harvest-merge for
		// reconstruct/cha-2185-fresh named CHA-2205's 1d5ce367acd4 while the
		// branch's own candidate was 991ce0757eeb. Newest is not the same
		// question as "this branch's".
		//
		// Latest-WITHIN-branch is still correct and intentional: repeated
		// reviews of one branch legitimately supersede each other.
		if !reachableFromBranch(repoRoot, row.SHA, branch) {
			offBranch = append(offBranch, row.SHA)
			continue
		}
		if row.Timestamp >= latest.Timestamp {
			latest = row
		}
	}
	if latest.SHA != "" {
		report.LastPassSHA = latest.SHA
	}
	report.OffBranchQueued = offBranch

	sha := strings.TrimSpace(requested)
	reconstructedSHA = strings.TrimSpace(reconstructedSHA)
	contentProof = strings.TrimSpace(contentProof)
	if (reconstructedSHA == "") != (contentProof == "") {
		return harvestCandidateReport{}, fmt.Errorf("--reconstructed-from and --content-proof must be provided together")
	}
	if reconstructedSHA != "" {
		if sha == "" {
			return harvestCandidateReport{}, fmt.Errorf("--candidate is required with --reconstructed-from")
		}
		reconstructedCmd := exec.Command("git", "rev-parse", "--verify", reconstructedSHA+"^{commit}")
		reconstructedCmd.Dir = repoRoot
		if _, err := reconstructedCmd.Output(); err != nil {
			return harvestCandidateReport{}, fmt.Errorf("reconstructed harvest %s is not a commit: %w", reconstructedSHA, err)
		}
		if !commitIsAncestor(repoRoot, reconstructedSHA, branch) {
			return harvestCandidateReport{}, fmt.Errorf("reconstructed harvest %s is not reachable from branch %s", reconstructedSHA, branch)
		}
	}
	if sha == "" {
		sha = report.Tip
		if report.LastPassSHA != report.Tip {
			return report, nil
		}
	} else if reconstructedSHA == "" {
		candidateCmd := exec.Command("git", "rev-parse", "--verify", sha+"^{commit}")
		candidateCmd.Dir = repoRoot
		if _, err := candidateCmd.Output(); err != nil {
			return harvestCandidateReport{}, fmt.Errorf("candidate %s is not a commit: %w", sha, err)
		}
		if !reachableFromBranch(repoRoot, sha, branch) {
			return harvestCandidateReport{}, fmt.Errorf("candidate %s is not reachable from branch %s", sha, branch)
		}
	}

	_, found := queuedBySHA[sha]
	if !found {
		return report, nil
	}
	gate := ledger.Eligible
	if accept {
		gate = ledger.EligibleAllowingUnrecordedProvenance
	}
	eligible, err := gate(sha, "")
	if err != nil {
		return harvestCandidateReport{}, fmt.Errorf("review ledger refuses %s: %w", shortSHA12(sha), err)
	}
	report.Pin = harvestmerge.CandidatePin{SHA: sha, Branch: branch}
	report.Eligible = eligible
	report.ReconstructedSHA = reconstructedSHA
	return report, nil
}

// harvestBody cherry-picks commits into a fresh worktree off base and gates
// the result. It returns an error rather than calling os.Exit so the caller
// can run cleanup via a deferred function — os.Exit skips defers, which would
// leak the worktree on exactly the failure paths where it matters most.
func harvestBody(dir, base, tempBranch string, commits []string, allowMarkers bool, configuredUnionPaths ...[]string) error {
	repoRoot, err := filepath.Abs(".")
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	unionConfig := harvestmerge.UnionMergeConfig{}
	if len(configuredUnionPaths) > 0 {
		unionConfig.Paths = configuredUnionPaths[0]
	}
	addCmd := exec.Command("git", "worktree", "add", "-B", tempBranch, "--", dir, base)
	addCmd.Dir = repoRoot
	if out, addErr := addCmd.CombinedOutput(); addErr != nil {
		return fmt.Errorf("worktree add: %v: %s", addErr, out)
	}
	for _, c := range commits {
		out, pickErr := exec.Command("git", "-C", dir, "cherry-pick", "--", c).CombinedOutput()
		if pickErr == nil {
			continue
		}
		if resolved, resolveErr := resolveUnionMergeConflicts(dir, base, unionConfig); resolveErr != nil {
			exec.Command("git", "-C", dir, "cherry-pick", "--abort").Run()
			return resolveErr
		} else if resolved {
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

// resolveUnionMergeConflicts repairs only configured append-only files in an
// in-progress cherry-pick. Any unconfigured conflict remains a hard failure.
func resolveUnionMergeConflicts(dir, base string, cfg harvestmerge.UnionMergeConfig) (bool, error) {
	resolved := false
	for _, path := range cfg.SortedPaths() {
		if !cfg.Enabled(path) {
			return false, fmt.Errorf("harvest-merge: invalid union-merge path %q; paths must be repo-relative", path)
		}
		unmerged, err := exec.Command("git", "-C", dir, "diff", "--name-only", "--diff-filter=U", "--", path).Output()
		if err != nil {
			return false, fmt.Errorf("harvest-merge: inspect union conflict %q: %w", path, err)
		}
		if strings.TrimSpace(string(unmerged)) == "" {
			continue
		}
		readStage := func(stage string) (string, error) {
			out, readErr := exec.Command("git", "-C", dir, "show", stage+":"+path).Output()
			if readErr != nil {
				return "", fmt.Errorf("harvest-merge: read %s stage for %q: %w", stage, path, readErr)
			}
			return string(out), nil
		}
		ours, err := readStage(":2")
		if err != nil {
			return false, err
		}
		theirs, err := readStage(":3")
		if err != nil {
			return false, err
		}
		baseContent, err := exec.Command("git", "-C", dir, "show", base+":"+path).Output()
		if err != nil {
			return false, fmt.Errorf("harvest-merge: read base for %q: %w", path, err)
		}
		merged, err := harvestmerge.UnionMerge(string(baseContent), ours, theirs)
		if err != nil {
			return false, fmt.Errorf("harvest-merge: union merge %q: %w", path, err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(path)), []byte(merged), 0o644); err != nil {
			return false, fmt.Errorf("harvest-merge: write union merge %q: %w", path, err)
		}
		if out, addErr := exec.Command("git", "-C", dir, "add", "--", path).CombinedOutput(); addErr != nil {
			return false, fmt.Errorf("harvest-merge: stage union merge %q: %v: %s", path, addErr, out)
		}
		resolved = true
	}
	if !resolved {
		return false, nil
	}
	if out, err := exec.Command("git", "-C", dir, "-c", "core.editor=true", "cherry-pick", "--continue").CombinedOutput(); err != nil {
		return false, fmt.Errorf("harvest-merge: continue union merge: %v: %s", err, out)
	}
	return true, nil
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
func harvestMergeVerdict(sha, operatorVeto string, allowUnrecorded bool) (harvestmerge.Verdict, error) {
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

	ledger, err := reviewledger.NewReviewLedger(".", reviewledger.DefaultPath(""))
	if err != nil {
		return "", fmt.Errorf("open review ledger: %w", err)
	}
	// Empty builder family is the STRICT form: only a cross-family PASS with a
	// provable launch record counts, and any unsuperseded FAIL/BLOCKED or an
	// already-consumed admission refuses.
	//
	// FAC-670: --allow-unrecorded-provenance relaxes exactly one thing -- a PASS
	// whose builder family was never recorded. Dissent, supersession and
	// consumption are unaffected and still refuse.
	gate := ledger.Eligible
	if allowUnrecorded {
		gate = ledger.EligibleAllowingUnrecordedProvenance
	}
	eligible, err := gate(sha, "")
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
	PullRequest                      int
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

	// FAC-566: a MOVING BRANCH HEAD IS NOT CANDIDATE IDENTITY. This used to
	// fall back to the worktree HEAD, which is correct only while the work is
	// still unmerged. For work already rebase-merged outside the fleet -- the
	// exact class this path serves -- HEAD is the LANDED head, so the recorded
	// candidate became the merge commit and Route B then refused the legitimate
	// verdict it was supposed to authorize.
	candidate, err := resolveVerifyLandedCandidate(wtDir, branch, binding)
	if err != nil {
		return err
	}

	if err := hsync.LandedProof(wtDir); err != nil {
		return fmt.Errorf("NOT LANDED — %v", err)
	}
	fmt.Printf("herd harvest-merge: LANDED — %s worktree is on origin/main (rebase + empty diff)\n", branch)

	// FAC-565: record the landing as a durable OBSERVATION before attempting to
	// mint a sealed receipt. A verdict admitted before the merge carries no
	// merge_sha, and receipt reconcile needs pre-merge provenance that cannot be
	// recovered afterwards -- so an already-merged candidate had no way to prove
	// its merge disposition at all. This disposition is not a receipt and says
	// so; it records only what git can still show.
	if ref := strings.TrimSpace(binding.Ref); ref != "" {
		// The landed merge is origin/main's tip at the moment landing was
		// proven: LandedProof establishes the candidate is contained there.
		mergeSHA := ""
		if out, gErr := exec.Command("git", "rev-parse", "origin/main").Output(); gErr == nil {
			mergeSHA = strings.TrimSpace(string(out))
		}
		if mergeSHA != "" {
			if rec, wErr := hsync.WriteLandedDisposition(".", hsync.LandedDisposition{
				Ref: ref, CandidateSHA: candidate, MergeSHA: mergeSHA,
				Branch: branch, Method: hsync.LandedByRebaseEmptyDiff,
			}); wErr != nil {
				fmt.Fprintf(os.Stderr, "herd harvest-merge: landed disposition not recorded: %v\n", wErr)
			} else {
				fmt.Printf("herd harvest-merge: DISPOSITION — %s candidate %s landed as %s (%s)\n  %s\n",
					rec.Ref, shortSHA12(rec.CandidateSHA), shortSHA12(rec.MergeSHA), rec.Method,
					hsync.LandedPath(".", rec.Ref))
			}
		}
	}

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
	if binding.PullRequest > 0 {
		req.ReducedProvenance = &mergeadmit.ReducedProvenance{PullRequest: binding.PullRequest, VerifyLanded: true}
		return req, nil
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
	repoRoot, err := filepath.Abs(".")
	if err != nil {
		return ""
	}
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
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

// reachableFromBranch reports whether an object is contained in a branch.
//
// FAC-561: this question was asked in two places in this file -- once for
// eligibility and, missing entirely, at candidate SELECTION. One definition now
// serves both, so the selector and the gate cannot disagree about what "on this
// branch" means. Six other copies of this git call exist elsewhere in the tree
// and are tracked for consolidation.
func reachableFromBranch(repoRoot, sha, branch string) bool {
	if strings.TrimSpace(sha) == "" || strings.TrimSpace(branch) == "" {
		return false
	}
	return commitIsAncestor(repoRoot, sha, branch)
}

// branchReachesSHA reports whether branch contains sha, using real git
// ancestry. FAC-620: this is what makes a launch receipt evidence about a
// COMMIT rather than about a name.
func branchReachesSHA(branch, sha string) bool {
	// commitIsAncestor is the tree's one definition of "does ref contain sha".
	// FAC-561 was caused by two copies of that check disagreeing about what "on
	// this branch" meant; a second copy here would be the third.
	return commitIsAncestor("", sha, branch)
}

// commitCreationTime is when the reviewed commit OBJECT was created.
//
// The rule itself -- committer time, never author time, and zero when
// unanswerable -- lives in pkg/committime, because pkg/candidateindex asks the
// same question and CI's duplicate-rule gate caught the two copies. That gate
// is right: the author/committer distinction had already been got wrong here
// once, and two copies means a fix lands on only one of them.
func commitCreationTime(sha string) time.Time {
	return committime.Of("", sha)
}
