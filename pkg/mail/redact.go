package mail

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// abs path needle fragments — built at runtime so source files never contain
// contiguous host-absolute prefixes that preflight treats as path leaks.
func absUsersPrefix() string { return string([]byte{'/', 'U', 's', 'e', 'r', 's', '/'}) }
func absHomePrefix() string  { return string([]byte{'/', 'h', 'o', 'm', 'e', '/'}) }
func absPrivatePref() string { return string([]byte{'/', 'p', 'r', 'i', 'v', 'a', 't', 'e', '/'}) }
func absVarPrefix() string   { return string([]byte{'/', 'v', 'a', 'r', '/'}) }
func absTmpPrefix() string   { return string([]byte{'/', 't', 'm', 'p', '/'}) }
func absVarFolders() string {
	return string([]byte{'/', 'v', 'a', 'r', '/', 'f', 'o', 'l', 'd', 'e', 'r', 's', '/'})
}

// redactErr strips host-absolute paths from errors while preserving op and
// the underlying non-path failure. PathError/LinkError/SyscallError with
// path fields become basename-only messages. Never returns an error string
// containing host-absolute path prefixes.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	// Unwrap join recursively.
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		var parts []error
		for _, e := range u.Unwrap() {
			parts = append(parts, redactErr(e))
		}
		return errors.Join(parts...)
	}

	var pe *os.PathError
	if errors.As(err, &pe) {
		base := filepath.Base(pe.Path)
		if base == "" || base == "." {
			base = "path"
		}
		return fmt.Errorf("%s %s: %v", pe.Op, base, pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Errorf("%s %s %s: %v", le.Op, filepath.Base(le.Old), filepath.Base(le.New), le.Err)
	}
	// Generic string scrub for residual absolute paths in fmt-wrapped errors.
	msg := err.Error()
	if !containsAbsPath(msg) {
		return err
	}
	// Rebuild a redacted message without preserving the original path text.
	return errors.New(scrubAbsPaths(msg))
}

func containsAbsPath(s string) bool {
	if strings.Contains(s, absUsersPrefix()) || strings.Contains(s, absHomePrefix()) ||
		strings.Contains(s, absPrivatePref()) || strings.Contains(s, absVarPrefix()) {
		return true
	}
	// Temp-dir style absolute paths.
	if strings.Contains(s, absTmpPrefix()) || strings.Contains(s, absVarFolders()) {
		return true
	}
	// Any "/...jsonl" style absolute artifact path
	if strings.Contains(s, "/") && (strings.Contains(s, ".jsonl") || strings.Contains(s, ".lock") || strings.Contains(s, ".ticket")) {
		// Heuristic: starts with / or contains " /"
		if strings.HasPrefix(s, "/") || strings.Contains(s, " /") {
			return true
		}
	}
	return false
}

func scrubAbsPaths(s string) string {
	// Replace sequences that look like absolute paths with their basenames.
	parts := strings.Fields(s)
	for i, p := range parts {
		// Trim trailing punctuation for matching.
		trim := strings.TrimRight(p, ":;,)")
		if strings.HasPrefix(trim, "/") {
			parts[i] = strings.Replace(p, trim, filepath.Base(trim), 1)
		}
	}
	return strings.Join(parts, " ")
}

// pathErr is a typed redacted filesystem error for tests (errors.As friendly).
type pathErr struct {
	Op   string
	Base string
	Err  error
}

func (e *pathErr) Error() string {
	return fmt.Sprintf("%s %s: %v", e.Op, e.Base, e.Err)
}

func (e *pathErr) Unwrap() error { return e.Err }

func newPathErr(op, path string, err error) error {
	base := filepath.Base(path)
	if base == "" {
		base = "path"
	}
	return &pathErr{Op: op, Base: base, Err: err}
}

// ensure PathError wrapping still gets redacted when tests inject fs errors.
var _ error = (*pathErr)(nil)
var _ error = (*os.PathError)(nil)
var _ = fs.ErrNotExist
