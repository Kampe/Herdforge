package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/stash"
)

// runStash ports bin/herd-stash: worktree-scoped stash on a private ref
// namespace, so one lane's pop can never grab another lane's WIP.
func runStash() {
	repo := stash.Repo{Dir: "."}
	args := os.Args[2:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "", "-h", "--help":
		fmt.Println("Usage: herd stash push [-m <msg>] | pop | apply | list")
		fmt.Println("  Worktree-scoped stash on refs/herd-stash/<worktree>/<n>; never the shared stack.")
		fmt.Println("  Tracked changes only; untracked files are left in place (git stash default).")
		return
	case "push", "pop", "apply", "list":
	default:
		fmt.Fprintf(os.Stderr, "herd stash: unknown command %q (push|pop|apply|list)\n", cmd)
		os.Exit(2)
	}

	// Refuse while the SHARED stack still holds entries on this branch: those
	// are exactly the racy entries herd-stash exists to replace, and ignoring
	// them would leave the cross-lane hazard in place.
	if hits := repo.SharedStackConflict(); len(hits) > 0 {
		fmt.Fprintf(os.Stderr, "herd stash: the shared 'git stash' stack holds entries on this branch:\n")
		for _, h := range hits {
			fmt.Fprintf(os.Stderr, "  %s\n", h)
		}
		fmt.Fprintln(os.Stderr, "herd stash: migrate them off the shared stack first (they race across worktrees):")
		fmt.Fprintln(os.Stderr, "  git stash apply <id> && herd stash push -m migrated   # then: git stash drop <id>")
		os.Exit(1)
	}

	switch cmd {
	case "push":
		msg := ""
		rest := args[1:]
		if len(rest) >= 2 && rest[0] == "-m" {
			msg = rest[1]
			rest = rest[2:]
		}
		// Refuse leftovers rather than ignore them: push REVERTS what it saves,
		// so a dropped argument sweeps files the caller never named.
		if len(rest) > 0 {
			fmt.Fprintf(os.Stderr, "herd stash: push takes -m <msg>; got %q.\n", strings.Join(rest, " "))
			fmt.Fprintln(os.Stderr, "  Refusing rather than ignoring it: push REVERTS what it saves.")
			fmt.Fprintln(os.Stderr, "  -u/--include-untracked is not implemented: untracked files are always left in place.")
			os.Exit(2)
		}
		ref, err := repo.Push(msg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd stash: %v\n", err)
			os.Exit(1)
		}
		if ref == "" {
			fmt.Println("herd stash: no local (tracked) changes to save")
			return
		}
		fmt.Printf("herd stash: saved %s; worktree reverted to HEAD (untracked left in place)\n", ref)

	case "pop", "apply":
		ref, err := repo.Apply(cmd == "pop")
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd stash: %v\n", err)
			os.Exit(1)
		}
		if cmd == "pop" {
			fmt.Printf("herd stash: popped %s\n", ref)
		} else {
			fmt.Printf("herd stash: applied %s (kept; drop with: git update-ref -d %s)\n", ref, ref)
		}

	case "list":
		refs, err := repo.Entries()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd stash: %v\n", err)
			os.Exit(1)
		}
		id, _ := repo.WorktreeID()
		if len(refs) == 0 {
			fmt.Printf("herd stash: no entries for worktree %q\n", id)
			return
		}
		fmt.Printf("herd stash entries for %q (newest first):\n", id)
		for i := len(refs) - 1; i >= 0; i-- {
			fmt.Printf("  %s\n", strings.TrimPrefix(refs[i], stash.NamespaceRoot+"/"))
		}
	}
}
