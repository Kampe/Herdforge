package freshbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	// Paths are resolved relative to root (not process cwd) when root is set.
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
	// DryRunClearLine is the dry-run message about what would be cleared.
	// Must not claim a clear the profile will not perform.
	DryRunClearLine() string
	// ClearedLine is printed after ClearChain, before rebuild.
	// Must not claim artifacts were cleared when ArtifactNames is empty.
	ClearedLine(n int) string
	// CleanLine is the success verdict after a zero-exit rebuild.
	// Must not claim "STALE DIST" unless the profile actually clears artifacts.
	CleanLine(n int) string
	// RealErrorHeader is the stderr banner for a genuine rebuild failure.
	RealErrorHeader() string
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

// Empty reports whether the spec clears nothing.
func (s ArtifactSpec) Empty() bool {
	return len(s.Dirs) == 0 && len(s.Files) == 0 && len(s.Globs) == 0
}

// PlanSummary is a human label for what the spec would clear (e.g. "dist/ + *.tsbuildinfo").
func (s ArtifactSpec) PlanSummary() string {
	if s.Empty() {
		return "nothing"
	}
	var parts []string
	for _, d := range s.Dirs {
		parts = append(parts, d+"/")
	}
	for _, f := range s.Files {
		parts = append(parts, f)
	}
	for _, g := range s.Globs {
		parts = append(parts, g)
	}
	return strings.Join(parts, " + ")
}

// DetectProfile picks an adapter from repository markers at root.
//
// Order and honesty:
//  1. Explicit pnpm workspace markers (pnpm-lock.yaml / pnpm-workspace.yaml) → pnpm
//  2. go.mod → go (checked before bare package.json so a Go repo that ships a
//     docs-tooling package.json is not misrouted to pnpm)
//  3. package.json only when packageManager declares pnpm (or no competing
//     npm/yarn lock); package-lock.json / yarn.lock → refuse, never guess npm
//  4. otherwise → nil
func DetectProfile(root string) Profile {
	root = filepath.Clean(root)
	if fileExists(filepath.Join(root, "pnpm-lock.yaml")) || fileExists(filepath.Join(root, "pnpm-workspace.yaml")) {
		return PnpmProfile{}
	}
	if fileExists(filepath.Join(root, "go.mod")) {
		return GoProfile{}
	}
	if fileExists(filepath.Join(root, "package.json")) {
		if fileExists(filepath.Join(root, "package-lock.json")) || fileExists(filepath.Join(root, "yarn.lock")) {
			// Declared npm/yarn manager — refuse rather than run pnpm --filter.
			return nil
		}
		if declaresPnpm(filepath.Join(root, "package.json")) {
			return PnpmProfile{}
		}
		// package.json alone with no packageManager / lock is ambiguous.
		return nil
	}
	return nil
}

// declaresPnpm reads package.json packageManager / packageManager field.
// True only when the declared manager is pnpm (e.g. "pnpm@9.0.0").
func declaresPnpm(packageJSON string) bool {
	raw, err := os.ReadFile(packageJSON)
	if err != nil {
		return false
	}
	var meta struct {
		PackageManager string `json:"packageManager"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false
	}
	pm := strings.ToLower(strings.TrimSpace(meta.PackageManager))
	return strings.HasPrefix(pm, "pnpm")
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
	sort.Strings(out)
	return out, nil
}
