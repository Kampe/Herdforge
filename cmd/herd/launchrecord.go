package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
)

// runLaunchRecord records that a lane was launched with a specific provider on a
// specific worktree, so the commits it produces have provable authorship.
//
// FAC-637: nothing recorded this. The fleet dispatches builders through
// herdr-dispatch, which launches via herdr-agent-tab and never touches
// Herdforge, so .herd/launch-receipts.jsonl held 10 rows, all claude, days stale,
// while the fleet ran grok and codex.
//
// The consequence was not cosmetic. Builder provenance for every new commit was
// unrecorded, so 28 reviewed-and-PASSING pull requests could not claim
// cross-family independence, and the class grew with every commit. Reviewers were
// asked to attest authorship nothing had ever written down, and the honest ones
// wrote "unknown" and had their reviews refused for it.
//
// A receipt saying "lane X ran provider grok on branch Y", plus a commit that is
// on branch Y, is traceable provenance. That is materially different from the
// domain inference a reviewer correctly refused ("apps/api is nominally
// api-crusader's territory") -- it is the launch record for the branch the commit
// actually sits on.
func runLaunchRecord() error {
	fs := flag.NewFlagSet("launch-record", flag.ContinueOnError)
	lane := fs.String("lane", "", "lane or agent name that was launched")
	cwd := fs.String("cwd", "", "worktree the lane runs in")
	provider := fs.String("provider", "", "resolved provider (claude|codex|grok|agy)")
	model := fs.String("model", "", "resolved model")
	taskRef := fs.String("task-ref", "", "optional card ref")
	role := fs.String("role", "worker", "lane role")
	shape := fs.String("task-shape", "implementation", "task shape")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	for name, v := range map[string]string{"--lane": *lane, "--cwd": *cwd, "--provider": *provider} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	family := router.FamilyFor(*provider, *model)
	if family == "" {
		// Refuse rather than write a receipt that cannot establish provenance: an
		// unmappable provider recorded as authorship would be an assertion, which
		// is exactly what the review gate exists to refuse.
		return fmt.Errorf("provider %q model %q maps to no vendor family; refusing to record unprovable authorship",
			*provider, *model)
	}
	branch := gitBranchOf(*cwd)
	if branch == "" {
		return fmt.Errorf("cannot resolve a branch in %s; a receipt with no branch cannot be joined to a commit", *cwd)
	}

	sink := launch.DefaultSink()
	if err := sink.Write(launch.Receipt{
		CreatedAt: time.Now().UTC(),
		TaskRef:   strings.TrimSpace(*taskRef),
		Lane:      strings.TrimSpace(*lane),
		Name:      strings.TrimSpace(*lane),
		Role:      strings.TrimSpace(*role),
		TaskShape: strings.TrimSpace(*shape),
		Provider:  strings.TrimSpace(*provider),
		Model:     strings.TrimSpace(*model),
		CWD:       strings.TrimSpace(*cwd),
		Branch:    branch,
		Accepted:  true,
	}); err != nil {
		return fmt.Errorf("write launch receipt: %w", err)
	}
	fmt.Printf("launch-record: lane=%s branch=%s provider=%s family=%s cwd=%s\n",
		*lane, branch, *provider, family, *cwd)
	return nil
}

// gitBranchOf is the detached-aware form of the canonical currentBranch().
//
// FAC-669: this re-implemented `rev-parse --abbrev-ref HEAD`, which FAC-556 had
// already extracted precisely so "the branch here" could not mean two different
// things. The duplicate-rule gate caught it. It now delegates, and keeps only
// the one behaviour it genuinely adds: a detached HEAD answers the literal
// string "HEAD", which is not a branch, and recording that as one is the shape
// that produced review artifacts naming a branch that did not contain their own
// SHA.
func gitBranchOf(dir string) string {
	b, err := currentBranch(dir)
	if err != nil {
		return ""
	}
	b = strings.TrimSpace(b)
	if b == "HEAD" {
		return ""
	}
	return b
}
