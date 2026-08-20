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

	"github.com/Kampe/Herdforge/pkg/herdr"
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
	shaFlag := fs.String("sha", parsedSHA, "Exact candidate commit (default: HEAD of .herd/worktrees/<ref>)")
	model := fs.String("model", "litellm/ollama/glm-5.2:cloud", "Persistent OpenCode model")
	poolRoot := fs.String("pool-root", filepath.Join(".herd", "pool"), "Warm review pool root")
	surfaceRoot := fs.String("surface-root", filepath.Join(".herd", "review-surfaces"), "Review surface symlink root")
	packetRoot := fs.String("packet-root", filepath.Join(".herd", "review-packets"), "Review packet root")
	noLaunch := fs.Bool("no-launch", false, "Prepare and print the surface without starting Herdr")
	// The command line was validated and parsed above. Parse the operational
	// options again to retain their defaults and values for this invocation;
	// --pool is deliberately registered so the selector is consumed here too.
	_ = fs.Bool("pool", false, "Select the warm-pool review path")
	if err := fs.Parse(leadingPositionalArgs(os.Args[2:])); err != nil {
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
	herdrBin, err := exec.LookPath("herdr")
	if err != nil {
		return fmt.Errorf("herdr CLI lookup: %w", err)
	}
	agentName := reviewAgentName(ref, sha)
	start := exec.Command(herdrBin, "agent", "start", agentName, "--kind", "opencode", "--pane", tab.Pane.ID, "--", "--model", *model, "--auto")
	if out, err := start.CombinedOutput(); err != nil {
		return fmt.Errorf("start OpenCode reviewer: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if _, err := herdr.AgentPrompt(agentName, "Read and execute the review packet at "+packet+" in full.", false); err != nil {
		return fmt.Errorf("deliver review packet: %w", err)
	}
	releaseOnFailure = false
	fmt.Printf("reviewer launched ref=%s sha=%s lease=%s surface=%s tab=%s agent=%s packet=%s\n", ref, shortSHA(sha), lease.LeaseID, surface, tabLabel, agentName, packet)
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
	_ = fs.Bool("pool", false, "Select the warm-pool review path")
	shaFlag := fs.String("sha", "", "Exact candidate commit")
	_ = fs.String("model", "", "Persistent reviewer model")
	_ = fs.String("pool-root", "", "Warm review pool root")
	_ = fs.String("surface-root", "", "Review surface symlink root")
	_ = fs.String("packet-root", "", "Review packet root")
	_ = fs.Bool("no-launch", false, "Prepare and print the surface without starting Herdr")
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

const reviewAgentNameLimit = 32

func reviewAgentName(ref, sha string) string {
	base := "review-" + safeReviewSurfacePart(ref) + "-" + shortSHA(sha)
	if len(base) <= reviewAgentNameLimit {
		return base
	}
	// Keep the name recognizable while retaining a stable ref-derived suffix so
	// long refs with the same visible prefix cannot collide after truncation.
	suffix := fmt.Sprintf("-%x", sha256.Sum256([]byte(strings.TrimSpace(ref))))[:9]
	prefixLen := reviewAgentNameLimit - len(suffix)
	return strings.TrimRight(base[:prefixLen], "-") + suffix
}

func reviewTabLabel(ref, sha string) string {
	// Herdr currently accepts the same 1-32 character surface used by agent
	// names, but keep this policy separate so a future tab-label limit can
	// change without changing agent identity semantics.
	base := "review-" + safeReviewSurfacePart(ref) + "-" + shortSHA(sha)
	if len(base) <= reviewAgentNameLimit {
		return base
	}
	suffix := fmt.Sprintf("-%x", sha256.Sum256([]byte(strings.TrimSpace(ref)+"\x00"+strings.TrimSpace(sha))))[:9]
	prefixLen := reviewAgentNameLimit - len(suffix)
	return strings.TrimRight(base[:prefixLen], "-") + suffix
}
