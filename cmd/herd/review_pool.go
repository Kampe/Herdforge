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
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/security"
	"github.com/Kampe/Herdforge/pkg/standing"
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

	// FAC-608: prove the candidate's builder family BEFORE spending anything.
	// Admission refuses a verdict whose builder-family is not provable, and it
	// refused 25 of 41 artifacts in one inbox for exactly that -- each one after a
	// full review lane, wall clock and quota had already been consumed. The same
	// question is free to ask here. Declining now costs a command; declining at
	// ingest costs the entire review.
	//
	// --allow-unproven-builder exists because refusing outright would stop the
	// review pipeline dead on a repo whose history predates launch records, which
	// is a worse failure than a review that has to be admitted by hand.
	provenFamily, familyErr := provenBuilderFamily(root, sha)
	if familyErr != nil {
		return fmt.Errorf("resolve builder family for %s: %w", shortSHA(sha), familyErr)
	}
	// FAC-612: --allow-unproven-builder alone was a DEAD END. It skipped this
	// check, so the review ran, and then ingest refused the verdict anyway for the
	// same missing provenance -- a full lane spent to produce something
	// unadmittable. Worse, the six mergeable PRs on the board could never merge:
	// the merge policy wants a verdict, a verdict wants provable provenance, and
	// nothing could supply it. The gate turned a wasted review into a deadlock.
	//
	// Asserting a family now RECORDS it, which is the difference between an
	// operator taking attributable responsibility for provenance and a reviewer
	// silently guessing it. The launch row is what admission reads, so the
	// resulting verdict is admissible by construction.
	if asserted := strings.TrimSpace(*opts.BuilderFamily); asserted != "" {
		if !reviewledger.FamilyAllowlist[asserted] {
			return fmt.Errorf("--builder-family %q is not an allowed vendor family", asserted)
		}
		if provenFamily != "" && !strings.EqualFold(provenFamily, asserted) {
			return fmt.Errorf(
				"--builder-family %q contradicts the recorded launch family %q for %s; the ledger is authoritative",
				asserted, provenFamily, shortSHA(sha))
		}
		if provenFamily == "" {
			if err := recordAssertedBuilderLaunch(root, ref, sha, asserted); err != nil {
				return fmt.Errorf("record asserted builder family: %w", err)
			}
			fmt.Printf("recorded asserted builder-family=%s for %s (operator-attributed provenance)\n", asserted, shortSHA(sha))
		}
		provenFamily = asserted
	}
	if provenFamily == "" && !*opts.AllowUnprovenBuilder {
		return fmt.Errorf(
			"candidate %s has no provable builder family: no launch record for this exact sha carries an allowlisted builder_family, "+
				"so review-ingest will refuse any verdict produced for it no matter how good the review is. "+
				"Record the builder launch, or pass --allow-unproven-builder to review it anyway and admit the verdict by hand",
			shortSHA(sha))
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
	packetBody := reviewPacketBody(ref, sha, surface, verdictPath, reviewSupervisorTarget(), provenFamily)
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
	// with cwd=$HOME while their surface symlinks were perfectly correct.
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
	// FAC-601: wait for the harness to actually accept input before delivering
	// the packet. StartReviewAgent proves the agent is LISTED and its pane
	// exists; it never proved the TUI was ready. Sending immediately raced
	// through on this warm host and failed every time on the WSL review node,
	// where claude needs about six seconds to become interactive: the packet
	// landed in a TUI that was not accepting input, was dropped, and the launch
	// reported queued-but-not-consumed with the pane still idle.
	//
	// A readiness timeout is NOT fatal here. The delivery below has its own
	// verification with a real consumption check, so the worst case of
	// proceeding is the same error we already report — whereas refusing on a
	// slow-but-healthy harness would throw away a working reviewer.
	if _, readyErr := herdr.AwaitInteractiveReady(agentName, 30*time.Second); readyErr != nil {
		fmt.Fprintf(os.Stderr, "review --pool: %s not interactive yet (%v); delivering anyway\n", agentName, readyErr)
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
	Pool                 *bool
	SHA                  *string
	Provider             *string
	Model                *string
	ExcludeFamily        *string
	PoolRoot             *string
	SurfaceRoot          *string
	PacketRoot           *string
	NoLaunch             *bool
	AllowUnprovenBuilder *bool
	BuilderFamily        *string
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
		BuilderFamily: fs.String("builder-family", "",
			"Assert the candidate's builder family and RECORD it, so the resulting verdict is admissible."),
		AllowUnprovenBuilder: fs.Bool("allow-unproven-builder", false,
			"Dispatch even when no launch record proves the candidate's builder family. The verdict will need hand admission."),
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
// builderFamilyOrUnproven keeps the packet honest when provenance is absent. A
// blank value would render "builder-family:" with nothing after it, which a
// reviewer would fill in by guessing -- the exact behaviour that produced 25
// refusals.
func builderFamilyOrUnproven(family string) string {
	if strings.TrimSpace(family) == "" {
		return "unproven"
	}
	return family
}

// recordAssertedBuilderLaunch writes the launch row admission reads, so an
// operator-asserted family produces an ADMISSIBLE verdict rather than a review
// that is refused after it has already been paid for.
func recordAssertedBuilderLaunch(root, ref, sha, family string) error {
	l, err := reviewledger.NewReviewLedger(root, reviewledger.DefaultPath(root))
	if err != nil {
		return err
	}
	return l.EnsureRecord(reviewledger.RecordOpts{
		SHA:           sha,
		BuilderFamily: family,
		Reviewer:      reviewAgentName(ref, sha),
		Task:          ref,
		Gate:          "operator-asserted",
	})
}

// provenBuilderFamily resolves the candidate's recorded builder family, or "" if
// no launch record proves one. An unreadable ledger is an error, never a quiet
// "unprovable" -- that would decline every candidate on an IO fault.
func provenBuilderFamily(root, sha string) (string, error) {
	l, err := reviewledger.NewReadOnlyReviewLedger(root, reviewledger.DefaultPath(root))
	if err != nil {
		return "", err
	}
	return l.ProvenBuilderFamily(sha)
}

// reviewSupervisorTarget names the lane a finished reviewer reports home to.
//
// FAC-603: the packet used to name no owner at all, so a reviewer that finished
// had no way to announce it. Completion was discoverable only by the supervisor
// polling every pane, which meant a verdict sat unowned until the next beat --
// and when the supervisor believed the host was saturated, it stopped looking.
// A reviewer that can report home turns completion into an event instead of
// something waiting to be noticed.
//
// The override exists because the supervisor's live agent name carries a
// per-launch suffix, so the canonical standing name is only the fallback.
func reviewSupervisorTarget() string {
	if v := strings.TrimSpace(os.Getenv("HERD_REVIEW_SUPERVISOR")); v != "" {
		return v
	}
	canonical := standing.AgentName("review-supervisor")
	// FAC-616: the canonical lane name is NOT the live agent name. Live agents
	// carry a per-launch suffix -- the running supervisor is
	// forge-review-harvest-su-467b70d7, not forge-review-supervisor -- so
	// FAC-603's "report home" instruction addressed a mailbox nobody drains.
	// Mail sent there sat unread for four days. A fix that turns completion into
	// an event is worthless if the event is delivered to a dead letterbox.
	//
	// Resolve the live lane by prefix and fall back to the canonical name only
	// when nothing is running, so the packet still names something meaningful in
	// a cold fleet.
	if live := liveAgentByPrefix("forge-review-harvest", "forge-review-supervisor", canonical); live != "" {
		return live
	}
	return canonical
}

// liveAgentByPrefix returns the first live agent whose name starts with any of
// the given prefixes, preferring earlier prefixes. An unreachable herdr yields
// "" so the caller falls back rather than guessing.
func liveAgentByPrefix(prefixes ...string) string {
	agents, err := herdr.AgentList()
	if err != nil {
		return ""
	}
	for _, prefix := range prefixes {
		for _, a := range agents {
			name := strings.TrimSpace(a.Name)
			if name != "" && strings.HasPrefix(name, prefix) {
				return name
			}
		}
	}
	return ""
}

func reviewPacketBody(ref, sha, surface, verdictPath, supervisor, builderFamily string) string {
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
reviewer-family: <your VENDOR family — see the exact list below>
builder-family: %s
verdict: PASS|FAIL|BLOCKED
reviewed-head: <output of git rev-parse HEAD in the tree you actually read>
---

The builder-family above is PREFILLED from this candidate's launch record. Do not
change it and do not re-derive it: it is the recorded provenance, and a verdict
that disagrees with the ledger is refused as a launch/verdict identity conflict.
If it reads "unproven", this review was dispatched with
--allow-unproven-builder and its verdict will need hand admission -- say so in
your evidence.

FAMILY VALUES ARE A CLOSED SET. Use exactly one of:

  anthropic  openai  google  xai  zhipu  moonshot  alibaba  deepseek
  open-weight  antigravity  proxy

These are VENDOR families, not harness names. Your harness is not a family: a
reviewer running under codex writes openai, claude writes anthropic, grok writes
xai, agy writes google. A verdict recorded as reviewer-family "codex" is refused
as an unknown family and the whole review is discarded, which has already
happened in this inbox.

Your evidence goes below the --- line. Keep the task: header accurate: it is
what ties this verdict to a board card, and a verdict that names no card cannot
be joined back to one.

THEN REPORT HOME. This is not optional and it is the last thing you do:

  herd mail send --from %s --to %s --file %s

Writing the verdict is not finishing. Until the supervisor knows this review is
done, the verdict is not ingested, the card does not move, nothing merges, and
your pool slot stays out of circulation. A verdict nobody is told about is
indistinguishable from a review that never ran -- 85 finished reviews were found
sitting unowned in this inbox for exactly that reason.

Report home even when the verdict is FAIL or BLOCKED. A negative verdict is a
result the supervisor needs in order to release the slot and re-plan; silence is
the only outcome that helps nobody.

If you are reviewing on a host other than the one holding the ledger, ALSO push
your verdict artifact to the verdicts/<this-host-workspace> branch. The mail
above does not cross hosts; the branch is the only transport that does, and a
verdict that stays on this filesystem is invisible to the ledger.
`, ref, sha, surface, verdictPath, sha, ref, builderFamilyOrUnproven(builderFamily), reviewAgentName(ref, sha), supervisor, verdictPath)
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
