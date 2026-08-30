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
	"github.com/Kampe/Herdforge/pkg/launch"
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
	// FAC-584: capacity BEFORE candidate resolution. resolvePoolReviewCandidateAt
	// can prepare a detached worktree when none holds the SHA; that is already
	// repository mutation. The W4 incident prepared a worktree and then died
	// because herdr was down -- this gate makes that order impossible.
	releaseCapacity, err := acquirePoolCapacityOrRefuse()
	if err != nil {
		return err
	}
	if releaseCapacity != nil {
		defer releaseCapacity()
	}
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	// FAC-648: the exact SHA participates in candidate resolution, because a
	// detached exact-SHA surface is a legitimate candidate and used to be refused.
	candidateDir, err := resolvePoolReviewCandidateAt(root, ref, strings.TrimSpace(*shaFlag))
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
	// FAC-627: an unprovable candidate is now REVIEWABLE, with the verdict
	// admitted under the provenance-unrecorded gate. Nothing writes a launch
	// receipt when a standing lane commits, so refusing here blocked every
	// candidate on the board and left the review host idle with ~20 reviewable
	// PRs. The review is worth having; the independence claim is what must be
	// withheld, and MergeReadiness withholds it.
	if provenFamily == "" {
		fmt.Printf("provenance UNRECORDED for %s: dispatching anyway; the verdict will be admitted "+
			"under gate=%s and cannot support a cross-family independence claim\n",
			shortSHA(sha), reviewledger.GateProvenanceUnrecorded)
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
		// FAC-677: the route resolution fetches live quota across every provider
		// and can take tens of seconds -- measured between 29s and 272s on this
		// fleet, tracking provider API latency. It said nothing while it ran.
		routeStart := time.Now()
		fmt.Println("resolving reviewer route (live quota across providers; can take 30s+)")
		resolved, err := resolvePoolReviewer(*provider, *model, *excludeFamily)
		if err != nil {
			return err
		}
		fmt.Printf("route resolved in %s: provider=%s model=%s pool=%s cache_age=%s\n",
			time.Since(routeStart).Round(time.Second), resolved.Provider, resolved.Model, resolved.Pool, resolved.QuotaAge.Round(time.Second))
		if err := preflightReviewerReadiness(resolved); err != nil {
			return err
		}
		reviewer = resolved
	}

	// FAC-682: refuse to launch INTO a shared checkout that is already dirty.
	//
	// A reviewer proving a test non-vacuous swaps a file for its parent-commit
	// blob and restores it afterwards. Done in the wrong directory that rewrites
	// canonical shared main, which is what happened on 2026-08-26: the shared
	// index and working tree had one path replaced with the candidate's parent
	// blob, HEAD unchanged, no MERGE_HEAD.
	//
	// Checking here does two things. It stops a new reviewer inheriting a
	// corrupted shared tree, and it surfaces the damage at the next launch rather
	// than whenever someone happens to look -- which is how this went unnoticed
	// long enough to need a hand repair.
	if dirty := sharedCheckoutDirtyPaths(root); len(dirty) > 0 {
		fmt.Fprintf(os.Stderr,
			"review --pool: REFUSING to launch: the canonical shared checkout at %s has uncommitted changes:\n  %s\n"+
				"  A review surface is isolated; the shared checkout should never be modified by one.\n"+
				"  This is the signature of a non-vacuity swap run in the wrong directory: a file replaced\n"+
				"  with its parent-commit blob and never restored.\n"+
				"  Inspect and restore before launching (`git -C %s status`, then `git -C %s checkout -- <path>`\n"+
				"  once you have confirmed the content is not someone's real work), or set\n"+
				"  HERD_ALLOW_DIRTY_SHARED_CHECKOUT=1 if these changes are intentional.\n",
			root, strings.Join(dirty, "\n  "), root, root)
		return fmt.Errorf("canonical shared checkout is dirty; refusing to launch a reviewer into an unclean repository")
	}

	// FAC-653: refuse a DUPLICATE launch before touching the pool.
	//
	// The agent name is derived deterministically from ref+sha, so relaunching
	// the same candidate collides with the reviewer already working on it. That
	// collision was only detected at `herdr agent start` -- the LAST step, after
	// this command had already leased a slot, EVICTED whatever was living in it,
	// and closed that pane. So a duplicate launch destroyed a healthy reviewer in
	// a DIFFERENT slot and then failed anyway, and every retry destroyed one more.
	//
	// Observed live: a relaunch of origin/repair/cha-2797-p2 evicted an occupant
	// from pool-05 and closed tab w4:t1H, then failed with agent_name_taken
	// because review-origin-repair-ch-66fa9d85 was already Working in pool-04.
	// Repeated attempts left four idle review agents and zero working reviewers
	// on a host that had been fully occupied -- read from outside as a pool,
	// reaper, or admission fault, when it was a retry loop eating its own fleet.
	//
	// Checking first makes a duplicate launch IDEMPOTENT and non-destructive: the
	// candidate is already being reviewed, which is success, not a conflict. An
	// unreadable agent list is NOT proof of absence, so it falls through to the
	// old behaviour rather than refusing a legitimate launch.
	if live, name, ok := liveReviewerFor(ref, sha); ok && live {
		fmt.Printf("review --pool: candidate %s is ALREADY being reviewed by %s; not launching a second reviewer "+
			"(the deterministic agent name is derived from ref+sha, so this would collide, and reaching the collision "+
			"would first evict whatever is living in the newly leased slot)\n", shortSHA(sha), name)
		return nil
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
	// FAC-632: evict any process still living in this slot BEFORE resetting it.
	//
	// The reset below rewrites the worktree to a different candidate's SHA. A
	// reviewer already running in that directory does not notice: its subsequent
	// reads return the NEW candidate's code while it still believes it is
	// reviewing the old one, so it emits a plausible verdict for a commit it never
	// examined -- and that verdict is admitted as genuine.
	//
	// Measured on the live review host: FIVE of eight slots each held two live
	// claude reviewers, the stale one 2-4x older than the lease holder. pool-01's
	// lease was review-cha-pr3270-c8a62372005d at HEAD c8a623720 while the older
	// occupant was working on CHA-2307/2757/2765/2774/2779 and sha 6adb55fa9078.
	//
	// A leftover pane is a capacity problem. A leftover PROCESS in a reset
	// worktree is a correctness problem, and it is the one that has to be closed
	// here rather than swept up afterwards.
	if evicted, err := evictPoolSlotOccupants(lease.Path, agentNameFor(ref, sha)); err != nil {
		// Fail closed: resetting a slot we could not clear risks exactly the
		// wrong-SHA verdict this guard exists to prevent.
		return fmt.Errorf("evict stale occupants of %s before pinning: %w", lease.Name, err)
	} else if evicted > 0 {
		fmt.Printf("evicted %d stale occupant(s) from %s before pinning %s\n",
			evicted, lease.Name, shortSHA(sha))
	}
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
	// FAC-618: resolve the workspace BEFORE rendering the packet. The branch
	// transport line used to interpolate $(herd config workspace) -- a subcommand
	// that does not exist -- so it expanded to an empty string and the reviewer
	// was told to push refs/heads/verdicts/, an invalid ref. The push fails and
	// the verdict never leaves the review host.
	//
	// A resolution failure here is not fatal: the packet still renders, with the
	// workspace left for the reviewer to supply, which is strictly better than a
	// command that silently produces a broken ref.
	packetWorkspace, wsErr := herdr.RequireWorkspace(root)
	if wsErr != nil {
		packetWorkspace = strings.TrimSpace(os.Getenv("HERD_WORKSPACE"))
	}
	packetBody := reviewPacketBody(ref, sha, surface, verdictPath, reviewSupervisorTarget(), provenFamily, packetWorkspace)
	if err := os.WriteFile(packet, []byte(packetBody), 0o600); err != nil {
		return fmt.Errorf("write review packet: %w", err)
	}

	// FAC-656: complete the launch record now that a lease EXISTS.
	//
	// The first record row is written before the pool is leased, so it cannot
	// carry a lease -- which is why every one of 2210 live ledger rows had an
	// empty lease and harvest admission could never admit anything. Appending a
	// completed row here is what makes Admit satisfiable at all.
	//
	// Best-effort: the reviewer is already launching and the ledger write is not
	// the authority for that. A failure here costs admissibility later, which is
	// recoverable and visible, whereas failing the launch would waste a leased
	// slot and a provider call for a bookkeeping problem.
	if err := completeReviewLaunchProvenance(root, ref, sha, lease.LeaseID); err != nil {
		fmt.Fprintf(os.Stderr, "review --pool: reviewer launched but launch provenance is INCOMPLETE (%v); "+
			"this candidate will be refused at harvest admission until a record row carries its lease and patch id\n", err)
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
	// FAC-677: agent start creates a pane and waits for the harness to become
	// interactive, which measured ~20s here. Silent, it is the third stretch a
	// bounded caller cannot tell apart from a hang.
	startedAt := time.Now()
	fmt.Printf("starting %s agent %s in pane %s\n", reviewer.Kind, agentName, tab.Pane.ID)
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
	fmt.Printf("agent started in %s\n", time.Since(startedAt).Round(time.Second))
	fmt.Printf("reviewer launched ref=%s sha=%s lease=%s surface=%s tab=%s agent=%s packet=%s harness=%s provider=%s model=%s pool=%s family=%s\n", ref, shortSHA(sha), lease.LeaseID, surface, tabLabel, agentName, packet, reviewer.Kind, reviewer.Provider, reviewer.Model, reviewer.Pool, reviewer.Family)
	return nil
}

// resolvePoolReviewCandidate first preserves the dispatched ticket convention,
// then resolves a real checked-out branch from Git's worktree metadata. Branch
// names are data passed to Git, never path fragments appended to the repo root.
// resolvePoolReviewCandidate keeps the branch-only behaviour for callers that
// have no exact SHA to verify against.
func resolvePoolReviewCandidate(root, ref string) (string, error) {
	return resolvePoolReviewCandidateAt(root, ref, "")
}

// resolvePoolReviewCandidateAt resolves the candidate surface, accepting a
// DETACHED worktree when its HEAD is the exact SHA under review.
//
// FAC-648: two defects met here and blocked every branch-style remote review.
//
//  1. Pool.Ensure creates slots with `git worktree add --detach`, and the remote
//     launcher prepares its surface the same way, but resolution accepted only a
//     porcelain `branch refs/heads/<ref>` line. A detached surface has no such
//     line, so `herd review <branch> --pool --sha <SHA>` always failed with
//     "candidate branch is not checked out in a worktree" -- for a surface that
//     was already sitting at exactly the right commit.
//
//  2. The directory probe was skipped entirely for any ref containing '/', and
//     even when tried it built the path with worktreePathForRef (lowercase only,
//     slashes KEPT) while the launcher names the directory with
//     safeReviewSurfacePart (every non-[a-z0-9-] to '-'). So the surface was
//     created at .herd/worktrees/feat-cha-2794-... and looked for at
//     .herd/worktrees/feat/cha-2794-... -- two sanitizers for one path, the same
//     mismatch class as the launcher's own verification grep.
//
// Requiring a named branch was never the safety property. The pool resets the
// slot --hard to the exact SHA regardless, so what matters is that the surface
// IS that SHA -- which is verified here rather than assumed from a branch name.
// With no SHA to check, the old branch-only behaviour is unchanged.
func resolvePoolReviewCandidateAt(root, ref, sha string) (string, error) {
	// Probe both spellings: the raw-ref path for historical ticket-style refs and
	// the launcher's sanitized path, so one sanitizer cannot hide the other's dir.
	for _, dir := range candidateSurfaceDirs(root, ref) {
		if !worktreeExists(dir) {
			continue
		}
		if sha == "" {
			return dir, nil
		}
		if headMatchesSHA(dir, sha) {
			return dir, nil
		}
	}

	branchPath, err := checkedOutBranchWorktree(root, ref)
	if err != nil {
		return "", err
	}
	if branchPath != "" {
		return branchPath, nil
	}
	if sha != "" {
		if dir := detachedSurfaceAtSHA(root, sha); dir != "" {
			return dir, nil
		}
		// FAC-678: prepare the surface rather than demanding the caller did.
		//
		// Reported as "the wrapper demands a checked-out branch despite exact
		// remote head availability". Reproduced: the branch existed locally, the
		// SHA resolved, and resolution still refused because no worktree happened
		// to hold it -- so every caller had to remember a manual
		// `git worktree add --detach` first, and two dispatches were rejected for
		// forgetting a step the command can do itself.
		//
		// This creates ONLY a detached surface at the exact SHA, under the
		// managed worktrees path, and only when the SHA is a real commit. It
		// never checks out a branch, so it cannot move anyone's work, and it
		// never reuses an existing directory.
		if dir, err := prepareCandidateSurface(root, ref, sha); err == nil && dir != "" {
			fmt.Printf("prepared review surface %s at %s\n", dir, shortSHA(sha))
			return dir, nil
		} else if err != nil {
			return "", fmt.Errorf("no worktree holds candidate %q at exact sha %s and one could not be prepared: %w",
				ref, shortSHA(sha), err)
		}
		// FAC-653: a SHA too short to verify is a bad ARGUMENT, not a missing
		// worktree. headMatchesSHA and detachedSurfaceAtSHA both require >=12
		// hex so an abbreviation cannot ambiguously match the wrong commit --
		// correct -- but the refusal then read "no worktree holds candidate at
		// exact sha", which sent an operator hunting for a surface that was
		// sitting right there with exactly the right HEAD. Say which it is.
		if len(strings.TrimSpace(sha)) < 12 {
			return "", fmt.Errorf("candidate sha %q is too short to verify (need at least 12 hex characters); "+
				"an abbreviation could match more than one commit, so it is refused rather than guessed. "+
				"Pass the full 40-character sha", sha)
		}
		return "", fmt.Errorf("no worktree holds candidate %q at exact sha %s: neither a checked-out branch nor a detached surface with that HEAD (looked in %v)",
			ref, shortSHA(sha), candidateSurfaceDirs(root, ref))
	}
	if filepath.Base(ref) != ref || strings.ContainsAny(ref, `/\\`) {
		return "", fmt.Errorf("candidate branch %q is not checked out in a worktree", ref)
	}
	return "", fmt.Errorf("candidate worktree %q does not exist", filepath.Join(root, worktreePathForRef(ref)))
}

// prepareCandidateSurface creates a detached worktree at the exact candidate.
//
// Detached on purpose: checking out the BRANCH would move a ref someone else may
// be working on, while a detached surface at an exact SHA is inert. Returns ""
// with no error when the SHA is not a commit here, so the caller falls through
// to its normal refusal rather than reporting a preparation failure for a
// candidate that never existed.
func prepareCandidateSurface(root, ref, sha string) (string, error) {
	if len(strings.TrimSpace(sha)) < 12 {
		return "", nil
	}
	if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "-q", sha+"^{commit}").Run(); err != nil {
		return "", nil // not a commit here; not ours to prepare
	}
	dir := filepath.Join(root, ".herd", "worktrees", safeReviewSurfacePart(ref)+"-"+shortSHA(sha))
	if worktreeExists(dir) {
		// Someone prepared it between the lookup and now. Use it only if it is
		// actually at the candidate, never merely because the path exists.
		if headMatchesSHA(dir, sha) {
			return dir, nil
		}
		return "", fmt.Errorf("surface %s exists but is not at %s", dir, shortSHA(sha))
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "--detach", dir, sha).CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return dir, nil
}

