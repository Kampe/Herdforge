package main

import (
	"os/exec"
	"strings"
)

// One definition each for two git questions this tree kept re-asking.
//
// FAC-669: pkg/invariant flags a decision written in more than one place,
// because that is the root cause behind FAC-562, 565, 569, 571, 573 and 574 --
// the copies diverge and a fix lands on only one of them. This session produced
// three more of exactly that shape: a workspace pinned in four places until one
// went stale, a task identity written from two sources so they could never
// agree, and a cache threshold that did not match the gate it served.
//
// These are small on purpose. The value is not the saved lines, it is that the
// next change to "how we ask git whether X contains Y" has one place to land.

// commitIsAncestor reports whether sha is contained in ref.
//
// Reachability, not equality: a commit produced on a branch is contained by it
// even when it is no longer the tip. An error is NOT containment -- an
// unanswerable question is never a yes.
func commitIsAncestor(root, sha, ref string) bool {
	sha = strings.TrimSpace(sha)
	ref = strings.TrimSpace(ref)
	if sha == "" || ref == "" {
		return false
	}
	args := []string{}
	if strings.TrimSpace(root) != "" {
		args = append(args, "-C", root)
	}
	args = append(args, "merge-base", "--is-ancestor", sha, ref)
	return exec.Command("git", args...).Run() == nil
}
