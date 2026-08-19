package main

import (
	"context"
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
	if filepath.Base(ref) != ref || strings.ContainsAny(ref, `/\\`) {
		return fmt.Errorf("candidate ref %q must be a ticket identifier, not a path", ref)
	}
	fs := flag.NewFlagSet("review --pool", flag.ContinueOnError)
	shaFlag := fs.String("sha", "", "Exact candidate commit (default: HEAD of .herd/worktrees/<ref>)")
	model := fs.String("model", "litellm/ollama/glm-5.2:cloud", "Persistent OpenCode model")
	poolRoot := fs.String("pool-root", filepath.Join(".herd", "pool"), "Warm review pool root")
	surfaceRoot := fs.String("surface-root", filepath.Join(".herd", "review-surfaces"), "Review surface symlink root")
	packetRoot := fs.String("packet-root", filepath.Join(".herd", "review-packets"), "Review packet root")
	noLaunch := fs.Bool("no-launch", false, "Prepare and print the surface without starting Herdr")
	if err := fs.Parse(leadingPositionalArgs(os.Args[2:])); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		ref = fs.Arg(0)
	}
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	candidateDir := worktreePathForRef(ref)
	if !worktreeExists(candidateDir) {
		return fmt.Errorf("candidate worktree %q does not exist", candidateDir)
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
	lease, err := p.Lease(context.Background(), "review-"+ref+"-"+shortSHA(sha))
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
	tabLabel := "review-" + safeReviewSurfacePart(ref) + "-" + shortSHA(sha)
	tab, err := herdr.TabCreate(herdr.TabCreateOptions{Workspace: ws, Label: tabLabel, Cwd: surface, NoFocus: true, Env: []string{herdr.AgentRoleEnv}})
	if err != nil {
		return fmt.Errorf("create reviewer tab: %w", err)
	}
	herdrBin, err := exec.LookPath("herdr")
	if err != nil {
		return fmt.Errorf("herdr CLI lookup: %w", err)
	}
	start := exec.Command(herdrBin, "agent", "start", tabLabel, "--kind", "opencode", "--pane", tab.Pane.ID, "--", "--model", *model, "--auto")
	if out, err := start.CombinedOutput(); err != nil {
		return fmt.Errorf("start OpenCode reviewer: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if _, err := herdr.AgentPrompt(tabLabel, "Read and execute the review packet at "+packet+" in full.", false); err != nil {
		return fmt.Errorf("deliver review packet: %w", err)
	}
	releaseOnFailure = false
	fmt.Printf("reviewer launched ref=%s sha=%s lease=%s surface=%s tab=%s packet=%s\n", ref, shortSHA(sha), lease.LeaseID, surface, tabLabel, packet)
	return nil
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
	if part == "" {
		return "candidate"
	}
	return part
}