// candidateSurfaceDirs returns every directory spelling a candidate surface may
// legitimately have: the historical raw-ref path and the launcher's sanitized
// path. Deduplicated so a slash-free ref yields one entry.
func candidateSurfaceDirs(root, ref string) []string {
	raw := filepath.Join(root, worktreePathForRef(ref))
	safe := filepath.Join(root, ".herd", "worktrees", safeReviewSurfacePart(ref))
	if raw == safe {
		return []string{raw}
	}
	return []string{raw, safe}
}

// headMatchesSHA reports whether dir's HEAD is exactly sha. A resolution error
// is NOT a match: an unreadable surface is unknown, never confirmation.
func headMatchesSHA(dir, sha string) bool {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return false
	}
	head := strings.TrimSpace(string(out))
	if len(head) < 12 || len(sha) < 12 {
		return false
	}
	return strings.EqualFold(head, sha) || strings.HasPrefix(strings.ToLower(head), strings.ToLower(sha))
}

// detachedSurfaceAtSHA finds any registered worktree whose HEAD is the exact
// SHA, so a surface prepared under an unexpected directory name still resolves.
func detachedSurfaceAtSHA(root, sha string) string {
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return ""
	}
	absRoot, _ := filepath.Abs(root)
	var path, head string
	// Only a DETACHED, non-root worktree qualifies. My first version matched on
	// HEAD alone and happily returned the main worktree, because the repo root is
	// usually sitting at the SHA under review -- which would have run the review
	// against the shared checkout instead of an isolated surface. Caught by the
	// wrong-SHA test, which passed for the wrong reason until the root was excluded.
	consider := func(detached bool) string {
		if !detached || path == "" || head == "" {
			return ""
		}
		if abs, err := filepath.Abs(path); err == nil && abs == absRoot {
			return ""
		}
		if !worktreeExists(path) || len(head) < 12 || len(sha) < 12 {
			return ""
		}
		if strings.HasPrefix(strings.ToLower(head), strings.ToLower(sha)) {
			return path
		}
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
			head = ""
		case strings.HasPrefix(line, "HEAD "):
			head = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
		case line == "detached":
			if match := consider(true); match != "" {
				return match
			}
		}
	}
	return ""
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
		// Retained as an accepted no-op: FAC-627 made unprovable candidates
		// reviewable by default, admitting the verdict under
		// gate=provenance-unrecorded. Removing the flag would break callers that
		// still pass it, and silently ignoring an unknown flag is worse.
		AllowUnprovenBuilder: fs.Bool("allow-unproven-builder", false,
			"Deprecated no-op: unprovable candidates are now reviewable and their verdicts are admitted as provenance-unrecorded."),
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
	Pool     string
	Family   string
	QuotaAge time.Duration
	Argv     []string
	Reason   string
}

