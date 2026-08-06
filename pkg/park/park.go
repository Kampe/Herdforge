// Package park ports bin/herd-park: make parked work DURABLE, and audit
// whether it already is.
//
// A lane parks unfinished work as a `wip(parked):` commit on its branch. That
// commit's only ref is then refs/heads/<branch>. The next thing that happens to
// a lane after a harvest is a `reset --hard` to origin/main — at that moment the
// parked commit becomes reflog-only, and a `git gc` deletes it for good.
//
// chainseer observed this twice in one hour on 2026-07-25: one lane's parked
// work survived only in the reflog, and three others sat in the same exposure
// unnoticed, because a `wip(parked):` commit LOOKS preserved.
//
// Three properties make parked work durable, and a branch has none of them:
//
//  1. A TAG, not a branch — a rebase or reset moves a branch, never a tag.
//  2. ANNOTATED, so resume context travels with the object instead of living in
//     a chat message that scrolls away.
//  3. PUSHED, so it survives local gc, worktree removal, and a fresh clone.
//
// Audit is the important verb: it answers "is anything parked reachable from
// nothing but a branch", and it exits non-zero so a heartbeat can surface it
// without a human remembering to look.
package park

import (
	"fmt"
	"os/exec"
	"strings"
)

// Durability classifies one parked commit.
type Durability string

const (
	// Durable: reachable from a tag that exists on the remote.
	Durable Durability = "DURABLE"
	// LocalTagOnly: tagged, but the tag was never pushed. Survives a reset,
	// not a fresh clone or a lost checkout.
	LocalTagOnly Durability = "LOCAL-TAG-ONLY"
	// Exposed: reachable from a branch and nothing else. One reset --hard
	// from gone.
	Exposed Durability = "EXPOSED"
)

// Finding is one parked commit and how exposed it is.
type Finding struct {
	SHA        string
	Branch     string
	Subject    string
	Tags       []string
	Durability Durability
}

// TagPrefix is the namespace for durable park tags.
const TagPrefix = "parked/"

// Repo runs git in one checkout.
type Repo struct{ Dir string }

func (r Repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// IsParkedSubject matches the commit subjects that mark deliberately
// unfinished work. Anything else on a branch is ordinary in-flight work and is
// not park's business.
func IsParkedSubject(subject string) bool {
	s := strings.ToLower(strings.TrimSpace(subject))
	return strings.HasPrefix(s, "wip(parked)") ||
		strings.HasPrefix(s, "wip:") ||
		strings.HasPrefix(s, "parked:")
}

// Classify decides how durable a parked commit is. Pure, so the policy is
// testable without a repository.
func Classify(tags []string, pushed bool) Durability {
	switch {
	case len(tags) == 0:
		return Exposed
	case !pushed:
		return LocalTagOnly
	default:
		return Durable
	}
}

// Audit reports every parked commit and its durability. Branches are scanned
// against origin/main; a commit already on main is not parked work.
func (r Repo) Audit() ([]Finding, error) {
	branches, err := r.git("for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil, fmt.Errorf("park: list branches: %w", err)
	}
	seen := map[string]bool{}
	var findings []Finding

	for _, b := range strings.Split(branches, "\n") {
		b = strings.TrimSpace(b)
		if b == "" || b == "main" {
			continue
		}
		log, err := r.git("log", "--format=%h%x09%s", "origin/main.."+b)
		if err != nil || log == "" {
			continue
		}
		for _, line := range strings.Split(log, "\n") {
			sha, subject, ok := strings.Cut(line, "\t")
			if !ok || sha == "" || !IsParkedSubject(subject) || seen[sha] {
				continue
			}
			seen[sha] = true
			tags := r.tagsContaining(sha)
			findings = append(findings, Finding{
				SHA: sha, Branch: b, Subject: subject, Tags: tags,
				Durability: Classify(tags, r.anyTagPushed(tags)),
			})
		}
	}
	return findings, nil
}

func (r Repo) tagsContaining(sha string) []string {
	out, err := r.git("for-each-ref", "--contains", sha, "--format=%(refname:short)", "refs/tags/")
	if err != nil || out == "" {
		return nil
	}
	var tags []string
	for _, t := range strings.Split(out, "\n") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

func (r Repo) anyTagPushed(tags []string) bool {
	for _, t := range tags {
		if _, err := r.git("ls-remote", "--exit-code", "--tags", "origin", t); err == nil {
			return true
		}
	}
	return false
}

// ExposedCount is how many findings are not durable. Callers exit non-zero on
// a positive count so a heartbeat surfaces the exposure automatically.
func ExposedCount(findings []Finding) int {
	n := 0
	for _, f := range findings {
		if f.Durability != Durable {
			n++
		}
	}
	return n
}

// Park makes one commit durable: annotated tag, then push. The push is part of
// the operation, not a follow-up — a local-only tag still dies with the
// checkout, which is one of the three failure modes this exists to prevent.
func (r Repo) Park(slug, sha, message string) (string, error) {
	if strings.TrimSpace(slug) == "" || strings.TrimSpace(sha) == "" {
		return "", fmt.Errorf("park: slug and sha are required")
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("park: -m <message> is required; the resume context must travel with the tag")
	}
	if _, err := r.git("rev-parse", "--verify", "--quiet", sha+"^{commit}"); err != nil {
		return "", fmt.Errorf("park: %s is not a commit in this repository", sha)
	}
	tag := TagPrefix + slug
	if _, err := r.git("rev-parse", "--verify", "--quiet", "refs/tags/"+tag); err == nil {
		return "", fmt.Errorf("park: tag %s already exists; choose another slug rather than moving a durable tag", tag)
	}
	if _, err := r.git("tag", "-a", tag, sha, "-m", message); err != nil {
		return "", fmt.Errorf("park: annotate %s: %w", tag, err)
	}
	if _, err := r.git("push", "origin", "refs/tags/"+tag); err != nil {
		return tag, fmt.Errorf("park: tagged %s locally but PUSH FAILED — still exposed to local gc: %w", tag, err)
	}
	return tag, nil
}

// List returns every park tag with its subject.
func (r Repo) List() ([]string, error) {
	out, err := r.git("for-each-ref", "--format=%(refname:short)%09%(contents:subject)", "refs/tags/"+TagPrefix+"*")
	if err != nil {
		return nil, err
	}
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			rows = append(rows, l)
		}
	}
	return rows, nil
}
