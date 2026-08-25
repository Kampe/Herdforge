package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// runVerdictPush transports one verdict artifact to the ledger host over git.
//
// FAC-619: this replaces a THREE-STEP git recipe in the review packet that could
// not work, for two reasons found only by running it:
//
//  1. .gitignore line 92 ignores /.herd/*, so `git add <verdict>` silently
//     no-ops. The reviewer sees "nothing to commit, working tree clean" and
//     reasonably concludes it already reported.
//  2. verdicts/<ws> is a long-lived branch that has diverged from the reviewer's
//     HEAD, so pushing HEAD to it is rejected as a non-fast-forward.
//
// Three previous report-home mechanisms shipped broken because each was checked
// by reading the rendered packet instead of executing it. A multi-step git recipe
// handed to a reviewer is a fourth chance to get it wrong, so this is one
// command that Herdforge owns and can be tested.
//
// It uses plumbing only -- hash-object, mktree, commit-tree -- so it never
// checks out, stashes, or switches a branch in the reviewer's worktree. A review
// host must not have its tree mutated to send a message.
func runVerdictPush() error {
	fs := flag.NewFlagSet("verdict-push", flag.ContinueOnError)
	artifact := fs.String("artifact", "", "Path to the verdict artifact to transport")
	workspace := fs.String("workspace", "", "Workspace id (default: resolved from config/env)")
	remote := fs.String("remote", "origin", "Git remote to push to")
	dryRun := fs.Bool("dry-run", false, "Build the commit and report the ref without pushing")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	path := strings.TrimSpace(*artifact)
	if path == "" {
		return fmt.Errorf("--artifact is required (usage: herd verdict-push --artifact <path>)")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("verdict artifact %q is not a readable file", path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("verdict artifact %q is empty; refusing to transport a blank verdict", path)
	}

	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	ws := strings.TrimSpace(*workspace)
	if ws == "" {
		if resolved, wsErr := herdr.RequireWorkspace(root); wsErr == nil {
			ws = strings.TrimSpace(resolved)
		} else {
			ws = strings.TrimSpace(os.Getenv("HERD_WORKSPACE"))
		}
	}
	if ws == "" {
		return fmt.Errorf("cannot resolve a workspace id; pass --workspace so the verdicts ref is not empty")
	}

	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// -w writes the blob regardless of .gitignore: plumbing does not consult it.
	// This is precisely the step the packet recipe could not express.
	blob, err := git("hash-object", "-w", path)
	if err != nil {
		return fmt.Errorf("hash verdict artifact: %s", blob)
	}
	leaf := filepath.Base(path)
	tree, err := gitMkTree(root, blob, leaf)
	if err != nil {
		return err
	}

	// Each verdict gets its OWN ref, so two reviewers finishing at once cannot
	// reject each other with a non-fast-forward. The harvester already fetches
	// refs/heads/verdicts/* as a glob.
	ref := fmt.Sprintf("refs/heads/verdicts/%s-%s", ws, strings.TrimSuffix(leaf, ".md"))
	if len(ref) > 220 {
		ref = ref[:220]
	}
	commit, err := git("commit-tree", tree, "-m", "verdict: "+leaf)
	if err != nil {
		return fmt.Errorf("create verdict commit: %s", commit)
	}
	if *dryRun {
		fmt.Printf("verdict-push DRY RUN commit=%s ref=%s artifact=%s\n", shortSHA(commit), ref, leaf)
		return nil
	}
	if out, pushErr := git("push", "--force", *remote, commit+":"+ref); pushErr != nil {
		return fmt.Errorf("push verdict ref %s: %s", ref, out)
	}
	// A push that exits 0 is not proof the ref landed. Read it back.
	out, err := git("ls-remote", "--heads", *remote, strings.TrimPrefix(ref, "refs/heads/"))
	if err != nil || strings.TrimSpace(out) == "" {
		return fmt.Errorf("push reported success but %s is not present on %s", ref, *remote)
	}
	fmt.Printf("verdict-push OK ref=%s commit=%s artifact=%s\n", ref, shortSHA(commit), leaf)
	return nil
}

// gitMkTree builds a single-entry tree placing the artifact under the canonical
// inbox path, so the harvester finds it exactly where it expects.
func gitMkTree(root, blob, leaf string) (string, error) {
	entry := fmt.Sprintf("100644 blob %s\t%s\n", blob, leaf)
	inner := exec.Command("git", "-C", root, "mktree")
	inner.Stdin = strings.NewReader(entry)
	innerOut, err := inner.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("mktree verdict leaf: %s", strings.TrimSpace(string(innerOut)))
	}
	innerTree := strings.TrimSpace(string(innerOut))

	// Nest it as .herd/review/inbox/<leaf>.
	tree := innerTree
	for _, dir := range []string{"inbox", "review", ".herd"} {
		e := fmt.Sprintf("040000 tree %s\t%s\n", tree, dir)
		c := exec.Command("git", "-C", root, "mktree")
		c.Stdin = strings.NewReader(e)
		o, mkErr := c.CombinedOutput()
		if mkErr != nil {
			return "", fmt.Errorf("mktree %s: %s", dir, strings.TrimSpace(string(o)))
		}
		tree = strings.TrimSpace(string(o))
	}
	return tree, nil
}