const (
	reviewerRouteTaskShape         = "qa"
	reviewerRouteDecisionAuthority = "strict-review-launch-v1"
)

// reviewerRouteResolution is the one route authority consumed by both
// `herd route qa` and review-pool launch. Keeping the quota read, strictness,
// probes, exact tuple, family exclusion, workspace-scoped concurrency and
// decision in this function prevents a diagnostic Pick from advertising a
// tuple that launch-time Decide immediately refuses.
type reviewerRouteResolution struct {
	Decision  *router.LaunchDecision
	QuotaAge  time.Duration
	Workspace string
}

func resolveReviewerRouteAuthority(provider, model, excludeFamily string) (reviewerRouteResolution, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	excludeFamily = strings.TrimSpace(excludeFamily)
	if model != "" && provider == "" {
		return reviewerRouteResolution{}, errors.New("--model requires --provider (a model without its surface is not a route)")
	}

	engine := usage.NewQuotaEngine()
	snap, age, err := usage.FetchSnapshotCached()
	if err != nil {
		return reviewerRouteResolution{}, fmt.Errorf(
			"reviewer route rejected task_shape=%s provider=%s model=%s pool=%s cache_age=unknown gate=quota-read reason=UNKNOWN no-quota-data: %w",
			reviewerRouteTaskShape, provider, model, router.QuotaPoolFor(provider, model), err)
	}

	surfaceRouter := router.NewRouter(engine, engine.ComputeAll(snap))
	// FAC-657 remains intact: health is evaluated for the exact model before
	// the decision, and probe-gated models receive evidence for that exact key.
	probeResults := map[string]bool{}
	launchable := surfaceRouter.Probes.Launchable
	surfaceRouter.Probes.Launchable = func(candidateProvider, candidateModel string) (bool, string) {
		ok, reason := launchable(candidateProvider, candidateModel)
		probeResults[router.ProbeKey(candidateProvider, candidateModel)] = ok
		return ok, reason
	}
	// The route command and launch path now see the same live roster, scoped by
	// the explicit HERD_WORKSPACE when present. Without an explicit workspace,
	// do not invent a cross-workspace concurrency authority.
	if strings.TrimSpace(os.Getenv("HERD_WORKSPACE")) != "" {
		surfaceRouter.Probes.LiveCount = liveRouteCount
	}

	decision, err := surfaceRouter.Decide(router.LaunchRequest{
		Role:              router.RoleReviewSupervisor,
		Shape:             reviewerRouteTaskShape,
		RequestedProvider: provider,
		RequestedModel:    model,
		ExcludedFamily:    excludeFamily,
		ProbeResults:      probeResults,
		StrictQuota:       true,
	})
	if err != nil {
		return reviewerRouteResolution{}, fmt.Errorf(
			"reviewer route rejected task_shape=%s provider=%s model=%s pool=%s cache_age=%s gate=route-decision reason=%w",
			reviewerRouteTaskShape, provider, model, router.QuotaPoolFor(provider, model), age.Round(time.Second), err)
	}
	return reviewerRouteResolution{
		Decision: decision, QuotaAge: age,
		Workspace: strings.TrimSpace(os.Getenv("HERD_WORKSPACE")),
	}, nil
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
	resolved, err := resolveReviewerRouteAuthority(provider, model, excludeFamily)
	if err != nil {
		return poolReviewer{}, err
	}
	if resolved.QuotaAge > 0 {
		fmt.Printf("using quota reading from %s ago\n", resolved.QuotaAge.Round(time.Second))
	}
	decision := resolved.Decision
	return poolReviewer{
		Kind: decision.Harness, Provider: decision.Provider, Model: decision.Model,
		Pool: decision.Pool, Family: decision.Family, QuotaAge: resolved.QuotaAge,
		Argv: decision.HarnessArgv, Reason: decision.Rationale,
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
	// FAC-677: say what this is doing before it does it.
	//
	// This probe sends a real request and may take up to 60 seconds, and it
	// produced NO output while it ran. A silent minute is indistinguishable from
	// a hang, so an operator bounding the command at a reasonable timeout kills
	// it right at the boundary and reports a hang -- which is exactly what
	// happened: "printed only 'provenance UNRECORDED' then hung indefinitely
	// with no lease/agent/pane". Measured here: --no-launch returns in 1.4s and
	// the same launch completes in 60-90s, all of it in this probe.
	//
	// The command was working. It just could not say so. Nothing is slower for
	// announcing itself, and a bounded caller now knows whether to wait.
	fmt.Printf("probing %s/%s can serve a request (up to 60s; this is the slow step)\n", r.Provider, r.Model)
	probeStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res := herdr.ProbeProviderModel(ctx, r.Provider, r.Model, "")
	fmt.Printf("probe finished in %s available=%v\n", time.Since(probeStart).Round(time.Second), res.Available)
	if !res.Available {
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
// reportHomeInstruction renders the transport that actually works from THIS
// host.
//
// FAC-617: mail is host-local. herd mail send appends to .herd/control-mail.jsonl
// in the local checkout, so a reviewer on the second host can compose a
// perfectly-formed message that the ledger host will never see. When no
// supervisor is reachable locally, the git branch is the ONLY transport that
// crosses hosts, so it becomes the primary instruction rather than an
// afterthought appended below a mail command that cannot work.
func reportHomeInstruction(agent, supervisor, verdictPath, workspace string) string {
	if strings.TrimSpace(supervisor) == "" {
		ws := strings.TrimSpace(workspace)
		// FAC-622: name the binary by ABSOLUTE PATH. `herd` is not on a reviewer's
		// PATH -- verified on the review host, where `command -v herd` finds
		// nothing in a pool worktree under a non-login shell, and only
		// ~/Projects/Herdforge/bin/herd resolves. A blocked reviewer reported it
		// as "herd isn't installed here" while holding a finished PASS verdict it
		// could not transport.
		//
		// Every previous version of this instruction was tested from an operator
		// shell that had the binary resolved already, which is why six attempts
		// looked correct and none ran.
		cmd := "  " + herdBinaryPathForPacket() + " verdict-push --artifact " + verdictPath
		if ws != "" {
			cmd += " --workspace " + ws
		}
		// FAC-620: this replaces a three-step git recipe that could not work.
		// `git add` silently no-ops because .gitignore covers /.herd/*, and
		// pushing HEAD to the shared verdicts branch is a non-fast-forward.
		// verdict-push uses plumbing, writes past the ignore, takes its own ref,
		// and reads the ref back before reporting success.
		return cmd + "\n\n" +
			"No review supervisor is reachable from this host, so MAIL WILL NOT WORK: herd mail send\n" +
			"writes a file in this checkout and nothing carries it to the ledger host. Git is the only\n" +
			"transport that crosses hosts, and the command above IS your report home.\n\n" +
			"Do NOT hand-roll this with git add/commit/push. That was tried and it fails silently:\n" +
			".gitignore covers /.herd/*, so git add stages nothing and reports a clean tree."
	}
	return "  herd mail send --from " + agent + " --to " + supervisor + " --file " + verdictPath
}

// herdBinaryPathForPacket resolves the absolute path of the running binary, so
// the packet names a command the reviewer can actually execute. Falls back to
// the bare name only when the executable cannot be located, which is strictly
// better than emitting nothing.
func herdBinaryPathForPacket() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, rErr := filepath.EvalSymlinks(exe); rErr == nil {
			return resolved
		}
		return exe
	}
	return "herd"
}

// builderFamilyOrUnrecorded keeps the packet honest when provenance is absent. A
// blank value would render "builder-family:" with nothing after it, which a
// reviewer would fill in by guessing -- the exact behaviour that produced 25
// refusals.
//
// FAC-630: this emitted the bare literal "unproven" while the ledger's canonical
// sentinel is reviewledger.FamilyUnrecorded ("unrecorded"). Two spellings for one
// concept, which is the duplicate-rule defect FAC-613 removed from this repo, and
// it meant every reviewer following the packet wrote a value that only survived
// because ingest happened to accept both. It now emits the constant, so the
// packet and the ledger cannot drift.
func builderFamilyOrUnrecorded(family string) string {
	if strings.TrimSpace(family) == "" {
		return reviewledger.FamilyUnrecorded
	}
	return family
}

// sharedCheckoutDirtyPaths reports uncommitted paths in the canonical checkout.
//
// Returns nothing when the operator has declared the state intentional, because
// a gate that cannot be satisfied gets bypassed wholesale rather than
// understood. Returns nothing on an unreadable status too: an unanswerable
// question must not block a launch.
func sharedCheckoutDirtyPaths(root string) []string {
	if strings.TrimSpace(os.Getenv("HERD_ALLOW_DIRTY_SHARED_CHECKOUT")) == "1" {
		return nil
	}
	out, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return nil
	}
	var dirty []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			dirty = append(dirty, line)
		}
		if len(dirty) >= 10 {
			dirty = append(dirty, "... (truncated)")
			break
		}
	}
	return dirty
}

