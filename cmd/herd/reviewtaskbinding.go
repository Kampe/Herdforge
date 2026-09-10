package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// runReviewTaskBind appends a narrowly authenticated correction for an
// existing verdict's closeable card task. It never changes the verdict or
// rewrites the append-only ledger.
func runReviewTaskBind(ledger *reviewledger.Ledger) {
	fs := flag.NewFlagSet("review-ledger task-bind", flag.ExitOnError)
	artifactPath := fs.String("artifact", "", "correction artifact containing the exact sha, reviewer, task, and reassesses digest")
	previousTask := fs.String("previous-task", "", "task ref recorded by the prior verdict")
	fs.Parse(os.Args[3:])
	if *artifactPath == "" || *previousTask == "" {
		fmt.Fprintln(os.Stderr, "Usage: herd review-ledger task-bind --artifact FILE --previous-task FAC-N")
		os.Exit(2)
	}
	body, err := os.ReadFile(*artifactPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review-ledger task-bind: read artifact: %v\n", err)
		os.Exit(1)
	}
	a := reviewingest.Parse(string(body))
	if len(a.UnknownHeaders) != 0 || a.MalformedHeaderRegion || len(a.ConflictingHeaders) != 0 {
		fmt.Fprintln(os.Stderr, "review-ledger task-bind: malformed or ambiguous artifact headers")
		os.Exit(1)
	}
	if a.Verdict != "PASS" {
		fmt.Fprintln(os.Stderr, "review-ledger task-bind: correction must refer to an existing PASS verdict")
		os.Exit(1)
	}
	digest := sha256.Sum256(body)
	if err := ledger.BindTask(reviewledger.TaskBindingOpts{
		SHA:              a.SHA,
		Reviewer:         a.Reviewer,
		PreviousTask:     *previousTask,
		Task:             a.TaskRef,
		PriorEventDigest: a.Reassesses,
		Artifact:         *artifactPath,
		ArtifactDigest:   hex.EncodeToString(digest[:]),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "review-ledger task-bind: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("task-binding appended sha=%s reviewer=%s task=%s\n", a.SHA, a.Reviewer, a.TaskRef)
}
