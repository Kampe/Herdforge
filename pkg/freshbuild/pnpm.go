package freshbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Kampe/Herdforge/pkg/slot"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// PnpmProfile is the original herd-fresh-build adapter for pnpm workspaces.
// Chain filter uses trailing "..." (pkg + dependencies), never dependents.
type PnpmProfile struct {
	// Pnpm is the binary name/path (default "pnpm"). Override via HERD_FRESH_BUILD_PNPM.
	Pnpm string
}

func (p PnpmProfile) bin() string {
	if s := strings.TrimSpace(p.Pnpm); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv("HERD_FRESH_BUILD_PNPM")); s != "" {
		return s
	}
	return "pnpm"
}

func (p PnpmProfile) Name() string { return "pnpm" }

func (p PnpmProfile) Available() error {
	return p.AvailableWith(exec.LookPath)
}

func (p PnpmProfile) AvailableWith(lookPath func(string) (string, error)) error {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if _, err := lookPath(p.bin()); err != nil {
		return fmt.Errorf("herd-fresh-build: pnpm required")
	}
	return nil
}

func (p PnpmProfile) ResolveTarget(root, target string) (string, bool, error) {
	return ResolveTarget(root, target)
}

// ResolveTarget maps a path argument to its package.json name; a bare name
// passes through. Paths are preferred under root when root is set.
func ResolveTarget(root, target string) (pkg string, isPath bool, err error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false, fmt.Errorf("herd-fresh-build: empty target")
	}
	cand := resolvePathUnderRoot(root, target)
	pkgJSON := filepath.Join(cand, "package.json")
	if st, e := os.Stat(pkgJSON); e == nil && !st.IsDir() {
		raw, rerr := os.ReadFile(pkgJSON)
		if rerr != nil {
			return "", true, fmt.Errorf("herd-fresh-build: read %s: %w", pkgJSON, rerr)
		}
		var meta struct {
			Name string `json:"name"`
		}
		if jerr := json.Unmarshal(raw, &meta); jerr != nil {
			return "", true, fmt.Errorf("herd-fresh-build: parse %s: %w", pkgJSON, jerr)
		}
		if strings.TrimSpace(meta.Name) == "" {
			return "", true, fmt.Errorf("herd-fresh-build: %s has no name", pkgJSON)
		}
		return meta.Name, true, nil
	}
	// Bare name (or non-dir path) passes through unchanged.
	return target, false, nil
}

// resolvePathUnderRoot prefers root-joined relative targets so CLI root walk-up
// and path resolution agree. Absolute paths and bare non-dir names are unchanged.
func resolvePathUnderRoot(root, target string) string {
	if filepath.IsAbs(target) {
		return target
	}
	if root != "" {
		joined := filepath.Join(root, target)
		if st, err := os.Stat(joined); err == nil && st.IsDir() {
			return joined
		}
	}
	// Fall back to as-given (cwd-relative) only when root join is not a dir.
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		return target
	}
	return target
}

func (p PnpmProfile) ChainFor(ctx context.Context, root, pkg string) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Trailing "..." is the pnpm filter for pkg + dependencies (not dependents).
	filter := pkg + "..."
	cmd := exec.CommandContext(ctx, p.bin(), "--filter", filter, "-c", "exec", "pwd")
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Fail-closed: partial stdout on non-zero exit is not a trusted chain.
		// Upstream zsh runs under set -euo pipefail and aborts on any failure.
		if stdout.Len() == 0 {
			return nil, fmt.Errorf("herd-fresh-build: no packages matched '%s' (unknown name/path, or not a workspace package)", pkg)
		}
		return nil, fmt.Errorf("herd-fresh-build: pnpm chain resolution failed for %q: %w", pkg, err)
	}
	var dirs []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		dirs = append(dirs, line)
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("herd-fresh-build: no packages matched '%s' (unknown name/path, or not a workspace package)", pkg)
	}
	return dirs, nil
}

func (p PnpmProfile) ArtifactNames() ArtifactSpec {
	return ArtifactSpec{
		Dirs:  []string{"dist"},
		Files: []string{"tsconfig.tsbuildinfo"},
		Globs: []string{"*.tsbuildinfo"},
	}
}

func (p PnpmProfile) ChainHeader(pkg string, n int) string {
	return fmt.Sprintf("herd-fresh-build: chain for %s = %d package(s) (target + dependencies):", pkg, n)
}

func (p PnpmProfile) DryRunClearLine() string {
	return "herd-fresh-build: --dry-run, would clear " + p.ArtifactNames().PlanSummary() + " in each above, then rebuild. Nothing changed."
}

func (p PnpmProfile) ClearedLine(n int) string {
	return fmt.Sprintf("herd-fresh-build: cleared %s for %d chain package(s), rebuilding fresh...", p.ArtifactNames().PlanSummary(), n)
}

func (p PnpmProfile) CleanLine(n int) string {
	return fmt.Sprintf("herd-fresh-build: fresh chain clean (%d pkgs rebuilt) -- any prior cross-package error was STALE DIST, not real.", n)
}

func (p PnpmProfile) RealErrorHeader() string {
	return "herd-fresh-build: REAL build error in the freshly-built chain (NOT stale dist):"
}

func (p PnpmProfile) Build(ctx context.Context, root, pkg string, log io.Writer) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	filter := pkg + "..."
	var err error
	var rc int
	semaphore, semErr := slot.Default()
	if semErr != nil {
		return 1, semErr
	}
	err = semaphore.With(ctx, "fresh-build: pnpm", slot.DefaultTimeout, func() error {
		cmd := exec.CommandContext(ctx, p.bin(), "--filter", filter, "run", "build")
		cmd.Dir = root
		cmd.Stdout = log
		cmd.Stderr = log
		err = cmd.Run()
		if err == nil {
			rc = 0
			return nil
		}
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
			if rc < 0 {
				rc = 1
			}
		}
		return err
	})
	if semErr := err; semErr != nil && rc == 0 {
		return 1, semErr
	}
	if err == nil {
		return 0, nil
	}
	return rc, err
}

// cannotFindModule re matches the TypeScript/Node diagnostic used by the zsh
// source: Cannot find module '[@a-z0-9._/-]+'
var cannotFindModule = regexp.MustCompile(`Cannot find module '([@a-z0-9._/-]+)'`)

func (p PnpmProfile) ClassifyFailure(root string, log []byte, rc int) (VerdictKind, string) {
	_ = rc
	m := cannotFindModule.FindSubmatch(log)
	if m == nil {
		return VerdictRealError, ""
	}
	mod := string(m[1])
	lockPath := filepath.Join(root, "pnpm-lock.yaml")
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		// No lockfile → cannot claim node_modules diagnosis; treat as real.
		return VerdictRealError, mod
	}
	// grep -qF equivalent: literal contains, not regex.
	if bytes.Contains(lock, []byte(mod)) {
		return VerdictNodeModules, mod
	}
	return VerdictRealError, mod
}
