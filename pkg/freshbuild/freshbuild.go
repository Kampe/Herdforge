// Package freshbuild ports bin/herd-fresh-build: prove whether a
// cross-package type/build error is REAL or just stale build artifacts.
//
// FAC-67 is profile-driven (FAC-94 seam). Pnpm is one adapter; Go is another.
// The shared flow resolves a target, builds its dependency-plus-self chain,
// optionally clears ONLY that chain's artifacts (never repo-wide), rebuilds
// fresh, and emits a three-way verdict:
//
//   - clean rebuild  → prior error was STALE DIST (exit 0)
//   - missing modules declared in the lockfile → STALE/MISSING install (rc)
//   - any other failure → REAL build error (rc)
package freshbuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// VerdictKind is the three-way diagnosis.
type VerdictKind string

const (
	VerdictClean       VerdictKind = "clean"        // stale dist, not real
	VerdictNodeModules VerdictKind = "node_modules" // stale/missing install
	VerdictRealError   VerdictKind = "real_error"   // genuine code/build error
	VerdictDryRun      VerdictKind = "dry_run"
	VerdictUsage       VerdictKind = "usage"
)

// Chain is the target package plus its dependencies as absolute dirs.
type Chain struct {
	Target string   // resolved package name
	Dirs   []string // absolute dirs, deduped + sorted
}

// Sections returns repo-root-relative paths for plan printing.
func (c *Chain) Sections(root string) []string {
	if c == nil {
		return nil
	}
	root = filepath.Clean(root)
	out := make([]string, 0, len(c.Dirs))
	for _, d := range c.Dirs {
		rel := d
		if root != "" {
			if r, err := filepath.Rel(root, d); err == nil && !strings.HasPrefix(r, "..") {
				rel = filepath.ToSlash(r)
			}
		}
		out = append(out, rel)
	}
	return out
}

// Options configure a fresh-build run.
type Options struct {
	Root   string // repository root
	Target string // package name or path
	DryRun bool

	// Profile selects the ecosystem adapter. Nil → DetectProfile(Root).
	Profile Profile

	// Runner overrides the profile's build command (tests inject fakes).
	// When nil, Profile.Build is used.
	Runner Runner

	// ChainFn overrides chain resolution (tests inject fakes).
	// When nil, Profile.ChainFor is used.
	ChainFn ChainResolver

	// LookPath overrides toolchain discovery (tests inject fakes).
	// Signature matches exec.LookPath. When nil, Profile.Available is used.
	LookPath func(file string) (string, error)

	// Stdout/Stderr receive human plan + verdict lines. Default to os.Stdout/Stderr.
	Stdout io.Writer
	Stderr io.Writer

	// TempDir is used for the build log (defaults to os.TempDir / $TMPDIR).
	TempDir string
}

// Runner shells out to rebuild the chain. Returns the process exit code.
// Implementations must not swallow the real exit code.
type Runner func(ctx context.Context, root, pkg string, log io.Writer) (int, error)

// ChainResolver returns absolute package directories for pkg (target + deps).
type ChainResolver func(ctx context.Context, root, pkg string) ([]string, error)

// Verdict is the machine-readable outcome.
type Verdict struct {
	Kind           VerdictKind
	Rc             int
	Pkg            string
	Chain          *Chain
	LogPath        string
	MissingModule  string
	NodeModulesHit bool
	Message        string
}

// UsageError is a fail-closed CLI/usage failure (exit 2).
type UsageError struct {
	Msg string
}

func (e *UsageError) Error() string {
	if e == nil {
		return "freshbuild: usage"
	}
	return e.Msg
}

