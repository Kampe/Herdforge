// Package finish contains the read-only post-merge completion gate.
package finish

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var sha40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Evidence is the coordinator's observed post-merge state. The gate is pure
// so every refusal can be table-tested without a provider, git, or filesystem.
type Evidence struct {
	Ref, LandedSHA, ReceiptRef, ReceiptDigest    string
	ReceiptCandidateSHA, ReceiptMergeSHA         string
	ReceiptVerdict, AuthorFamily, ReviewerFamily string
	ReceiptIntegration                           string
	ReceiptValid, ReviewPass, ChecksPass         bool
	LandedOnMain, Clean, BranchRemoved           bool
	WorktreeRemoved, UniqueWork                  bool
}

// Result is a stable, machine-readable completion decision.
type Result struct {
	Ready   bool     `json:"ready"`
	Reasons []string `json:"reasons,omitempty"`
}

// Evaluate refuses unless every independent post-merge proof is present.
func Evaluate(e Evidence) Result {
	r := Result{Ready: true}
	ref := strings.TrimSpace(e.Ref)
	if ref == "" {
		r.add("task ref is required")
	}
	if !sha40.MatchString(e.LandedSHA) {
		r.add("landed SHA must be the exact 40-character commit SHA")
	}
	if e.ReceiptRef == "" || !strings.EqualFold(e.ReceiptRef, ref) {
		r.add("receipt is not bound to the exact task ref")
	}
	if !e.ReceiptValid {
		r.add("completion receipt is missing or invalid")
	}
	if !sha40.MatchString(e.ReceiptCandidateSHA) {
		r.add("receipt candidate SHA is not exact")
	}
	if !sha40.MatchString(e.ReceiptMergeSHA) || e.ReceiptMergeSHA != e.LandedSHA {
		r.add("receipt merge SHA does not equal the exact landed SHA")
	}
	if e.ReceiptVerdict != "PASS" {
		r.add("receipt review verdict is not PASS")
	}
	if e.AuthorFamily == "" || e.ReviewerFamily == "" || e.AuthorFamily == e.ReviewerFamily {
		r.add("receipt lacks an independent cross-family review")
	}
	if e.ReceiptIntegration != "merged" {
		r.add("receipt does not prove merged integration")
	}
	if !e.ReviewPass {
		r.add("exact candidate has no admissible review PASS")
	}
	if !e.ChecksPass {
		r.add("required post-merge checks did not pass")
	}
	if !e.LandedOnMain {
		r.add("landed SHA is not on origin/main")
	}
	if !e.Clean {
		r.add("coordinator worktree is dirty")
	}
	if e.UniqueWork {
		r.add("unique work remains in the branch/worktree")
	}
	if !e.BranchRemoved {
		r.add("task branch has not been cleaned up")
	}
	if !e.WorktreeRemoved {
		r.add("task worktree has not been cleaned up")
	}
	return r
}

func (r *Result) add(reason string) { r.Ready = false; r.Reasons = append(r.Reasons, reason) }

var ErrNotReady = errors.New("post-merge completion is not ready")

func (r Result) Error() error {
	if r.Ready {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrNotReady, strings.Join(r.Reasons, "; "))
}