// completeReviewLaunchProvenance records the lease and an independent patch
// identity for the exact candidate, so Ledger.Admit has the bindings it requires.
//
// Patch identity comes from git, never from the reviewer: it is what allows a
// REBASED candidate to keep its verdict rather than be re-reviewed, and it must
// therefore be computed from the tree rather than asserted by anyone.
func completeReviewLaunchProvenance(root, ref, sha, leaseID string) error {
	patch, err := candidatePatchIdentity(root, sha)
	if err != nil {
		return fmt.Errorf("patch identity for %s: %w", shortSHA(sha), err)
	}
	l, err := reviewledger.NewReviewLedger(root, reviewledger.DefaultPath(root))
	if err != nil {
		return err
	}
	reviewer := reviewAgentName(ref, sha)
	// FAC-667: ensure the record row EXISTS before completing it.
	//
	// FAC-656 completed a launch record, and completion requires a prior row --
	// but only the operator-asserted --builder-family path writes one. On the
	// ordinary path, where provenance is honestly unrecorded, no record row was
	// ever written, so the completion failed with "provenance cannot be
	// completed for a launch that was never recorded" and the lease and patch id
	// still went unrecorded. That is the MAJORITY of launches: the exact case
	// the binding work existed to fix.
	//
	// I verified FAC-656 on the asserted path and generalised without exercising
	// this one. The seed carries the honest `unrecorded` family, so it asserts
	// nothing about who built the candidate; it only gives the lease and patch
	// bindings a row to live on.
	if err := l.EnsureRecord(reviewledger.RecordOpts{
		SHA:           sha,
		Reviewer:      reviewer,
		Task:          ref,
		BuilderFamily: reviewledger.FamilyUnrecorded,
		Gate:          reviewledger.GateProvenanceUnrecorded,
	}); err != nil {
		return fmt.Errorf("seed launch record: %w", err)
	}
	return l.CompleteLaunchProvenance(reviewledger.RecordOpts{
		SHA:      sha,
		Reviewer: reviewer,
		Task:     ref,
		Lease:    strings.TrimSpace(leaseID),
		PatchURL: patch,
		Gate:     "launch-provenance",
	})
}

