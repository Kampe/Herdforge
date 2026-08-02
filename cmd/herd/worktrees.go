package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
)

// Row is a single worktree snapshot. Fields match the JSON schema from
// bin/herd-worktrees and the human column-t table.
type Row struct {
	Worktree string   `json:"worktree"`
	Branch   string   `json:"branch"`
	Head     string   `json:"head"`
	Locked   bool     `json:"locked"`
	Ahead    int      `json:"ahead"`
	Dirty    int      `json:"dirty"`
	Files    []string `json:"files"`
}

// collision maps a file path to the set of worktree branches that touch it.
type collision struct {
	File     string
	Branches []string
}

func runWorktrees() {
	fs := flag.NewFlagSet("worktrees", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Output JSON array")
	filesFlag := fs.Bool("files", false, "Also list per-worktree touched files")
	fs.Usage = func() {
		fmt.Println(`herd-worktrees , one-shot collision snapshot across every repo worktree:
  {worktree, branch, ahead-of-origin/main commits, dirty files, touched files}.
  Replaces the coordinator's manual per-worktree ` + "`git log` + `git status`" + `
  loop when ranking claimable tickets (planner request 2026-07-21). A final
  COLLISIONS section lists any file touched (committed-ahead or dirty) by
  more than one worktree, so collision-checking is exact, not eyeballed.

    herd worktrees            # human summary + collisions
    herd worktrees --json     # machine-readable array
    herd worktrees --files    # also list per-worktree touched files`)
	}

	// Handle --help before flag parse
	for _, a := range os.Args[2:] {
		if a == "--help" || a == "-h" {
			fs.Usage()
			os.Exit(0)
		}
	}

	if err := fs.Parse(os.Args[2:]); err != nil {
		// Print our own error message format matching bin/herd-worktrees
		if err == flag.ErrHelp {
			// Already handled above
		} else {
			fmt.Fprintf(os.Stderr, "herd-worktrees: %v\n", err)
		}
		os.Exit(2)
	}

	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "herd-worktrees: unknown arg %s\n", fs.Arg(0))
		os.Exit(2)
	}

	// Resolve repo root
	repoRoot := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		absRoot = repoRoot
	}

	// Verify origin/main exists
	ctx := context.Background()
	if err := runGit(ctx, absRoot, "rev-parse", "-q", "--verify", "origin/main"); err != nil {
		fmt.Fprintln(os.Stderr, "herd-worktrees: origin/main not found")
		os.Exit(1)
	}

	base := "origin/main"

	// Enumerate worktrees from principal repo
	out, err := gitOutput(ctx, absRoot, "worktree", "list", "--porcelain")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-worktrees: git worktree list: %v\n", err)
		os.Exit(1)
	}

	type wtEntry struct {
		Path   string
		Locked bool
	}

	var entries []wtEntry
	blocks := strings.Split(strings.TrimRight(string(out), "\n"), "\n\n")
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			continue
		}
		if !strings.HasPrefix(lines[0], "worktree ") {
			continue
		}
		path := strings.TrimPrefix(lines[0], "worktree ")
		locked := false
		for _, line := range lines[1:] {
			if line == "locked" {
				locked = true
				break
			}
		}
		entries = append(entries, wtEntry{Path: path, Locked: locked})
	}

	var rows []Row

	for _, e := range entries {
		// Skip vanished worktree directories
		if _, err := os.Stat(e.Path); os.IsNotExist(err) {
			continue
		}

		r := Row{
			Worktree: e.Path,
			Locked:   e.Locked,
		}

		r.Branch = runGitOrDefault(ctx, e.Path, "?", "rev-parse", "--abbrev-ref", "HEAD")
		r.Head = runGitOrDefault(ctx, e.Path, "?", "rev-parse", "--short", "HEAD")

		// Patch-equivalence count via git cherry
		r.Ahead = countCherryAhead(ctx, e.Path, base)

		// Committed (ahead) files
		var committedFiles []string
		if r.Ahead > 0 {
			cfOut, err := gitOutput(ctx, e.Path, "diff", "--name-only", base+"...HEAD")
			if err == nil {
				raw := strings.TrimSpace(string(cfOut))
				if raw != "" {
					committedFiles = strings.Split(raw, "\n")
				}
			}
		}

		// Dirty files
		dirtyOut, err := gitOutput(ctx, e.Path, "status", "--porcelain")
		if err == nil {
			raw := strings.TrimSpace(string(dirtyOut))
			if raw != "" {
				lines := strings.Split(raw, "\n")
				for _, line := range lines {
					// Strip the XY status column, keep the path
					if len(line) > 3 {
						pathPart := strings.TrimSpace(line[2:])
						if pathPart != "" {
							r.Files = append(r.Files, pathPart)
						}
					}
				}
				r.Dirty = len(lines)
			}
		}

		// Union committed + dirty files
		r.Files = append(r.Files, committedFiles...)
		if len(r.Files) > 0 {
			sort.Strings(r.Files)
			// Deduplicate
			seen := make(map[string]bool)
			uniq := make([]string, 0, len(r.Files))
			for _, f := range r.Files {
				if !seen[f] {
					seen[f] = true
					uniq = append(uniq, f)
				}
			}
			r.Files = uniq
		}

		rows = append(rows, r)
	}

	// Early exit for --json
	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rows)
		os.Exit(0)
	}

	// Human output
	printHuman(rows, *filesFlag)

	// Collisions
	cols := computeCollisions(rows)
	if len(cols) > 0 {
		fmt.Println("COLLISIONS:")
		for _, c := range cols {
			fmt.Printf("  %s  <-  %s\n", c.File, strings.Join(c.Branches, ", "))
		}
		os.Exit(3)
	}
	fmt.Println("COLLISIONS: none")
}

