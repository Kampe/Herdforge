package freshbuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Profile is an ecosystem adapter for fresh-build diagnosis.
// FAC-94 supplies VerificationProfile for test planning; this interface is the
// parallel build-artifact seam (clear + rebuild + classify).
type Profile interface {
	// Name is a stable adapter id (pnpm, go, ...).
	Name() string
	// Available reports whether the required toolchain is on PATH.
	Available() error
	// AvailableWith allows tests to inject LookPath.
	AvailableWith(lookPath func(string) (string, error)) error
	// ResolveTarget maps a path-or-name to a package identifier.
	ResolveTarget(root, target string) (pkg string, isPath bool, err error)
	// ChainFor returns absolute dependency-plus-self package directories.
	ChainFor(ctx context.Context, root, pkg string) ([]string, error)
	// ArtifactNames lists directory/file basenames cleared per chain member.
	// Directory names are removed entirely; file globs use filepath.Glob.
	ArtifactNames() ArtifactSpec
	// Build rebuilds the chain and writes combined output to log.
	// Returns the process exit code (never swallowed into a bool).
	Build(ctx context.Context, root, pkg string, log io.Writer) (int, error)
	// ClassifyFailure maps a non-zero rebuild into clean/node_modules/real.
	ClassifyFailure(root string, log []byte, rc int) (VerdictKind, string)
}

// ArtifactSpec describes what a profile may delete inside a chain directory.
// Explicit allow-list is the destructive boundary: nothing else is removed.
type ArtifactSpec struct {
	// Dirs are basenames removed via RemoveAll (e.g. "dist").
	Dirs []string
	// Files are exact basenames removed (e.g. "tsconfig.tsbuildinfo").
	Files []string
	// Globs are filepath.Glob patterns relative to each chain dir
	// (e.g. "*.tsbuildinfo").
	Globs []string
}

// DetectProfile picks an adapter from repository markers at root.
// Prefer pnpm (has the original stale-dist semantics), then Go.
func DetectProfile(root string) Profile {
	root = filepath.Clean(root)
	if fileExists(filepath.Join(root, "pnpm-lock.yaml")) || fileExists(filepath.Join(root, "pnpm-workspace.yaml")) {
		return PnpmProfile{}
	}
	if fileExists(filepath.Join(root, "package.json")) {
		// package.json without pnpm lock still uses the pnpm adapter only if
		// pnpm is the declared manager; otherwise refuse rather than guess npm.
		if fileExists(filepath.Join(root, "package-lock.json")) || fileExists(filepath.Join(root, "yarn.lock")) {
			return nil
		}
		return PnpmProfile{}
	}
	if fileExists(filepath.Join(root, "go.mod")) {
		return GoProfile{}
	}
	return nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// normalizeChainDirs abs-normalizes, dedupes, sorts, and enforces the
// destructive boundary: every dir must live under root.
func normalizeChainDirs(root string, dirs []string) ([]string, error) {
	root = filepath.Clean(root)
	seen := map[string]struct{}{}
	var out []string
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, fmt.Errorf("freshbuild: abs chain dir %q: %w", d, err)
		}
		abs = filepath.Clean(abs)
		rel, err := filepath.Rel(root, abs)
		if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
			return nil, fmt.Errorf("freshbuild: chain dir %q escapes repository root %q", abs, root)
		}
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	sortStrings(out)
	return out, nil
}

func sortStrings(s []string) {
	// local sort to avoid importing sort in every file; tiny N.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
