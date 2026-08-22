package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// runPoolReview exposes the warm-pool reviewer path for use while the signed
// review admission path is unavailable. The lease remains held for the
// review supervisor to release after verdict ingest.
func runPoolReview(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return errors.New("candidate ref is required (usage: herd review <ref> --pool)")
	}
	parsedRef, parsedSHA, err := parseReviewPoolArgs(os.Args[2:])
	if err != nil {
		return err
	}
	if parsedRef != "" {
		ref = parsedRef
	}
	fs := flag.NewFlagSet("review --pool", flag.ContinueOnError)
	opts := registerPoolReviewFlags(fs)
	if err := fs.Parse(leadingPositionalArgs(os.Args[2:])); err != nil {
		return err
	}
	shaFlag, provider, model := opts.SHA, opts.Provider, opts.Model
	excludeFamily, poolRoot := opts.ExcludeFamily, opts.PoolRoot
	surfaceRoot, packetRoot, noLaunch := opts.SurfaceRoot, opts.PacketRoot, opts.NoLaunch
	if strings.TrimSpace(*shaFlag) == "" {
		*shaFlag = parsedSHA
	}
	// Argument validation is the cheapest gate, so it runs before any git or
	// pool work: a malformed option must not be reported only after candidate
	// resolution has already failed for an unrelated reason.
	if err := opts.Validate(); err != nil {
		return err
	}
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	candidateDir, err := resolvePoolReviewCandidate(root, ref)
	if err != nil {
		return err
	}
	sha := strings.TrimSpace(*shaFlag)
	if sha == "" {
		out, err := exec.Command("git", "-C", candidateDir, "rev-parse", "HEAD").Output()
		if err != nil {
			return fmt.Errorf("resolve candidate HEAD: %w", err)
		}
		sha = strings.TrimSpace(string(out))
	}
	if len(sha) < 12 {
		return fmt.Errorf("candidate SHA %q is too short", sha)
	}
	if out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", sha+"^{commit}").CombinedOutput(); err != nil {
		return fmt.Errorf("candidate %s is not a commit: %s", sha[:min(12, len(sha))], strings.TrimSpace(string(out)))
	}

	// FAC-577: resolve AND preflight the reviewer before the pool lease, not
	// just before the tab. A launch that dies on an unusable provider used to
	// take a warm-pool slot and a tab with it; the previous change only moved
	// this ahead of the tab create, so the lease was still acquired first.
	//
	// Skipped when --no-launch: that path deliberately prepares a surface
	// without starting a harness, so provider readiness is not its business.
	reviewer := poolReviewer{}
	if !*noLaunch {
		resolved, err := resolvePoolReviewer(*provider, *model, *excludeFamily)
		if err != nil {
			return err
		}
		if err := preflightReviewerReadiness(resolved); err != nil {
			return err
		}
		reviewer = resolved
	}

	p := worktree.NewPool(root, *poolRoot, 2)
	if err := p.Ensure(context.Background()); err != nil {
		return err
	}
	lease, err := p.Lease(context.Background(), "review-"+safeReviewSurfacePart(ref)+"-"+shortSHA(sha))
	if err != nil {
		return err
	}
	releaseOnFailure := true
	defer func() {
		if releaseOnFailure {
			_ = p.Release(context.Background(), lease.LeaseID)
		}
	}()
	if out, err := exec.Command("git", "-C", lease.Path, "reset", "--hard", sha).CombinedOutput(); err != nil {
		return fmt.Errorf("pin pool slot %s: %v (%s)", lease.Name, err, strings.TrimSpace(string(out)))
	}

	surfaceName := "review-" + safeReviewSurfacePart(ref) + "-" + shortSHA(sha)
	surface := filepath.Join(*surfaceRoot, surfaceName)
	if err := os.MkdirAll(*surfaceRoot, 0o755); err != nil {
		return fmt.Errorf("create review surface root: %w", err)
	}
	if info, statErr := os.Lstat(surface); statErr == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("review surface %q exists and is not a symlink", surface)
		}
		if err := os.Remove(surface); err != nil {
			return fmt.Errorf("replace review surface symlink: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect review surface: %w", statErr)
	}
	relTarget, err := filepath.Rel(filepath.Dir(surface), lease.Path)
	if err != nil {
		return fmt.Errorf("make repo-relative review surface target: %w", err)
	}
	if err := os.Symlink(relTarget, surface); err != nil {
		return fmt.Errorf("create review surface symlink: %w", err)
	}

	packet := filepath.Join(*packetRoot, surfaceName+".md")
	if err := os.MkdirAll(*packetRoot, 0o755); err != nil {
		return fmt.Errorf("create review packet root: %w", err)
	}
	packetBody := fmt.Sprintf("REVIEW %s — verdict only, edit nothing.\n\nCandidate: %s\nSurface: %s\nRead docs/prompts/review-contract.md, inspect only this candidate, and report the required verdict artifact to the review supervisor.\n", ref, sha, surface)
	if err := os.WriteFile(packet, []byte(packetBody), 0o600); err != nil {
		return fmt.Errorf("write review packet: %w", err)
	}

	if *noLaunch {
		fmt.Printf("review surface ready ref=%s sha=%s lease=%s path=%s packet=%s\n", ref, shortSHA(sha), lease.LeaseID, surface, packet)
		return nil
	}
	if !herdr.IsAvailable() {
		return errors.New("herdr CLI is unavailable; use --no-launch to prepare the surface")
	}
	ws, err := herdr.RequireWorkspace(root)
	if err != nil {
		return err
	}
	tabLabel := reviewTabLabel(ref, sha)
	tab, err := herdr.TabCreate(herdr.TabCreateOptions{Workspace: ws, Label: tabLabel, Cwd: surface, NoFocus: true, Env: []string{herdr.AgentRoleEnv}})
	if err != nil {
		return fmt.Errorf("create reviewer tab: %w", err)
	}
	agentName := reviewAgentName(ref, sha)
	cleanupTab := true
	defer func() {
		if cleanupTab {
			// StartReviewAgent and the prompt path both use the exact tab/name
			// identity; a failed launch must never leave a pool orphan behind.
			_ = herdr.CloseReviewTab(tab.ID, agentName)
		}
	}()
	if err := herdr.StartReviewAgent(tab.ID, agentName, tab.Pane.ID, reviewer.Kind, reviewer.Argv...); err != nil {
		return fmt.Errorf("start %s reviewer (%s): %w", reviewer.Kind, reviewer.Model, err)
	}
	if _, err := herdr.Send(agentName, "Read and execute the review packet at "+packet+" in full.", true, 30*time.Second); err != nil {
		return errors.Join(fmt.Errorf("deliver review packet: %w", err), herdr.CloseReviewTab(tab.ID, agentName))
	}
	cleanupTab = false
	releaseOnFailure = false
	fmt.Printf("reviewer launched ref=%s sha=%s lease=%s surface=%s tab=%s agent=%s packet=%s harness=%s provider=%s model=%s family=%s\n", ref, shortSHA(sha), lease.LeaseID, surface, tabLabel, agentName, packet, reviewer.Kind, reviewer.Provider, reviewer.Model, reviewer.Family)
	return nil
}