// candidatePatchIdentity computes the stable patch id for a candidate against
// its merge base. Verified stable across a clean rebase, which is the property
// the admission binding depends on.
func candidatePatchIdentity(root, sha string) (string, error) {
	base, err := exec.Command("git", "-C", root, "merge-base", sha, "origin/main").Output()
	if err != nil {
		return "", fmt.Errorf("resolve merge base: %w", err)
	}
	diff := exec.Command("git", "-C", root, "diff", strings.TrimSpace(string(base)), sha)
	pipe := exec.Command("git", "-C", root, "patch-id", "--stable")
	out, err := diff.Output()
	if err != nil {
		return "", fmt.Errorf("read candidate diff: %w", err)
	}
	pipe.Stdin = strings.NewReader(string(out))
	id, err := pipe.Output()
	if err != nil {
		return "", fmt.Errorf("compute patch id: %w", err)
	}
	fields := strings.Fields(string(id))
	if len(fields) == 0 || strings.TrimSpace(fields[0]) == "" {
		return "", fmt.Errorf("git produced no patch id (empty diff?)")
	}
	return fields[0], nil
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
	family, err := l.ProvenBuilderFamily(sha)
	if err != nil || family != "" {
		return family, err
	}
	// FAC-637: fall back to the LAUNCH RECEIPTS. A receipt records "lane X ran
	// provider grok on branch Y"; if the candidate commit is reachable from
	// branch Y, the launch that produced it is known and the family is provable.
	//
	// This is not the domain inference a reviewer correctly refused ("apps/api is
	// nominally api-crusader's territory"). It is the recorded launch for the
	// branch the commit actually sits on, joined by git reachability rather than
	// by guessing which lane owns a directory.
	return builderFamilyFromReceipts(root, sha), nil
}

