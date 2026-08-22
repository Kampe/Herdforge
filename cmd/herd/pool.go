package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

// runPool is the small operational surface for the durable warm pool. The
// coordinator owns lease IDs; workers only receive the leased path.
func runPool() {
	fs := flag.NewFlagSet("pool", flag.ContinueOnError)
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	size := fs.Int("size", 2, "number of warm worktrees")
	// FAC-577: this flag is the POOL DIRECTORY, but it was named --root, which
	// reads as "repository root". A caller passing `--root .` pointed the pool
	// at the working directory instead of ./.herd/pool, so `release <lease>`
	// answered "lease not found" for a lease that was plainly held, and `list`
	// printed nothing while pool.json held two slots. "Not found" reads as
	// "already released", which is how a consumer concluded a warm slot was free
	// while it was still leased.
	//
	// --pool-root is the documented name and matches `review --pool`'s flag.
	// --root stays as an alias so existing call sites keep working; it has
	// always meant the pool directory, and silently changing its meaning would
	// break anyone who passed the right thing.
	poolDefault := filepath.Join(root, ".herd", "pool")
	poolRoot := fs.String("pool-root", poolDefault, "pool DIRECTORY (not the repository root)")
	poolRootAlias := fs.String("root", "", "alias for --pool-root (pool directory, not the repository root)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "herd pool: %v\n", err)
		os.Exit(2)
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprintln(os.Stderr, "usage: herd pool [--size N] [--pool-root DIR] <ensure|lease|release|gc|list>")
		os.Exit(2)
	}
	resolvedPool := *poolRoot
	if strings.TrimSpace(*poolRootAlias) != "" {
		resolvedPool = *poolRootAlias
	}
	// A directory with no pool state that CONTAINS a pool is almost certainly a
	// repository root passed by mistake. Say that, instead of letting every
	// subsequent lookup answer "not found" — which reads as "already released".
	if hint, misdirected := misdirectedPoolRoot(resolvedPool); misdirected {
		fmt.Fprintf(os.Stderr,
			"herd pool: %q holds no pool state, but %q does — --pool-root takes the POOL DIRECTORY, not the repository root\n",
			resolvedPool, hint)
		os.Exit(2)
	}
	p := worktree.NewPool(root, resolvedPool, *size)
	ctx := context.Background()
	switch fs.Arg(0) {
	case "ensure":
		if err := p.Ensure(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "herd pool ensure: %v\n", err)
			os.Exit(1)
		}
	case "lease":
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "herd pool lease: purpose is required")
			os.Exit(2)
		}
		lease, err := p.Lease(ctx, fs.Arg(1))
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd pool lease: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%s\t%s\t%s\n", lease.LeaseID, lease.Path, lease.Purpose)
	case "release":
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "herd pool release: lease id is required")
			os.Exit(2)
		}
		if err := p.Release(ctx, fs.Arg(1)); err != nil {
			fmt.Fprintf(os.Stderr, "herd pool release: %v\n", err)
			os.Exit(1)
		}
	case "gc":
		if err := p.GC(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "herd pool gc: %v\n", err)
			os.Exit(1)
		}
	case "list":
		slots, err := p.Slots()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd pool list: %v\n", err)
			os.Exit(1)
		}
		for _, slot := range slots {
			fmt.Printf("%s\t%s\t%s\n", slot.Name, slot.Path, slot.Purpose)
		}
	default:
		fmt.Fprintf(os.Stderr, "herd pool: unknown action %s\n", fs.Arg(0))
		os.Exit(2)
	}
}

// misdirectedPoolRoot reports whether a pool root looks like a repository root.
//
// FAC-577: the signal is unambiguous — the given directory has no pool.json but
// <dir>/.herd/pool does. Guessing is not involved: there is a pool, and it is
// not where we were told to look.
func misdirectedPoolRoot(dir string) (string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", false
	}
	nested := filepath.Join(dir, ".herd", "pool")
	_, hereErr := os.Stat(filepath.Join(dir, "pool.json"))
	_, nestedErr := os.Stat(filepath.Join(nested, "pool.json"))
	if nestedErr != nil {
		// No pool underneath, so nothing suggests a misdirection.
		return "", false
	}
	if hereErr != nil {
		return nested, true
	}
	// BOTH exist. The one here is almost certainly stray: a previous misuse of
	// this flag creates an empty pool.json wherever it was pointed, and that
	// file then makes the mistake look legitimate forever. Reporting it is the
	// only way the operator learns which of the two is real — I created exactly
	// such a file in this repository by making this mistake myself.
	return nested, true
}