func printHuman(rows []Row, showFiles bool) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		line := fmt.Sprintf("%s\t%s\tahead=%d\tdirty=%d", r.Worktree, r.Branch, r.Ahead, r.Dirty)
		if r.Locked {
			line += "\tlocked"
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	if showFiles {
		for _, r := range rows {
			if len(r.Files) == 0 {
				continue
			}
			fmt.Printf("\n-- %s\n", r.Branch)
			for _, f := range r.Files {
				fmt.Printf("   %s\n", f)
			}
		}
	}
}

func computeCollisions(rows []Row) []collision {
	// Map file path → set of branches
	pathBranches := make(map[string]map[string]bool)
	for _, r := range rows {
		for _, f := range r.Files {
			if pathBranches[f] == nil {
				pathBranches[f] = make(map[string]bool)
			}
			pathBranches[f][r.Branch] = true
		}
	}

	var cols []collision
	for f, branches := range pathBranches {
		if len(branches) < 2 {
			continue
		}
		sorted := make([]string, 0, len(branches))
		for b := range branches {
			sorted = append(sorted, b)
		}
		sort.Strings(sorted)
		cols = append(cols, collision{File: f, Branches: sorted})
	}
	sort.Slice(cols, func(i, j int) bool {
		return cols[i].File < cols[j].File
	})
	return cols
}

// countCherryAhead returns the number of patch-equivalent commits on HEAD
// that are not in base, using git cherry (not rev-list). This is critical:
// in a rebase-merge repo, reachability misreports already-merged branches
// as ahead.
func countCherryAhead(ctx context.Context, worktree, base string) int {
	out, err := gitOutput(ctx, worktree, "cherry", base, "HEAD")
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "+") {
			count++
		}
	}
	return count
}

// runGit runs a git command and returns an error if it fails.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd.Run()
}

// gitOutput runs a git command and returns its stdout.
func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd.Output()
}

// runGitOrDefault runs a git command and returns the default value on failure.
func runGitOrDefault(ctx context.Context, dir, def string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return def
	}
	return strings.TrimSpace(string(out))
}
