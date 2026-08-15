package preflight

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitdir"
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
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return fmt.Errorf("signal-literal preflight open root: %w", err)
	}
	defer root.Close()

	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path == "." {
				return nil
			}
			name := d.Name()
			if name == ".git" || name == "vendor" || name == "node_modules" ||
				name == "bin" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			fullPath := filepath.Join(rootDir, path)
			if gitdir.IsNestedGitDir(fullPath, rootDir) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip the procsignal host backend implementation file: it is the
		// single authorized syscall.Kill site and does not embed -1 literals.
		// (Defense remains runtime validateKillArg.)
		data, err := root.ReadFile(path)
		if err != nil {
			return nil
		}
		content := string(data)
		for _, re := range dangerousSignalPatterns {
			if re.MatchString(content) {
				hits = append(hits, fmt.Sprintf("%s (pattern %s)", path, re.String()))
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
