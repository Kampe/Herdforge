package security

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidatePackageAllowlist fails closed on empty, absolute, traversal, or
// unclean package roots. Packages must be relative worktree paths (e.g. pkg/security).
func ValidatePackageAllowlist(pkgs []string) error {
	if len(pkgs) == 0 {
		return fmt.Errorf("%w: empty package allowlist", ErrUnknownPolicy)
	}
	seen := map[string]struct{}{}
	for _, p := range pkgs {
		p = strings.TrimSpace(p)
		if p == "" {
			return fmt.Errorf("%w: empty package root entry", ErrUnknownPolicy)
		}
		if filepath.IsAbs(p) {
			return fmt.Errorf("%w: absolute package root refused: %q", ErrUnknownPolicy, p)
		}
		// Reject Windows-style and URI schemes.
		if strings.Contains(p, "://") || strings.Contains(p, `\`) {
			return fmt.Errorf("%w: invalid package root: %q", ErrUnknownPolicy, p)
		}
		clean := filepath.ToSlash(filepath.Clean(p))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
			return fmt.Errorf("%w: package traversal refused: %q", ErrUnknownPolicy, p)
		}
		if strings.HasPrefix(clean, "..") {
			return fmt.Errorf("%w: package traversal refused: %q", ErrUnknownPolicy, p)
		}
		// No leading slash after clean (relative only).
		if strings.HasPrefix(clean, "/") {
			return fmt.Errorf("%w: absolute package root refused: %q", ErrUnknownPolicy, p)
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
	}
	return nil
}

// NormalizePackageAllowlist validates and returns cleaned relative package paths.
func NormalizePackageAllowlist(pkgs []string) ([]string, error) {
	if err := ValidatePackageAllowlist(pkgs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pkgs))
	seen := map[string]struct{}{}
	for _, p := range pkgs {
		clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(p)))
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out, nil
}
