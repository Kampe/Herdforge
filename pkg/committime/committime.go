// Package committime owns one decision: when a commit was CREATED.
//
// FAC-620. Two callers were asking git that question, and the answer is a
// policy, not vocabulary:
//
//   - Committer time (%cI), never author time (%aI). `git commit --amend` and
//     `git cherry-pick` both PRESERVE the original author timestamp and stamp a
//     new committer timestamp. Reading %aI made an amended commit look as
//     though it predated the launch that produced it, so the receipt of the
//     lane that actually wrote the code was rejected as "recorded after the
//     commit" -- and a rejected receipt left the reviewer's asserted builder
//     family unchecked. The guard reopened the hole it existed to close.
//
//   - An unanswerable question is not a time. A missing repository, an unknown
//     SHA or an unparseable stamp yields the zero time, which every caller must
//     read as "no provenance" -- never as licence to attribute on some weaker
//     signal.
//
// pkg/invariant flags a decision written in more than one place, because that
// is the root cause behind FAC-562, 565, 569, 571, 573 and 574: the copies
// diverge and a fix lands on only one of them. That gate caught this exact
// duplication in CI -- %cI in both cmd/herd/reviewingest.go and
// pkg/candidateindex/index.go -- which is precisely the failure mode it exists
// for, since the author/committer distinction had already been got wrong once
// and would then have had two places to be wrong in.
package committime

import (
	"os/exec"
	"strings"
	"time"
)

// Of returns when the commit object was created, or the zero time when that
// cannot be established. root may be empty to use the process's working
// directory.
func Of(root, sha string) time.Time {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return time.Time{}
	}
	args := []string{}
	if r := strings.TrimSpace(root); r != "" {
		args = append(args, "-C", r)
	}
	args = append(args, "show", "-s", "--format=%cI", sha)

	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return time.Time{}
	}
	return t
}
