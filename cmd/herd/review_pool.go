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
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/security"
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
	// FAC-591: teach the pool which lease holders are still alive so it can
	// reclaim the rest itself. Every launch that died after leasing used to
	// leave an ownerless lease no command could free, and the pool wedged at
	// "no available clean slots" while reporting itself saturated with zero live
	// reviewers. Self-healing here means no operator or coordinator has to
	// notice and unstick it.
	p.HolderLive = reviewHolderLive
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
	// FAC-588: filepath.Rel refuses to mix an absolute base with a relative
	// target, and the two sides come from different places — the lease path is
	// always absolute (built from the repo root) while --surface-root defaults to
	// the relative ".herd/review-surfaces". So every `herd review --pool` that did
	// not pass an absolute --surface-root died here with
	//
	//	Rel: can't make /abs/.herd/pool/pool-02 relative to .herd/review-surfaces
	//
	// after leasing a slot but before launching anything. To a caller that lost
	// stderr it looked like the command silently did nothing, which is what made
	// the reviewer pool appear permanently unfillable. Absolutise both sides so
	// the default flag values work.
	surfaceDirAbs, err := filepath.Abs(filepath.Dir(surface))
	if err != nil {
		return fmt.Errorf("resolve review surface dir: %w", err)
	}
	leasePathAbs, err := filepath.Abs(lease.Path)
	if err != nil {
		return fmt.Errorf("resolve pool lease path: %w", err)
	}
	relTarget, err := filepath.Rel(surfaceDirAbs, leasePathAbs)
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
	// FAC-597: name the exact destination. Both .herd/review/inbox and
	// .herd/review/outbox exist, MoveToIngestedNamed is location-agnostic (it
	// creates ingested/ beside whatever it is handed), and the packet never said
	// where to write. So reviewers inferred the location by pattern-matching
	// nearby files: a pool-01 reviewer for CHA-2255 wrote to outbox because
	// other files were already there, and review-ingest never saw it.
	//
	// A reviewer that infers its output location can silently write to the one
	// the coordinator is not watching, which loses a completed review with no
	// error anywhere. inbox is canonical per reviewingest.InboxRel and
	// IsInboxPath ("durable review inbox artifact"); the packet now states an
	// absolute path so there is nothing left to infer.
	inboxAbs, err := filepath.Abs(filepath.Join(root, reviewingest.InboxRel))
	if err != nil {
		return fmt.Errorf("resolve review inbox: %w", err)
	}
	if err := os.MkdirAll(inboxAbs, 0o700); err != nil {
		return fmt.Errorf("create review inbox: %w", err)
	}
	verdictPath := filepath.Join(inboxAbs, fmt.Sprintf("%s-%s.md", shortSHA(sha), reviewAgentName(ref, sha)))
	packetBody := reviewPacketBody(ref, sha, surface, verdictPath)
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
	// FAC-592: the tab cwd MUST be absolute. --surface-root defaults to the
	// relative ".herd/review-surfaces", and herdr cannot resolve a relative cwd
	// against this process's directory, so it silently started the reviewer in
	// $HOME. A reviewer sitting in $HOME reads a tree that has nothing to do
	// with the candidate and still writes a verdict — the worst possible
	// outcome, because the artifact looks legitimate. Observed live: two lanes
	// with cwd=/Users/kampe while their surface symlinks were perfectly correct.
	//
	// Verified absolute rather than assumed: a cwd that cannot be resolved must
	// fail the launch, never fall back to somewhere plausible.
	surfaceAbs, err := filepath.Abs(surface)
	if err != nil {
		return fmt.Errorf("resolve review surface for reviewer cwd: %w", err)
	}
	if st, statErr := os.Stat(surfaceAbs); statErr != nil || !st.IsDir() {
		return fmt.Errorf("reviewer cwd %q does not resolve to a directory: %v", surfaceAbs, statErr)
	}
	tab, err := herdr.TabCreate(herdr.TabCreateOptions{Workspace: ws, Label: tabLabel, Cwd: surfaceAbs, NoFocus: true, Env: []string{herdr.AgentRoleEnv}})
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
	// FAC-576: pass the FLAGS, not the whole argv. herdr's `agent start ... --
	// <args>` appends these after the harness command it resolves from --kind,
	// so including argv[0] ran `claude claude --model ...` and the extra
	// positional made the pane land somewhere the launch check reads as an auth
	// screen. The pre-FAC-574 code passed flags only; carrying argv[0] over was
	// my regression.
	if err := herdr.StartReviewAgent(tab.ID, agentName, tab.Pane.ID, reviewer.Kind, reviewer.LaunchFlags()...); err != nil {
		return fmt.Errorf("start %s reviewer (%s): %w", reviewer.Kind, reviewer.Model, err)
	}
	// FAC-592: the delivered path must be ABSOLUTE. --packet-root defaults to the
	// relative ".herd/review-packets", and the reviewer resolves it against its
	// own cwd — which is the pool slot, not the repo root, so the packet is not
	// there. A reviewer whose cwd fell back to $HOME looked for
	// ~/.herd/review-packets/... and correctly refused to execute a different
	// file instead. An absolute path removes the ambiguity entirely rather than
	// depending on where the reviewer happens to stand.
	packetAbs, err := filepath.Abs(packet)
	if err != nil {
		return errors.Join(fmt.Errorf("resolve review packet path: %w", err), herdr.CloseReviewTab(tab.ID, agentName))
	}
	if _, statErr := os.Stat(packetAbs); statErr != nil {
		return errors.Join(fmt.Errorf("review packet %q is not readable: %w", packetAbs, statErr), herdr.CloseReviewTab(tab.ID, agentName))
	}
	if _, err := herdr.Send(agentName, "Read and execute the review packet at "+packetAbs+" in full.", true, 30*time.Second); err != nil {
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

// LaunchFlags returns the argv WITHOUT the harness command itself.
//
// herdr resolves the command from --kind and appends these after `--`, so
// argv[0] must not be repeated: doing so passes the harness name to itself as a
// positional argument.
func (r poolReviewer) LaunchFlags() []string {
	if len(r.Argv) == 0 {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(r.Argv[0]), strings.TrimSpace(r.Kind)) {
		return r.Argv[1:]
	}
	// A harness whose argv[0] is not the kind (a wrapper, for example) is passed
	// through whole rather than silently truncated.
	return r.Argv
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

// preflightReviewerReadiness proves the resolved reviewer can actually be
// launched BEFORE any lease or tab exists.
//
// FAC-579: the previous version ran a real generation as a CHILD OF THIS
// PROCESS and reported Available, then the pane launch failed with "pane is at
// a login or authentication screen". Both statements were true: a probe in the
// coordinator's own process proves the COORDINATOR's credential context, and
// the reviewer runs in a pane with a different one. A preflight that cannot
// observe the boundary it claims to check is worse than none, because it
// converts an honest failure into a false pass.
//
// The gate is now the same authority the launch path itself consults —
// security.DiagnoseKindAuthReadiness — which reports whether worker credentials
// for this harness kind can actually be brokered. It is non-spawning and costs
// nothing, and it is exactly what `herd hostcreds diagnose --kind <k>` reports,
// so the preflight and the diagnostic can no longer disagree.
//
// The in-process generation probe is kept as a SECONDARY signal only, and only
// to catch a quota or model failure early. It can never turn a brokerability
// refusal into a pass.
func preflightReviewerReadiness(r poolReviewer) error {
	auth := security.DiagnoseKindAuthReadiness(r.Kind)
	if !auth.Brokerable {
		return fmt.Errorf(
			"reviewer %s/%s cannot be launched: %s\n"+
				"  %s\n"+
				"  An interactive host login is not worker credential readiness: the pane runs\n"+
				"  in a different credential context than this process, so `auth status` saying\n"+
				"  logged-in does not make a spawned reviewer able to authenticate.\n"+
				"  Same check as: herd hostcreds diagnose --kind %s\n"+
				"  No lease or tab was created.",
			r.Provider, r.Model, auth.Blocker, auth.RecommendedAction, r.Kind)
	}
	// Secondary: a surface that is brokerable can still be out of quota or
	// pointed at a model that does not exist. This probe runs in this process,
	// so it proves nothing about the pane's credentials and is never allowed to
	// stand in for the brokerability gate above.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if res := herdr.ProbeProviderModel(ctx, r.Provider, r.Model, ""); !res.Available {
		return fmt.Errorf(
			"reviewer %s/%s failed a request in this process: %s\n"+
				"  Worker credentials are brokerable, so this is a quota or model problem\n"+
				"  rather than an authentication one. No lease or tab was created.",
			r.Provider, r.Model, res.Reason)
	}
	return nil
}

// reviewPacketBody is the reviewer's instruction sheet.
//
// FAC-583: it used to say "report the required verdict artifact" without ever
// stating the artifact's shape. review-ingest REFUSES anything whose front
// matter is not the leading block ("front matter must be the leading block
// ending in ---"), so a reviewer that opened with prose had its verdict thrown
// away after doing all the work. A real Opus review of PR-3115 — which caught a
// fabricated eth_call response causing a 1e12 money error — was refused for
// exactly this reason and never reached the ledger.
//
// The contract is therefore inlined WITH the known values prefilled, so the
// reviewer's output is ingestible by construction rather than by luck. sha and
// task are filled here because we know them; the reviewer supplies only what
// only it can know.
func reviewPacketBody(ref, sha, surface, verdictPath string) string {
	return fmt.Sprintf(`REVIEW %s — verdict only, edit nothing.

Candidate: %s
Surface: %s

Read docs/prompts/review-contract.md and inspect only this candidate.

WRITE YOUR VERDICT ARTIFACT TO EXACTLY THIS PATH:

  %s

Do not infer the location from other files you find nearby. Two review
directories exist and only this one is watched by the ingest step; a verdict
written elsewhere is silently never read, and the review is lost with no error
reported anywhere.

The artifact MUST begin with this front-matter block, as the very first bytes of
the file. Any prose above it makes review-ingest refuse the whole verdict and
your review is discarded. Do not wrap it in a code fence.

sha: %s
branch: <the branch this candidate lives on>
task: %s
reviewer: <your lane name — never a coordinator>
reviewer-family: <your model family: anthropic|openai|google|xai|...>
builder-family: <the AUTHOR's family — must differ from yours>
verdict: PASS|FAIL|BLOCKED
reviewed-head: <output of git rev-parse HEAD in the tree you actually read>
---

Your evidence goes below the --- line. Keep the task: header accurate: it is
what ties this verdict to a board card, and a verdict that names no card cannot
be joined back to one.
`, ref, sha, surface, verdictPath, sha, ref)
}

// settledAgentStatuses are the states in which a reviewer is no longer doing
// work and its pool lease must not keep a slot out of circulation.
//
// "done" and "idle" both count. A reviewer that finished is done; a reviewer
// that was launched and never consumed its packet sits idle forever. Treating
// idle as live is what let a launch failure hold a slot indefinitely.
var settledAgentStatuses = map[string]bool{
	"done": true, "idle": true, "": true,
}

// reviewHolderLive reports whether a pool lease's holder is still working.
//
// It answers false for an ABSENT agent as well as a settled one: the common
// failure was a lease whose agent never existed because the launch died between
// leasing and starting, and an agent that cannot be found cannot be working.
//
// A herdr failure returns true — refusing to reclaim. Inability to ask is not
// evidence that a reviewer is dead, and reclaiming a live reviewer's slot would
// reset the tree out from under it mid-review.
func reviewHolderLive(purpose string) bool {
	name := strings.TrimSpace(purpose)
	if name == "" {
		return false
	}
	agents, err := herdr.AgentList()
	if err != nil {
		return true
	}
	for _, a := range agents {
		if strings.TrimSpace(a.Name) != name {
			continue
		}
		return !settledAgentStatuses[strings.ToLower(strings.TrimSpace(a.Status))]
	}
	return false
}
