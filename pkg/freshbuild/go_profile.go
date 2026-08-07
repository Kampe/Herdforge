package freshbuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GoProfile is a non-pnpm adapter so fresh-build is not locked to Node.
// Go has no stale-dist equivalent: ArtifactNames is empty, no clear runs, and
// success/failure messages never claim a STALE DIST diagnosis.
type GoProfile struct {
	GoBin string
}

func (g GoProfile) bin() string {
	if s := strings.TrimSpace(g.GoBin); s != "" {
		return s
	}
	return "go"
}

func (g GoProfile) Name() string { return "go" }

func (g GoProfile) Available() error {
	return g.AvailableWith(exec.LookPath)
}

func (g GoProfile) AvailableWith(lookPath func(string) (string, error)) error {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if _, err := lookPath(g.bin()); err != nil {
		return fmt.Errorf("herd-fresh-build: go required")
	}
	return nil
}

func (g GoProfile) ResolveTarget(root, target string) (string, bool, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false, fmt.Errorf("herd-fresh-build: empty target")
	}
	root = filepath.Clean(root)

	// Prefer root-relative resolution so CLI walk-up to repo root and package
	// path stay consistent (never stat cwd-local path then build under root).
	if filepath.IsAbs(target) {
		if st, err := os.Stat(target); err == nil && st.IsDir() {
			rel, rerr := filepath.Rel(root, target)
			if rerr != nil || strings.HasPrefix(rel, "..") {
				return "", true, fmt.Errorf("herd-fresh-build: path %q is outside repository root", target)
			}
			return goPkgFromRel(rel), true, nil
		}
		return target, false, nil
	}

	// Relative: only accept as a path when it exists under root.
	joined := filepath.Join(root, filepath.FromSlash(target))
	if st, err := os.Stat(joined); err == nil && st.IsDir() {
		rel, rerr := filepath.Rel(root, joined)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			return "", true, fmt.Errorf("herd-fresh-build: path %q is outside repository root", target)
		}
		return goPkgFromRel(rel), true, nil
	}

	// Bare import path or ./pkg/foo that is not a directory under root passes through.
	return target, false, nil
}

func goPkgFromRel(rel string) string {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." {
		return "."
	}
	return "./" + rel
}

func (g GoProfile) ChainFor(ctx context.Context, root, pkg string) ([]string, error) {
	_ = ctx
	root = filepath.Clean(root)
	// Single-directory chain for the resolved package path under root.
	dir, err := goDirForPkg(root, pkg)
	if err != nil {
		return nil, err
	}
	return []string{dir}, nil
}

func goDirForPkg(root, pkg string) (string, error) {
	pkg = strings.TrimSpace(pkg)
	if pkg == "" || pkg == "." {
		return root, nil
	}
	if strings.HasPrefix(pkg, "./") {
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, "./")))
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return "", fmt.Errorf("herd-fresh-build: no packages matched '%s' (unknown name/path, or not a workspace package)", pkg)
		}
		return dir, nil
	}
	if filepath.IsAbs(pkg) {
		rel, err := filepath.Rel(root, pkg)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("herd-fresh-build: path %q is outside repository root", pkg)
		}
		return filepath.Clean(pkg), nil
	}
	// Try as repo-relative path.
	try := filepath.Join(root, filepath.FromSlash(pkg))
	if st, err := os.Stat(try); err == nil && st.IsDir() {
		return try, nil
	}
	// Non-dir import path: chain is the module root (build still uses pkg).
	return root, nil
}

func (g GoProfile) ArtifactNames() ArtifactSpec {
	// No safe universal stale-artifact set for Go source packages.
	return ArtifactSpec{}
}

func (g GoProfile) ChainHeader(pkg string, n int) string {
	return fmt.Sprintf("herd-fresh-build: chain for %s = %d package(s) (target only; Go profile does not walk dependencies):", pkg, n)
}

func (g GoProfile) DryRunClearLine() string {
	return "herd-fresh-build: --dry-run, would clear nothing (Go profile has no stale-artifact clear step), then rebuild. Nothing changed."
}

func (g GoProfile) ClearedLine(n int) string {
	return fmt.Sprintf("herd-fresh-build: no artifact clear for %d package(s) (Go profile); rebuilding...", n)
}

func (g GoProfile) CleanLine(n int) string {
	return fmt.Sprintf("herd-fresh-build: fresh package build clean (%d package(s)) -- rebuild succeeded; Go profile does not diagnose stale dist.", n)
}

func (g GoProfile) RealErrorHeader() string {
	return "herd-fresh-build: REAL build error:"
}

func (g GoProfile) Build(ctx context.Context, root, pkg string, log io.Writer) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, g.bin(), "build", pkg)
	cmd.Dir = root
	cmd.Stdout = log
	cmd.Stderr = log
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		rc := ee.ExitCode()
		if rc < 0 {
			rc = 1
		}
		return rc, err
	}
	return 1, err
}

func (g GoProfile) ClassifyFailure(root string, log []byte, rc int) (VerdictKind, string) {
	_, _, _ = root, log, rc
	return VerdictRealError, ""
}
