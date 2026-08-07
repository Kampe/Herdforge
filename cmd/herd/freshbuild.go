package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/freshbuild"
)

// runFreshBuild ports bin/herd-fresh-build: prove whether a cross-package
// type/build error is REAL or just stale dist (profile-driven; pnpm is one
// adapter). See pkg/freshbuild.
func runFreshBuild() {
	args := os.Args[2:]
	target, dry, err := parseFreshBuildArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}
	if target == "" {
		fmt.Println(freshbuild.UsageLine)
		os.Exit(2)
	}

	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-fresh-build: %v\n", err)
		os.Exit(2)
	}
	// Prefer repository root when invoked inside a worktree (walk up for
	// pnpm-lock / go.mod / .git). Stay in cwd when markers are local.
	if r := findRepoRoot(root); r != "" {
		root = r
	}
	// Realpath the root (zsh :A). Getwd keeps logical $PWD; pnpm `exec pwd`
	// returns the physical path — without EvalSymlinks, normalizeChainDirs
	// rejects every chain dir under a symlink checkout or /tmp→/private/tmp.
	if resolved, rerr := filepath.EvalSymlinks(root); rerr == nil && resolved != "" {
		root = resolved
	}

	v, err := freshbuild.FreshBuild(context.Background(), freshbuild.Options{
		Root:   root,
		Target: target,
		DryRun: dry,
	})
	if err != nil {
		var u *freshbuild.UsageError
		if errors.As(err, &u) {
			// Usage-style messages already include the herd-fresh-build prefix
			// or the usage line; print as-is to stderr for die(), stdout for usage.
			msg := u.Error()
			if strings.HasPrefix(msg, "usage:") {
				fmt.Println(msg)
			} else {
				fmt.Fprintln(os.Stderr, msg)
			}
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "herd-fresh-build: %v\n", err)
		os.Exit(2)
	}
	if v == nil {
		os.Exit(2)
	}
	os.Exit(v.Rc)
}

// parseFreshBuildArgs accepts:
//
//	herd fresh-build <pkg-or-path> [--dry-run]
//	herd fresh-build --dry-run <pkg-or-path>
//
// A bare --dry-run with no target is usage (exit 2).
func parseFreshBuildArgs(args []string) (target string, dry bool, err error) {
	for _, a := range args {
		switch a {
		case "--dry-run":
			dry = true
		default:
			if strings.HasPrefix(a, "-") {
				return "", false, fmt.Errorf("herd-fresh-build: unknown flag %s", a)
			}
			if target != "" {
				return "", false, fmt.Errorf("herd-fresh-build: unexpected argument %q", a)
			}
			target = a
		}
	}
	return target, dry, nil
}

func findRepoRoot(start string) string {
	dir := start
	for {
		for _, marker := range []string{"pnpm-lock.yaml", "pnpm-workspace.yaml", "go.mod", ".git"} {
			if st, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				_ = st
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