// builderFamilyFromReceipts resolves authorship by joining a commit to the
// branch of a recorded launch. Returns "" when no receipt's branch contains it.
func builderFamilyFromReceipts(root, sha string) string {
	// FAC-646: anchor on the caller's root. This function already takes `root`
	// and used a cwd-relative receipt path anyway, so a caller outside the
	// project read the wrong receipt log and resolved the wrong builder family
	// -- or none, which FAC-644 then reports as unresolvable provenance.
	receipts, err := launch.ReadReceipts(launch.ReceiptPathFor(root))
	if err != nil {
		return ""
	}
	for i := len(receipts) - 1; i >= 0; i-- {
		r := receipts[i]
		branch := strings.TrimSpace(r.Branch)
		if branch == "" || strings.TrimSpace(r.Provider) == "" {
			continue
		}
		family := router.FamilyFor(r.Provider, r.Model)
		if family == "" || !reviewledger.FamilyAllowlist[family] {
			continue
		}
		// Reachability, not equality: the commit need not be the branch tip, it
		// only has to have been produced on that branch.
		if commitIsAncestor(root, sha, branch) {
			return family
		}
	}
	return ""
}

// agentNameFor is the name this dispatch will give its reviewer, so eviction can
// spare a pane that is already the legitimate holder (an idempotent re-dispatch).
func agentNameFor(ref, sha string) string { return reviewAgentName(ref, sha) }

