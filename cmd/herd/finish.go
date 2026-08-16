package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/finish"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

// runFinish is coordinator-only and deliberately has no provider dependency:
// it reports readiness for approve; harvest/merge/board transitions remain
// separate authorities.
func runFinish() {
	fs := flag.NewFlagSet("finish", flag.ExitOnError)
	landed := fs.String("landed-sha", "", "exact SHA landed on origin/main (required)")
	receiptPath := fs.String("receipt", "", "completion receipt path")
	branch := fs.String("branch", "", "task branch, which must be removed")
	worktree := fs.String("worktree", "", "task worktree, which must be removed")
	build := fs.String("build", "go build ./...", "required build check")
	test := fs.String("test", "go test ./...", "required test check")
	jsonOut := fs.Bool("json", false, "emit JSON")
	args := os.Args[2:]
	ref := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ref, args = args[0], args[1:]
	}
	fs.Parse(args)
	if ref == "" || *landed == "" {
		fmt.Fprintln(os.Stderr, "usage: herd finish <ref> --landed-sha <exact-sha> [--receipt path] [--branch branch] [--worktree path]")
		os.Exit(2)
	}
	if *receiptPath == "" {
		*receiptPath = hsync.ReceiptPath(".", ref)
	}
	if *branch == "" {
		*branch = "herd/" + strings.ToLower(ref)
	}
	if *worktree == "" {
		*worktree = filepath.Join(".herd", "worktrees", strings.ToLower(ref))
	}

	e := finish.Evidence{Ref: ref, LandedSHA: strings.TrimSpace(*landed), ReceiptValid: false}
	receipt, err := hsync.LoadReceipt(*receiptPath)
	if err == nil {
		e.ReceiptRef, e.ReceiptDigest = receipt.TaskRef, receipt.Digest
		e.ReceiptCandidateSHA, e.ReceiptMergeSHA = receipt.CandidateSHA, receipt.MergeSHA
		e.ReceiptVerdict, e.AuthorFamily, e.ReviewerFamily = receipt.Verdict, receipt.AuthorFamily, receipt.ReviewerFamily
		e.ReceiptIntegration = receipt.IntegrationResult
		e.ReceiptValid = receipt.Digest != "" && receipt.Digest == receipt.ComputeDigest()
	}
	e.ReviewPass = receipt != nil && e.ReceiptValid && receipt.Verdict == "PASS" && receipt.CandidateSHA != ""
	e.LandedOnMain = gitOK(".", "merge-base", "--is-ancestor", e.LandedSHA, "origin/main")
	e.Clean = finishGitOutput(".", "status", "--porcelain") == ""
	e.BranchRemoved = !gitOK(".", "show-ref", "--verify", "--quiet", "refs/heads/"+*branch)
	e.WorktreeRemoved = !pathExists(*worktree)
	// A surviving branch or worktree is a hard refusal; checks are only run
	// after cheap identity/cleanup guards to avoid doing work on bad evidence.
	if e.Clean && e.LandedOnMain && e.BranchRemoved && e.WorktreeRemoved {
		b, berr := verifier.NewVerifier(*build).Execute(context.Background(), ".")
		t, terr := verifier.NewVerifier(*test).Execute(context.Background(), ".")
		e.ChecksPass = berr == nil && terr == nil && b != nil && t != nil && b.Passed && t.Passed
	}
	result := finish.Evaluate(e)
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(result)
	} else if result.Ready {
		fmt.Printf("herd finish: %s READY-FOR-APPROVE landed=%s\n", ref, e.LandedSHA)
	} else {
		fmt.Printf("herd finish: %s REFUSED\n", ref)
		for _, reason := range result.Reasons {
			fmt.Printf("  - %s\n", reason)
		}
	}
	if !result.Ready {
		os.Exit(1)
	}
}

func gitOK(dir string, args ...string) bool {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).Run() == nil
}
func finishGitOutput(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "\x00"
	}
	return strings.TrimSpace(string(out))
}
func pathExists(path string) bool { _, err := os.Stat(path); return err == nil }
