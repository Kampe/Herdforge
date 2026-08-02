package sync

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// Port of bin/herd-board-done: move a ticket to done ONLY when its work is
// provably on origin/main.
//
// WHY (incident, 2026-07-28, chainseer): marking done is a claim about
// reality, and nothing checked it. Cards were moved to done while the merge
// had been REFUSED by a gate, because the board write was chained behind a
// pipe whose tail exited 0. Proof is by CONTENT, not commit message alone:
// tickets have shipped with zero commits naming them, so an explicit
// --evidence ancestor commit is accepted as first-class proof.

// ErrNoEvidence marks an honest refusal: no proof the work is on origin/main.
var ErrNoEvidence = errors.New("no merge evidence found on origin/main")

var zeroPadRef = regexp.MustCompile(`^([A-Za-z]+-)0+([0-9])`)

// NormalizeRef strips zero padding from a ticket ref: FAC-018 and FAC-18 are
// the same ticket; a padded ref misses the board.
func NormalizeRef(ref string) string {
	return zeroPadRef.ReplaceAllString(ref, "${1}${2}")
}

func git(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// MergeEvidence returns a human-readable proof that ref's work is on
// origin/main, or "" when no proof exists. Order of proof:
//  1. an explicit evidenceSHA that is an ancestor of origin/main (hard error
//     if given but NOT an ancestor — a wrong claim must not fall through), or
//  2. a commit on origin/main naming the ref, with an explicit non-digit
//     boundary so FAC-18 does not match FAC-180 (git's POSIX ERE has no \b).
func MergeEvidence(repoDir, ref, evidenceSHA string) (string, error) {
	// Refresh origin/main; offline is fine, we check against the local ref.
	_, _ = git(repoDir, "fetch", "-q", "origin", "main")
	if _, err := git(repoDir, "rev-parse", "--verify", "-q", "origin/main"); err != nil {
		return "", fmt.Errorf("no origin/main in %s", repoDir)
	}

	if evidenceSHA != "" {
		if _, err := git(repoDir, "merge-base", "--is-ancestor", evidenceSHA, "origin/main"); err != nil {
			return "", fmt.Errorf("REFUSING: evidence %s is not an ancestor of origin/main", evidenceSHA)
		}
		short, _ := git(repoDir, "rev-parse", "--short", evidenceSHA)
		return fmt.Sprintf("explicit evidence commit %s is an ancestor of origin/main", short), nil
	}

	hit, err := git(repoDir, "log", "origin/main", "--format=%h %s", "-E",
		"--grep="+ref+`([^0-9]|$)`, "-1")
	if err == nil && hit != "" {
		return fmt.Sprintf("origin/main carries a commit naming %s: %s", ref, hit), nil
	}
	return "", nil
}

// DoneResult reports what BoardDone did and why it was allowed to.
type DoneResult struct {
	Ref           string
	TaskID        string
	Proof         string
	Forced        bool
	CommentPosted bool
}

// BoardDone moves the ticket with the given ref to done, gated on merge
// evidence, and verifies the write by READ-BACK: board APIs are known to
// report success on writes that did not persist.
func BoardDone(ctx context.Context, tp provider.TaskProvider, repoDir, projectID, ref, evidenceSHA string, force bool) (*DoneResult, error) {
	ref = NormalizeRef(ref)

	proof, err := MergeEvidence(repoDir, ref, evidenceSHA)
	if err != nil {
		return nil, err
	}
	if proof == "" {
		if !force {
			return nil, fmt.Errorf("%w for %s: no commit on origin/main names it and no evidence commit was given; "+
				"if the work truly landed without naming the ref, prove it by content: herd board-done %s --evidence <sha>",
				ErrNoEvidence, ref, ref)
		}
		proof = "operator --force, no automatic evidence found"
	}

	tasks, err := tp.ListTasks(ctx, projectID, "")
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	var task *provider.Task
	for _, t := range tasks {
		if strings.EqualFold(NormalizeRef(t.Ref), ref) {
			task = t
			break
		}
	}
	if task == nil {
		return nil, fmt.Errorf("no task with ref %s on the board", ref)
	}

	if err := tp.UpdateStatus(ctx, task.ID, "done"); err != nil {
		return nil, fmt.Errorf("status write for %s: %w", ref, err)
	}

	back, err := tp.GetTask(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("read-back for %s failed after status write: %w", ref, err)
	}
	if back.Status != "done" {
		return nil, fmt.Errorf("write reported success but %s reads back as %q", ref, back.Status)
	}

	res := &DoneResult{Ref: ref, TaskID: task.ID, Proof: proof, Forced: force && !strings.Contains(proof, "origin/main")}
	if err := tp.AddComment(ctx, task.ID, "board-done: "+proof); err == nil {
		res.CommentPosted = true
	}
	return res, nil
}