// liveReviewerFor reports whether a reviewer for this exact candidate is already
// running. The third return is false when the agent list could not be read at
// all, which must never be mistaken for "no reviewer exists" (FAC-653).
func liveReviewerFor(ref, sha string) (live bool, name string, known bool) {
	want := agentNameFor(ref, sha)
	agents, err := herdr.AgentList()
	if err != nil {
		return false, "", false
	}
	for _, a := range agents {
		if strings.TrimSpace(a.Name) != want {
			continue
		}
		// Any registered agent under this exact name blocks the start, whatever
		// its status: herdr refuses the name itself, not just working agents.
		return true, want, true
	}
	return false, want, true
}

// evictPoolSlotOccupants closes every pane whose foreground process is living
// inside slotPath, except one already named keepAgent.
//
// FAC-632: liveness and ownership are judged by the pane's foreground process
// CWD, not by the agent registry. That registry loses registrations while panes
// keep running, which is exactly how five stale reviewers went unnoticed.
func evictPoolSlotOccupants(slotPath, keepAgent string) (int, error) {
	abs, err := filepath.Abs(slotPath)
	if err != nil {
		return 0, err
	}
	panes, err := herdr.PaneList()
	if err != nil {
		// No pane inventory means we cannot prove the slot is clear.
		return 0, fmt.Errorf("pane inventory unavailable: %w", err)
	}
	evicted := 0
	for _, pane := range panes {
		procs, procErr := herdr.PaneProcessInfo(pane.PaneID)
		if procErr != nil || len(procs) == 0 {
			// Unknowable: never evict on a failed probe. An unreadable pane is not
			// evidence of a stale occupant.
			continue
		}
		resolved, absErr := filepath.Abs(strings.TrimSpace(procs[0].Cwd))
		if absErr != nil || resolved != abs && !strings.HasPrefix(resolved, abs+string(filepath.Separator)) {
			continue
		}
		// Spare an idempotent re-dispatch of the same candidate.
		if keepAgent != "" && (pane.Name == keepAgent || strings.Contains(pane.Name, keepAgent)) {
			continue
		}
		var closeErr error
		if strings.TrimSpace(pane.TabID) != "" {
			closeErr = herdr.TabClose(pane.TabID)
		}
		if closeErr != nil || strings.TrimSpace(pane.TabID) == "" {
			closeErr = herdr.PaneClose(pane.PaneID)
		}
		if closeErr != nil {
			return evicted, fmt.Errorf("close stale occupant %s: %w", pane.PaneID, closeErr)
		}
		evicted++
	}
	return evicted, nil
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
	//
	// FAC-617: when nothing matches, return "" rather than the canonical name.
	// The emptiness is MEANINGFUL -- reviewPacketBody uses it to render the git
	// verdicts branch as the primary report home. A canonical fallback would
	// print a mail command that cannot work from a non-ledger host, because
	// herd mail send writes .herd/control-mail.jsonl in the LOCAL checkout and
	// mail never crosses hosts. On the WSL review host the supervisor is not in
	// the local agent list at all, so this path is the normal case there, not an
	// edge case.
	_ = canonical
	return liveAgentByPrefix("forge-review-harvest", "forge-review-supervisor")
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

func reviewPacketBody(ref, sha, surface, verdictPath, supervisor, builderFamily, workspace string) string {
	return fmt.Sprintf(`REVIEW %s — verdict only, edit nothing.

ISOLATION — READ THIS BEFORE RUNNING ANY GIT COMMAND
Your cwd is an isolated review surface. Every command you run must stay inside it.
NEVER run git, or any command that writes, against the canonical shared checkout.

The Surface path below is a SYMLINK into your exclusive leased warm-pool slot
(.herd/pool/pool-NN). That alias is intentional: herd review --pool leases one
clean slot, pins the candidate there, and points the surface at that same tree.
Seeing the symlink resolve to your own pool cwd is NOT a broken isolation
contract and is NOT shared main. Isolation means exclusive lease + pool
worktree, not "surface path string differs from cwd".

git rev-parse --show-toplevel resolves THROUGH the symlink to the pool
worktree path. That is correct. Fail closed only if toplevel is the canonical
shared checkout (the repo root that is not under .herd/pool/). Comparing
toplevel to the literal Surface symlink string and calling a match
"non-isolated" is a false positive.

This is not hypothetical. Proving a test non-vacuous by swapping a file for its
parent-commit blob is GOOD practice and 132 verdicts in this corpus do it. Done in
the wrong directory it silently rewrites shared main: on 2026-08-26 the canonical
checkout's index and working tree had apps/api/src/routes.ts replaced with the
parent blob of the candidate under review, with HEAD unchanged and no MERGE_HEAD,
and a coordinator had to restore it by hand.

If you swap a file to prove non-vacuity:
  1. confirm where you are first: git rev-parse --show-toplevel
     it MUST resolve under .herd/pool/ (your leased slot). If it names the
     shared checkout root, STOP.
  2. prefer /tmp or an untracked scratch file; if you must swap a tracked file
     inside the pool, do the swap, run the test, then restore: git checkout -- <path>
  3. confirm clean before writing your verdict: git status --porcelain
Never pass -C, --git-dir or --work-tree pointing outside your surface/pool, and
never cd out of it to run a build or test. If something seems to require the
shared checkout, that is a finding to report, not a step to take.

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

%s

Writing the verdict is not finishing. Until the supervisor knows this review is
done, the verdict is not ingested, the card does not move, nothing merges, and
your pool slot stays out of circulation. A verdict nobody is told about is
indistinguishable from a review that never ran -- 85 finished reviews were found
sitting unowned in this inbox for exactly that reason.

Report home even when the verdict is FAIL or BLOCKED. A negative verdict is a
result the supervisor needs in order to release the slot and re-plan; silence is
the only outcome that helps nobody.

A verdict that stays on this filesystem is invisible to the ledger.
`, ref, sha, surface, verdictPath, sha, ref, builderFamilyOrUnrecorded(builderFamily), reportHomeInstruction(reviewAgentName(ref, sha), supervisor, verdictPath, workspace))
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
