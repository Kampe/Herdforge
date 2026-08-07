package security

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// absPathRE matches Unix absolute path tokens for durable-artifact redaction.
var absPathRE = regexp.MustCompile(`/[A-Za-z0-9._\-][A-Za-z0-9._\-/]*`)

// RedactAbsPaths replaces host absolute paths with repo-relative or opaque tokens
// for durable security events and persisted metadata.
// Absolute paths may still exist ephemerally in process memory/argv for the OS.
func RedactAbsPaths(detail string, sharedCheckout string) string {
	if detail == "" {
		return detail
	}
	if sharedCheckout != "" {
		if s, err := filepath.Abs(sharedCheckout); err == nil && s != "" {
			detail = strings.ReplaceAll(detail, s, "$HERD_ROOT")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if h, err2 := filepath.Abs(home); err2 == nil {
			detail = strings.ReplaceAll(detail, h, "$HOME")
		}
	}
	// Remaining absolute-looking segments.
	detail = absPathRE.ReplaceAllString(detail, "$ABS")
	return detail
}

// RelIdentity returns a repo-relative path for display when under shared root.
func RelIdentity(absPath, sharedCheckout string) string {
	if absPath == "" {
		return ""
	}
	if sharedCheckout == "" {
		return "$ABS"
	}
	shared, err := filepath.Abs(sharedCheckout)
	if err != nil {
		return "$ABS"
	}
	abs, err := filepath.Abs(absPath)
	if err != nil {
		return "$ABS"
	}
	rel, err := filepath.Rel(shared, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "$ABS"
	}
	if rel == "." {
		return "."
	}
	return filepath.ToSlash(rel)
}