// resolvePoolReviewCandidate first preserves the dispatched ticket convention,
// then resolves a real checked-out branch from Git's worktree metadata. Branch
// names are data passed to Git, never path fragments appended to the repo root.
func resolvePoolReviewCandidate(root, ref string) (string, error) {
	if filepath.Base(ref) == ref && !strings.ContainsAny(ref, `/\\`) {
		candidateDir := filepath.Join(root, worktreePathForRef(ref))
		if worktreeExists(candidateDir) {
			return candidateDir, nil
		}
	}

	branchPath, err := checkedOutBranchWorktree(root, ref)
	if err != nil {
		return "", err
	}
	if branchPath != "" {
		return branchPath, nil
	}
	if filepath.Base(ref) != ref || strings.ContainsAny(ref, `/\\`) {
		return "", fmt.Errorf("candidate branch %q is not checked out in a worktree", ref)
	}
	return "", fmt.Errorf("candidate worktree %q does not exist", filepath.Join(root, worktreePathForRef(ref)))
}

func checkedOutBranchWorktree(root, ref string) (string, error) {
	branchRef := "refs/heads/" + ref
	if _, err := exec.Command("git", "-C", root, "rev-parse", "--verify", branchRef+"^{commit}").Output(); err != nil {
		return "", nil
	}
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return "", fmt.Errorf("list Git worktrees: %w", err)
	}

	var path, branch string
	match := func() string {
		if branch == branchRef && worktreeExists(path) {
			return path
		}
		return ""
	}
	reset := func() {
		path = ""
		branch = ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			branch = strings.TrimPrefix(line, "branch ")
		case line == "":
			if matched := match(); matched != "" {
				return matched, nil
			}
			reset()
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read Git worktrees: %w", err)
	}
	if matched := match(); matched != "" {
		return matched, nil
	}
	return "", nil
}

