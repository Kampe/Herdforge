package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/Kampe/Herdforge/pkg/config"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
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
	// FAC-673: the PR is what CLOSES, and closure is the event that should retire
	// a worktree. Recording it turns retirement from a periodic sweep into a
	// lifecycle transition. Optional: a launch with no PR yet records what it
	// knows, because an empty field is honest and a fabricated one is not.
	pr := fs.String("pr", "", "pull request this launch's work is proposed through (optional)")
	candidate := fs.String("candidate-sha", "", "exact candidate commit this launch produced (optional)")
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

	// FAC-625: the receipt log is the PROJECT's, not the process cwd's.
	// launch.DefaultSink is cwd-relative, so recording from a lane worktree wrote
	// the receipt into that worktree's log where ingest never looks. Two of the
	// three defects on this card are the same cwd-relative class as FAC-643/646.
	projectRoot, _, rootErr := gitroot.ProjectRoot(context.Background(), ".")
	if rootErr != nil {
		return fmt.Errorf("resolve project root for the receipt log: %w", rootErr)
	}
	sink := &launch.JSONLSink{Path: launch.ReceiptPathFor(projectRoot)}

	// FAC-625: TaskRef defaults to the lane.
	//
	// The standing workflow directs `--lane CHA-####` without `--task-ref`, so
	// TaskRef was written empty and the receipt could not be joined to a card.
	// The lane IS the task identity on that path; requiring the operator to
	// repeat it is a second place to get one fact right.
	recordedTaskRef := strings.TrimSpace(*taskRef)
	if recordedTaskRef == "" {
		recordedTaskRef = strings.TrimSpace(*lane)
	}

	if err := sink.Write(launch.Receipt{
		CreatedAt: time.Now().UTC(),
		TaskRef:   recordedTaskRef,
		// FAC-625: THE defect. family was computed, guarded and PRINTED, but
		// never written. Every manually recorded receipt went out with an empty
		// builder_family, which BuilderFamilyReachingSHA skips -- so the command
		// whose whole purpose is recording provenance recorded everything except
		// provenance, and its success line printed `family=anthropic` while the
		// row on disk proved nothing. Live rows: CHA-3211 21:24:34,
		// CHA-3455 21:16:50, CHA-3466 21:20:25.
		BuilderFamily: family,
		Lane:          strings.TrimSpace(*lane),
		Name:         strings.TrimSpace(*lane),
		Role:         strings.TrimSpace(*role),
		TaskShape:    strings.TrimSpace(*shape),
		Provider:     strings.TrimSpace(*provider),
		Model:        strings.TrimSpace(*model),
		CWD:          strings.TrimSpace(*cwd),
		Branch:       branch,
		PullRequest:  strings.TrimSpace(*pr),
		CandidateSHA: strings.TrimSpace(*candidate),
		Accepted:     true,
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

// recordResolvedLaunchReceipt persists provenance for a standing lane from the
// route that ACTUALLY resolved, before any kickoff text is delivered.
//
// FAC-620: FAC-615 restored provider fallthrough, so a lane configured for
// codex now routinely runs claude. Nothing wrote that down. Chainseer produced
// real work whose builder family could not be proven -- CHA-2582 9d76009de5,
// CHA-3455 ac1ffa7321, CHA-3454 6f6c250d5 and CHA-3456 2ca09828d all have a
// Claude/Anthropic builder and NO launch row -- and the only lane that did have
// a row got it because a worker appended one by hand after the fact, which
// proves nothing about what the launcher resolved.
//
// Every field comes from the DECISION, never from lane config. A receipt built
// from config would name the configured pin after a reroute, and a wrong family
// is worse than a missing one: independence would be computed against a family
// that never wrote the code, and the record would look authoritative while
// being false.
//
// An unmappable family REFUSES rather than recording "unknown", matching the
// manual recorder's existing rule: unprovable authorship must not be written
// down as if it were provenance.
func recordResolvedLaunchReceipt(decision *router.LaunchDecision, lane *config.LaneDef, agentName, cwd, repository string, tabID, paneID string) error {
	if decision == nil {
		return fmt.Errorf("launch receipt requires a resolved decision")
	}
	if lane == nil {
		return fmt.Errorf("launch receipt requires a lane")
	}
	provider := strings.TrimSpace(decision.Provider)
	model := strings.TrimSpace(decision.Model)
	family := router.FamilyFor(provider, model)
	if strings.TrimSpace(family) == "" {
		return fmt.Errorf("resolved route %s/%s maps to no vendor family; refusing to record unprovable authorship for lane %q",
			provider, model, lane.Name)
	}
	branch := gitBranchOf(cwd)
	if branch == "" {
		return fmt.Errorf("cannot resolve a branch in %s; a receipt with no branch cannot be joined to a commit", cwd)
	}
	// FAC-620 P2: TaskRef is the JOIN KEY every consumer uses -- review intake
	// matches on it, and the signed-context path refuses when it does not equal
	// the ref under review. The first version of this writer set Lane and Name
	// and omitted TaskRef entirely, so the receipt was unresolvable by ref and
	// the propagation it was written for could never happen. Caught by
	// independent review.
	//
	// For a standing lane the task identity IS the lane: launch.Request on this
	// path already carries TaskRef: lane.Name, so the receipt agrees with the
	// request that produced it rather than inventing a second convention.
	taskRef := strings.TrimSpace(lane.Name)
	return launch.DefaultSink().Write(launch.Receipt{
		CreatedAt:     time.Now().UTC(),
		TaskRef:       taskRef,
		Lane:          lane.Name,
		Name:          strings.TrimSpace(agentName),
		Role:          strings.TrimSpace(lane.Role),
		TaskShape:     strings.TrimSpace(decision.Shape),
		Provider:      provider,
		Model:         model,
		Effort:        strings.TrimSpace(decision.Effort),
		BuilderFamily: family,
		CWD:           cwd,
		Branch:        branch,
		Repository:    strings.TrimSpace(repository),
		TabID:         strings.TrimSpace(tabID),
		PaneID:        strings.TrimSpace(paneID),
		Accepted:      true,
	})
}
