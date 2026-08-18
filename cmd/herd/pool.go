package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

// runPool is the small operational surface for the durable warm pool. The
// coordinator owns lease IDs; workers only receive the leased path.
func runPool() {
	fs := flag.NewFlagSet("pool", flag.ContinueOnError)
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	size := fs.Int("size", 2, "number of warm worktrees")
	poolRoot := fs.String("root", filepath.Join(root, ".herd", "pool"), "pool directory")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "herd pool: %v\n", err)
		os.Exit(2)
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprintln(os.Stderr, "usage: herd pool [--size N] [--root DIR] <ensure|lease|release|gc|list>")
		os.Exit(2)
	}
	p := worktree.NewPool(root, *poolRoot, *size)
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
