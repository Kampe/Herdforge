package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/reviewroot"
)

// runLedgerBackfill recovers admission bindings for verdicts already on disk.
//
// FAC-659: FAC-656/657/658 fixed the WRITERS, but 1129 record rows and 1087
// verdict rows were already written with every binding empty. Fixing a writer
// does not fix history, so candidates carrying real PASS verdicts stayed
// permanently inadmissible over a bookkeeping gap rather than a review problem.
//
// Three of the four bindings are recoverable from durable evidence that still
// exists:
//
//	task     the card ref or branch the artifact itself declares
//	patch id computed from git for any candidate SHA still reachable
//	digest   the reviewer's own recorded verification, read from its artifact
//
// The LEASE is not, and is deliberately never invented. A historical pool lease
// is gone: only 4 of 105 review packets mention one and 0 of 17 launch receipts
// carry one. Writing a placeholder would satisfy the shape of the admission gate
// while proving nothing about the slot the review actually ran in, which is the
// exact failure this tree refuses everywhere else. So rows whose lease cannot be
// recovered stay inadmissible, and this command reports how many and why rather
// than quietly closing the gap with a fiction.
//
// Dry-run by default. Nothing is written without --apply.
func runLedgerBackfill(args []string) error {
	fs := flag.NewFlagSet("review-ledger backfill", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "write the recovered bindings; without it, report only")
	limit := fs.Int("limit", 0, "stop after N candidates (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	ledgerPath := reviewLedgerPath()
	l, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		return err
	}
	rows, err := l.AllRows()
	if err != nil {
		return fmt.Errorf("read ledger: %w", err)
	}

	// Index the artifact corpus once: inbox plus everything already ingested.
	reviewRoot := reviewroot.Resolve(".").Root
	artifacts := indexReviewArtifacts(reviewRoot)

	type target struct{ sha, reviewer, artifact string }
	// Read the LAST verdict row per sha+reviewer, exactly as Admit does.
	//
	// The first version kept the FIRST row it saw, which is always the ORIGINAL
	// with empty bindings, so a completed candidate still looked incomplete and
	// the dry run reported 1044 rows as needing recovery immediately after
	// recovering all 1044 of them. The write path was idempotent so nothing was
	// corrupted, but a tool whose report never converges cannot tell an operator
	// whether the work is done -- which is the whole reason to run it.
	latest := map[string]reviewledger.LedgerRow{}
	var order []string
	for i := range rows {
		r := rows[i]
		if r.Event != string(reviewledger.EventVerdict) {
			continue
		}
		key := r.SHA + ":" + r.Reviewer
		if _, seen := latest[key]; !seen {
			order = append(order, key)
		}
		latest[key] = r
	}
	var todo []target
	for _, key := range order {
		r := latest[key]
		if strings.TrimSpace(r.PatchURL) != "" && strings.TrimSpace(r.VerificationDigest) != "" {
			continue // already complete on the bindings we can recover
		}
		todo = append(todo, target{sha: r.SHA, reviewer: r.Reviewer, artifact: r.Artifact})
	}

	var recovered, noArtifact, noPatch, noDigest, stuckNoLease int
	for i, t := range todo {
		if *limit > 0 && i >= *limit {
			break
		}
		body, ok := artifacts[filepath.Base(strings.TrimSpace(t.artifact))]
		if !ok {
			noArtifact++
			continue
		}
		a := reviewingest.Parse(body)
		task := ingestTaskIdentityFor(a.TaskRef, a.Branch)
		digest := a.VerificationDigest()
		if digest == "" {
			noDigest++
		}
		patch, patchErr := candidatePatchIdentity(root, t.sha)
		if patchErr != nil {
			noPatch++
			patch = ""
		}
		if patch == "" && digest == "" && task == "" {
			continue
		}
		// The lease is unrecoverable for historical rows, so even a fully
		// recovered candidate stays inadmissible. Say so; do not invent one.
		stuckNoLease++
		if !*apply {
			recovered++
			continue
		}
		if err := l.CompleteVerdictProvenance(t.sha, t.reviewer, task, patch, digest, ""); err != nil {
			fmt.Fprintf(os.Stderr, "backfill %s/%s: %v\n", shortSHA(t.sha), t.reviewer, err)
			continue
		}
		if err := l.CompleteLaunchProvenance(reviewledger.RecordOpts{
			SHA: t.sha, Reviewer: t.reviewer, Task: task, PatchURL: patch,
			Gate: reviewledger.GateBackfilledProvenance,
		}); err != nil {
			// A record row may legitimately be absent for an artifact-only
			// verdict; the verdict row is still worth completing.
			fmt.Fprintf(os.Stderr, "backfill record %s: %v\n", shortSHA(t.sha), err)
		}
		recovered++
	}

	mode := "DRY RUN (pass --apply to write)"
	if *apply {
		mode = "APPLIED"
	}
	fmt.Printf("review-ledger backfill: %s\n", mode)
	fmt.Printf("  candidates missing bindings : %d\n", len(todo))
	fmt.Printf("  bindings recovered          : %d\n", recovered)
	fmt.Printf("  no artifact on disk         : %d\n", noArtifact)
	fmt.Printf("  patch id unrecoverable      : %d  (candidate sha no longer reachable)\n", noPatch)
	fmt.Printf("  no verification recorded    : %d  (reviewer wrote none; correctly gets no digest)\n", noDigest)
	fmt.Printf("  STILL INADMISSIBLE          : %d  (historical lease is unrecoverable and is never invented)\n", stuckNoLease)
	fmt.Println("  a recovered row is marked gate=backfilled-provenance, so a binding")
	fmt.Println("  reconstructed later is always distinguishable from one recorded at launch")
	return nil
}

// indexReviewArtifacts reads the inbox and the ingested archive once, keyed by
// basename, so a corpus of hundreds is not re-walked per candidate.
func indexReviewArtifacts(reviewRoot string) map[string]string {
	out := map[string]string{}
	for _, dir := range []string{
		filepath.Join(reviewRoot, "inbox"),
		filepath.Join(reviewRoot, "inbox", "ingested"),
		filepath.Join(reviewRoot, "ingested"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if _, dup := out[e.Name()]; dup {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				out[e.Name()] = string(b)
			}
		}
	}
	return out
}