// FreshBuild resolves, optionally clears, rebuilds, and classifies.
func FreshBuild(ctx context.Context, opts Options) (*Verdict, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("freshbuild: resolve root: %w", err)
		}
		root = wd
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("freshbuild: abs root: %w", err)
	}

	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	target := strings.TrimSpace(opts.Target)
	if target == "" {
		return &Verdict{Kind: VerdictUsage, Rc: 2, Message: UsageLine}, &UsageError{Msg: UsageLine}
	}

	prof := opts.Profile
	if prof == nil {
		prof = DetectProfile(root)
	}
	if prof == nil {
		return nil, &UsageError{Msg: "freshbuild: no supported repository profile (need pnpm-lock.yaml or go.mod)"}
	}

	if opts.LookPath != nil {
		if err := prof.AvailableWith(opts.LookPath); err != nil {
			return nil, &UsageError{Msg: err.Error()}
		}
	} else if err := prof.Available(); err != nil {
		return nil, &UsageError{Msg: err.Error()}
	}

	pkg, _, err := prof.ResolveTarget(root, target)
	if err != nil {
		return nil, &UsageError{Msg: err.Error()}
	}

	var dirs []string
	if opts.ChainFn != nil {
		dirs, err = opts.ChainFn(ctx, root, pkg)
	} else {
		dirs, err = prof.ChainFor(ctx, root, pkg)
	}
	if err != nil {
		return nil, &UsageError{Msg: err.Error()}
	}
	if len(dirs) == 0 {
		return nil, &UsageError{Msg: fmt.Sprintf("herd-fresh-build: no packages matched '%s' (unknown name/path, or not a workspace package)", pkg)}
	}
	dirs, err = normalizeChainDirs(root, dirs)
	if err != nil {
		return nil, err
	}
	chain := &Chain{Target: pkg, Dirs: dirs}

	fmt.Fprintf(stdout, "herd-fresh-build: chain for %s = %d package(s) (target + dependencies):\n", pkg, len(dirs))
	for _, s := range chain.Sections(root) {
		fmt.Fprintf(stdout, "  %s\n", s)
	}

	if opts.DryRun {
		fmt.Fprintln(stdout, "herd-fresh-build: --dry-run, would clear dist/ + *.tsbuildinfo in each above, then rebuild. Nothing changed.")
		return &Verdict{Kind: VerdictDryRun, Rc: 0, Pkg: pkg, Chain: chain, Message: "dry-run"}, nil
	}

	if err := ClearChain(root, dirs, prof.ArtifactNames()); err != nil {
		return nil, fmt.Errorf("freshbuild: clear: %w", err)
	}
	fmt.Fprintf(stdout, "herd-fresh-build: cleared dist + .tsbuildinfo for %d chain package(s), rebuilding fresh...\n", len(dirs))

	tmpDir := opts.TempDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	logFile, err := os.CreateTemp(tmpDir, "herd-fresh-build.*")
	if err != nil {
		return nil, fmt.Errorf("freshbuild: create log: %w", err)
	}
	logPath := logFile.Name()

	runner := opts.Runner
	if runner == nil {
		runner = prof.Build
	}
	rc, runErr := runner(ctx, root, pkg, logFile)
	_ = logFile.Close()
	if runErr != nil && rc == 0 {
		// Runner failed without an exit code (e.g. start error).
		rc = 1
	}

	if rc == 0 {
		_ = os.Remove(logPath)
		msg := fmt.Sprintf("herd-fresh-build: fresh chain clean (%d pkgs rebuilt) -- any prior cross-package error was STALE DIST, not real.", len(dirs))
		fmt.Fprintln(stdout, msg)
		return &Verdict{Kind: VerdictClean, Rc: 0, Pkg: pkg, Chain: chain, Message: msg}, nil
	}

	logBytes, _ := os.ReadFile(logPath)
	kind, missing := prof.ClassifyFailure(root, logBytes, rc)
	v := &Verdict{
		Kind:           kind,
		Rc:             rc,
		Pkg:            pkg,
		Chain:          chain,
		LogPath:        logPath,
		MissingModule:  missing,
		NodeModulesHit: kind == VerdictNodeModules,
	}

	switch kind {
	case VerdictNodeModules:
		fmt.Fprintln(stderr, "herd-fresh-build: STALE/MISSING node_modules, not stale dist and not a code error.")
		fmt.Fprintf(stderr, "  '%s' is in pnpm-lock.yaml but unresolved after a fresh build. Run: pnpm install\n", missing)
		_ = os.Remove(logPath)
		v.LogPath = ""
		v.Message = "stale/missing node_modules"
	default:
		fmt.Fprintln(stderr, "herd-fresh-build: REAL build error in the freshly-built chain (NOT stale dist):")
		for _, line := range errorTail(logBytes, 20) {
			fmt.Fprintf(stderr, "  %s\n", line)
		}
		fmt.Fprintf(stderr, "herd-fresh-build: full log at %s (rc=%d)\n", logPath, rc)
		v.Message = "real build error"
	}
	return v, nil
}

// UsageLine is the exact no-target usage text (ported from bin/herd-fresh-build).
const UsageLine = "usage: herd fresh-build <pkg-or-path> [--dry-run]"
