package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/stash"
)

// runStash ports bin/herd-stash: worktree-scoped stash on a private ref
// namespace, so one lane's pop can never grab another lane's WIP.
func runStash() {
	// Fail closed when not in a git repository (matches zsh entrypoint).
	if err := exec.Command("git", "rev-parse", "--git-dir").Run(); err != nil {
		fmt.Fprintln(os.Stderr, "herd-stash: not in a git repository")
		os.Exit(1)
	}

	repo := stash.Repo{Dir: "."}
	ctx := context.Background()
	args := os.Args[2:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "", "-h", "--help":
		fmt.Println("Usage: herd stash push [-m <msg>] [-- <paths>...] | pop | apply | list")
		fmt.Println("  Worktree-scoped stash on refs/herd-stash/<worktree>/<n>; never the shared stack.")
		fmt.Println("  Tracked changes only; untracked files are left in place (git stash default).")
		fmt.Println("  -u/--include-untracked is not implemented and is refused.")
		return
	case "push", "pop", "apply", "list":
	default:
		fmt.Fprintf(os.Stderr, "herd-stash: unknown command '%s' (push|pop|apply|list)\n", cmd)
		os.Exit(1)
	}

	// Refuse while the SHARED stack still holds entries on this branch: those
	// are exactly the racy entries herd-stash exists to replace.
	branch := repo.BranchContext(ctx)
	if hits, _ := stash.RefuseSharedStack(ctx, ".", branch); len(hits) > 0 {
		fmt.Fprint(os.Stderr, stash.FormatSharedStackRefusal(branch, hits))
		os.Exit(1)
	}

	switch cmd {
	case "push":
		msg, paths, err := stash.ParsePushArgs(args[1:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd-stash: %v\n", err)
			os.Exit(1)
		}
		ref, err := repo.PushOpts(ctx, stash.PushOptions{
			Message:     msg,
			ScopedPaths: paths,
			Stderr:      os.Stderr,
		})
		if err != nil {
			if errors.Is(err, stash.ErrNoChanges) {
				if len(paths) > 0 {
					fmt.Printf("herd-stash: no local (tracked) changes to save in: %s\n", strings.Join(paths, " "))
				} else {
					fmt.Println("herd-stash: no local (tracked) changes to save")
				}
				return
			}
			fmt.Fprintf(os.Stderr, "herd-stash: %v\n", err)
			os.Exit(1)
		}
		// Success notice already printed by PushOpts to stderr (matches zsh).
		_ = ref

	case "pop":
		ref, err := repo.Pop(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd-stash: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("herd-stash: popped %s\n", ref)

	case "apply":
		ref, err := repo.ApplyKeep(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd-stash: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("herd-stash: applied %s (kept; drop with: git update-ref -d %s)\n", ref, ref)

	case "list":
		id, _ := repo.WorktreeIDContext(ctx)
		entries, err := repo.List(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd-stash: %v\n", err)
			os.Exit(1)
		}
		if len(entries) == 0 {
			fmt.Printf("herd-stash: no entries for worktree %q\n", id)
			return
		}
		fmt.Printf("herd-stash entries for '%s' (newest first):\n", id)
		for _, e := range entries {
			fmt.Printf("  %s  %s\n", e.Short, e.Summary)
		}
	}
}