// parseReviewPoolArgs parses the review command's pool selector and the
// options that are meaningful only on the warm-pool path. The outer review
// parser also registers these flags so ExitOnError does not reject them before
// this ContinueOnError parser can produce a normal error.
func parseReviewPoolArgs(args []string) (ref, sha string, err error) {
	fs := flag.NewFlagSet("review --pool", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	shaFlag := registerPoolReviewFlags(fs).SHA
	if err := fs.Parse(leadingPositionalArgs(args)); err != nil {
		return "", "", err
	}
	if fs.NArg() > 0 {
		ref = fs.Arg(0)
	}
	return ref, *shaFlag, nil
}

func safeReviewSurfacePart(ref string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(ref)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	part := strings.Trim(b.String(), "-")
	for strings.Contains(part, "--") {
		part = strings.ReplaceAll(part, "--", "-")
	}
	if part == "" {
		return "candidate"
	}
	return part
}

// poolReviewOptions is THE definition of the `herd review --pool` option
// schema.
//
// FAC-574: this schema existed in THREE hand-maintained copies -- the outer
// parseReviewArgs in main.go, parseReviewPoolArgs here, and runPoolReview's own
// operational FlagSet. Adding --provider/--exclude-family to only the
// operational copy made the native route unreachable: the outer ExitOnError
// parser rejected `flag provided but not defined: -provider` before any lease
// or tab existed. Every parsing pass now registers from this one function, so a
// new option cannot be accepted by one pass and refused by another.
type poolReviewOptions struct {
	Pool          *bool
	SHA           *string
	Provider      *string
	Model         *string
	ExcludeFamily *string
	PoolRoot      *string
	SurfaceRoot   *string
	PacketRoot    *string
	NoLaunch      *bool
}

// Validate rejects option combinations that are not routes. It lives on the
// schema so every parsing pass shares one notion of a valid pool command line.
func (o *poolReviewOptions) Validate() error {
	if strings.TrimSpace(*o.Model) != "" && strings.TrimSpace(*o.Provider) == "" {
		return errors.New("--model requires --provider (a model without its surface is not a route)")
	}
	return nil
}

// registerPoolReviewFlags registers the complete pool option schema on fs.
// Callers that only need the command line accepted may discard the result.
func registerPoolReviewFlags(fs *flag.FlagSet) *poolReviewOptions {
	return &poolReviewOptions{
		Pool:     fs.Bool("pool", false, "Select the warm-pool review path"),
		SHA:      fs.String("sha", "", "Exact candidate commit (default: HEAD of .herd/worktrees/<ref>)"),
		Provider: fs.String("provider", "", "Force a reviewer provider (default: router pick)"),
		Model:    fs.String("model", "", "Force an exact reviewer model (requires --provider)"),
		ExcludeFamily: fs.String("exclude-family", "",
			"Refuse a model family (use the builder's family for a disjoint review)"),
		PoolRoot:    fs.String("pool-root", filepath.Join(".herd", "pool"), "Warm review pool root"),
		SurfaceRoot: fs.String("surface-root", filepath.Join(".herd", "review-surfaces"), "Review surface symlink root"),
		PacketRoot:  fs.String("packet-root", filepath.Join(".herd", "review-packets"), "Review packet root"),
		NoLaunch:    fs.Bool("no-launch", false, "Prepare and print the surface without starting Herdr"),
	}
}

const reviewAgentNameLimit = 32

func reviewAgentName(ref, sha string) string {
	// FAC-574: the truncation suffix used to hash the REF ONLY, so two distinct
	// SHAs on the same branch produced the SAME agent name and the second
	// reviewer collided with the first still-active one. Identity must include
	// the candidate, because reviewing a second exact SHA on one branch is
	// normal and is the whole point of exact-SHA review.
	//
	// reviewTabLabel three lines below already hashed ref+sha correctly, so the
	// right implementation was adjacent to the wrong one. Both now call one
	// definition.
	return truncatedReviewIdentity(ref, sha)
}

func reviewTabLabel(ref, sha string) string {
	// Herdr currently accepts the same 1-32 character surface used by agent
	// names, but keep this policy separate so a future tab-label limit can
	// change without changing agent identity semantics.
	return truncatedReviewIdentity(ref, sha)
}

// truncatedReviewIdentity builds a bounded identity that stays unique per
// (ref, sha). The disambiguating hash covers BOTH, so truncation can never
// merge two candidates on one branch into one identity.
func truncatedReviewIdentity(ref, sha string) string {
	base := "review-" + safeReviewSurfacePart(ref) + "-" + shortSHA(sha)
	if len(base) <= reviewAgentNameLimit {
		return base
	}
	suffix := fmt.Sprintf("-%x", sha256.Sum256([]byte(strings.TrimSpace(ref)+"\x00"+strings.TrimSpace(sha))))[:9]
	prefixLen := reviewAgentNameLimit - len(suffix)
	return strings.TrimRight(base[:prefixLen], "-") + suffix
}

// poolReviewer is the resolved launch identity for one exact-SHA review.
type poolReviewer struct {
	Kind     string
	Provider string
	Model    string
	Family   string
	Argv     []string
	Reason   string
}

// resolvePoolReviewer routes the reviewer through the same quota-aware router
// every other launch uses.
//
// FAC-574: the pool used to hardcode OpenCode with an Ollama default model, so
// a native Claude reviewer was unreachable through the exact-SHA pool lifecycle
// no matter what the operator asked for — and when that proxy was rate limited
// the whole exact-review path was dead with no alternative route. Routing here
// means a spent surface reroutes instead of retrying into the same wall.
func resolvePoolReviewer(provider, model, excludeFamily string) (poolReviewer, error) {
	// Defense in depth: the command line was validated at parse time, but this
	// function is also reachable directly, and one definition of the rule keeps
	// the two from disagreeing.
	if err := (&poolReviewOptions{Model: &model, Provider: &provider}).Validate(); err != nil {
		return poolReviewer{}, err
	}
	engine := usage.NewQuotaEngine()
	computed := map[string]usage.BurnState{}
	if snap, err := usage.FetchSnapshot(); err == nil {
		computed = engine.ComputeAll(snap)
	} else {
		// Fail loud but keep routing on availability: a quota outage must not
		// silently collapse back onto a single hardcoded surface.
		fmt.Fprintf(os.Stderr, "review --pool: WARN live quota unavailable (%v); routing on availability only\n", err)
	}
	route, err := router.NewRouter(engine, computed).Pick("qa", strings.TrimSpace(provider), strings.TrimSpace(excludeFamily))
	if err != nil {
		return poolReviewer{}, fmt.Errorf("route reviewer: %w", err)
	}
	pickedModel := route.Model
	if override := strings.TrimSpace(model); override != "" {
		pickedModel = override
	}
	kind, argv, err := router.HarnessArgvFor(route.Provider, pickedModel, route.Effort)
	if err != nil {
		return poolReviewer{}, fmt.Errorf("reviewer launch argv for %s/%s: %w", route.Provider, pickedModel, err)
	}
	if len(argv) == 0 {
		return poolReviewer{}, fmt.Errorf("reviewer launch argv for %s/%s resolved empty", route.Provider, pickedModel)
	}
	return poolReviewer{
		Kind: kind, Provider: route.Provider, Model: pickedModel,
		// Recompute the family from the pair that actually launches: an
		// operator --model override can change family, and a stale family
		// makes a legitimate override look like a launch/verdict conflict.
		Family: router.FamilyFor(route.Provider, pickedModel),
		Argv:   argv, Reason: route.Reason,
	}, nil
}

// preflightReviewerReadiness proves the resolved provider can actually execute a
// request BEFORE any lease or tab exists.
//
// FAC-577: quota being healthy and the host CLI reporting an interactive login
// are NOT worker credential readiness. A routed claude reviewer launched into a
// pane that was sitting at an authentication screen, which surfaced only after
// the lease and tab had been created and had to be compensated by hand.
//
// The probe runs a real minimal generation and requires the exact probe token,
// so it exercises the same boundary the launch will hit rather than a cheaper
// proxy for it. A login screen, a missing credential handle, or an unconfigured
// model all fail here, before anything needs cleaning up.
func preflightReviewerReadiness(r poolReviewer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res := herdr.ProbeProviderModel(ctx, r.Provider, r.Model, "")
	if res.Available {
		return nil
	}
	return fmt.Errorf(
		"reviewer %s/%s is not ready to execute a request: %s\n"+
			"  Quota and an interactive host login are not worker credential readiness.\n"+
			"  Provision an approved credential handle for this surface, or route another\n"+
			"  surface with --provider. No lease or tab was created.",
		r.Provider, r.Model, res.Reason)
}
