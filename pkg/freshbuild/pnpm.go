package freshbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
// passes through. Matches bin/herd-fresh-build lines 31-35.
func ResolveTarget(root, target string) (pkg string, isPath bool, err error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false, fmt.Errorf("herd-fresh-build: empty target")
	}
	// Absolute or relative path that contains package.json → use .name.
	cand := target
	if !filepath.IsAbs(cand) {
		// Prefer target as given relative to cwd semantics: if root is set and
		// target looks like a path under the repo, join; else try as-is.
		if st, e := os.Stat(cand); e == nil && st.IsDir() {
			// ok, use cand
		} else if root != "" {
			joined := filepath.Join(root, cand)
			if st, e := os.Stat(joined); e == nil && st.IsDir() {
				cand = joined
			}
		}
	}
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
		// Empty / no match → usage-style error (exit 2 at CLI).
		if stdout.Len() == 0 {
			return nil, fmt.Errorf("herd-fresh-build: no packages matched '%s' (unknown name/path, or not a workspace package)", pkg)
		}
		// Non-zero with some output still attempt parse.
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

func (p PnpmProfile) Build(ctx context.Context, root, pkg string, log io.Writer) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	filter := pkg + "..."
	cmd := exec.CommandContext(ctx, p.bin(), "--filter", filter, "run", "build")
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
