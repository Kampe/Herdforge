package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// dangerousSignalPatterns match host-wide / sentinel kill literals in
// production Go sources. FAC-174: native preflight must reject these before
// a destructive profile can ship.
var dangerousSignalPatterns = []*regexp.Regexp{
	regexp.MustCompile(`syscall\.Kill\s*\(\s*-1\b`),
	regexp.MustCompile(`syscall\.Kill\s*\(\s*0\s*,`),
	regexp.MustCompile(`syscall\.Kill\s*\(\s*1\s*,`),
	// Process-group form of host-wide: kill(-1) already covered; also bare -1
	// as first arg to any .Kill( that is not clearly a test fake comment.
	regexp.MustCompile(`\.Kill\s*\(\s*-1\b`),
}

// CheckDangerousSignalLiterals walks root for non-test .go files and fails if
// any contain host-wide or sentinel kill literals (FAC-174 preflight).
// Test files (*_test.go) are skipped so mutation baselines may demonstrate
// unguarded FakeBackend behavior without tripping the gate.
func CheckDangerousSignalLiterals(rootDir string) error {
	var hits []string
	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "vendor" || name == "node_modules" ||
				name == "bin" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip the procsignal host backend implementation file: it is the
		// single authorized syscall.Kill site and does not embed -1 literals.
		// (Defense remains runtime validateKillArg.)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := string(data)
		rel, _ := filepath.Rel(rootDir, path)
		for _, re := range dangerousSignalPatterns {
			if re.MatchString(content) {
				hits = append(hits, fmt.Sprintf("%s (pattern %s)", rel, re.String()))
				break
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("signal-literal preflight walk: %w", err)
	}
	if len(hits) > 0 {
		return fmt.Errorf("FAC-174 dangerous signal literals in production sources: %v", hits)
	}
	return nil
}
