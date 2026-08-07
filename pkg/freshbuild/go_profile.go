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
// Go has no stale-dist equivalent; the adapter rebuilds the target package
// without clearing source trees. A clean rebuild → STALE DIST phrasing still
// means "fresh compile succeeded"; failure is always REAL.
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
	// Directory path → treat as package path relative to root if possible.
	cand := target
	if !filepath.IsAbs(cand) {
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			if rel, rerr := filepath.Rel(root, cand); rerr == nil && !strings.HasPrefix(rel, "..") {
				pkg := "./" + filepath.ToSlash(rel)
				if rel == "." {
					pkg = "."
				}
				return pkg, true, nil
			}
			return cand, true, nil
		}
		joined := filepath.Join(root, cand)
		if st, err := os.Stat(joined); err == nil && st.IsDir() {
			pkg := "./" + filepath.ToSlash(filepath.Clean(cand))
			return pkg, true, nil
		}
	} else if st, err := os.Stat(cand); err == nil && st.IsDir() {
		if rel, rerr := filepath.Rel(root, cand); rerr == nil && !strings.HasPrefix(rel, "..") {
			pkg := "./" + filepath.ToSlash(rel)
			if rel == "." {
				pkg = "."
			}
			return pkg, true, nil
		}
	}
	// Bare import path or ./pkg/foo passes through.
	return target, false, nil
}

func (g GoProfile) ChainFor(ctx context.Context, root, pkg string) ([]string, error) {
	_ = ctx
	// Go packages do not have a pnpm-style dependency dist chain. The chain
	// is the single target directory so clear stays a no-op and rebuild is
	// scoped.
	dir := pkg
	if strings.HasPrefix(pkg, "./") || pkg == "." {
		dir = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, "./")))
	} else if !filepath.IsAbs(pkg) {
		// Try as repo-relative path first.
		try := filepath.Join(root, filepath.FromSlash(pkg))
		if st, err := os.Stat(try); err == nil && st.IsDir() {
			dir = try
		} else {
			// Non-dir import path: chain is the module root only.
			dir = root
		}
	}
	return []string{dir}, nil
}

func (g GoProfile) ArtifactNames() ArtifactSpec {
	// No safe universal stale-artifact set for Go source packages.
	return ArtifactSpec{}
}

func (g GoProfile) Build(ctx context.Context, root, pkg string, log io.Writer) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	args := []string{"build", pkg}
	if pkg == "" {
		args = []string{"build", "./..."}
	}
	cmd := exec.CommandContext(ctx, g.bin(), args...)
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
