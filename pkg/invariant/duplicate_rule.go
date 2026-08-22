// Package invariant holds repository-wide structural gates.
//
// FAC-575: six defects in one session had the same root cause -- one rule
// implemented twice, the copies diverging, and the fix applied to whichever copy
// was in front of me:
//
//	FAC-562  tip set built in two places        (budget counted one half)
//	FAC-565  two review-ledger path resolvers   (read a different ledger)
//	FAC-569  two closure acceptance gates       (one never learned Route B)
//	FAC-571  fence message at two call sites    (only one reclassified)
//	FAC-573  agy argv in two routers            (only one flag fixed)
//	FAC-574  two harvest branch generators      (fixed the unused one)
//
// Every one was reported by a consumer, not caught here. The prevention cannot
// be an intention to check, because the intention was already present and failed
// six times. It has to be mechanical.
//
// The mechanical signature they share: a DISTINCTIVE STRING LITERAL appearing in
// more than one non-test file. A rule expressed as a path, flag, filename, or
// message is almost always duplicated logic rather than coincidence.
package invariant

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Occurrence is one literal found in one file.
type Occurrence struct {
	Literal string
	Files   []string
}

// minLiteralLen is the shortest literal considered. Below this, collisions are
// mostly noise ("json", "main").
const minLiteralLen = 12

// ubiquitous are literals that are shared vocabulary rather than a duplicated
// decision. Excluding them is not weakening the gate: a git ref every file
// legitimately names, or an import path, carries no policy. The gate fired on
// exactly these when first run against a real change, which is how the list was
// found rather than guessed.
var ubiquitous = map[string]bool{
	"origin/main": true, "origin/": true, "refs/heads": true, "refs/heads/": true,
	"refs/remotes": true, "refs/remotes/": true, "refs/tags/": true,
	"origin/HEAD": true, "HEAD": true, ".herd/": true, "herd/": true,
	// git's own output-format tokens are vocabulary too: two files asking git
	// to print a refname are not two copies of a decision, any more than two
	// files naming origin/main are. Note what is deliberately NOT here:
	// "--is-ancestor". That one IS a rule — FAC-561 was caused by two copies of
	// the ancestry check disagreeing about what "on this branch" meant — so the
	// gate must keep firing on it.
	"--format=%(refname)": true, "--format=%(refname:short)": true,
	"--format=%H": true, "--format=%(objectname)": true,
}

// Distinctive reports whether a literal is the kind that encodes a rule.
//
// A bare word duplicated across files is usually coincidence. A path, a CLI
// flag, a filename, or a long sentence is a rule someone wrote down twice.
func Distinctive(s string) bool {
	if ubiquitous[s] {
		return false
	}
	// An import path is structure, not policy.
	if strings.Contains(s, "github.com/") || strings.HasPrefix(s, "golang.org/") {
		return false
	}
	// A ref/path prefix is structural even when short: "harvest/" is 8 chars
	// and was the FAC-574 duplicate. Require a separator so bare words do not
	// qualify at this length.
	if strings.Contains(s, "/") && len(s) >= 6 {
		return true
	}
	if len(s) < minLiteralLen {
		return false
	}
	// Long prose is a message contract (the FAC-571 case).
	if len(s) >= 40 {
		return true
	}
	switch {
	case strings.HasPrefix(s, "--"): // CLI flag (FAC-573)
		return true
	case strings.Contains(s, "/"): // path or ref prefix (FAC-565, FAC-574)
		return true
	case strings.Contains(s, "."): // filename (FAC-565)
		return true
	}
	return false
}

// indexKeys returns the keys a literal should be counted under.
//
// The literal itself, plus -- for path-like values -- its leading segment. The
// FAC-574 duplication was "harvest/%s-%s" in one file and "harvest/" in another:
// NOT identical literals, so exact matching missed it entirely. The shared
// namespace prefix is the actual duplicated decision.
func indexKeys(value string) []string {
	keys := []string{value}
	if i := strings.Index(value, "/"); i > 0 {
		prefix := value[:i+1]
		if prefix != value && len(prefix) >= 6 {
			keys = append(keys, prefix)
		}
	}
	return keys
}

// FindDuplicateLiterals returns distinctive literals present in more than one
// non-test Go file under the given roots.
func FindDuplicateLiterals(repoRoot string, roots []string) ([]Occurrence, error) {
	byLiteral := map[string]map[string]bool{}
	for _, root := range roots {
		walkErr := filepath.Walk(filepath.Join(repoRoot, root), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				// A file that does not parse is not this gate's problem.
				return nil
			}
			rel, _ := filepath.Rel(repoRoot, path)
			// Import paths are structure, never policy. Excluding them
			// STRUCTURALLY beats blacklisting path prefixes: the first version
			// excluded github.com/ and golang.org/ and was then flagged on
			// crypto/sha256 and encoding/hex. A denylist of vendors was always
			// going to be incomplete; the AST already knows what an import is.
			importLits := map[token.Pos]bool{}
			for _, imp := range file.Imports {
				if imp.Path != nil {
					importLits[imp.Path.Pos()] = true
				}
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if importLits[lit.Pos()] {
					return true
				}
				value := strings.Trim(lit.Value, "`\"")
				if !Distinctive(value) {
					return true
				}
				for _, key := range indexKeys(value) {
					if byLiteral[key] == nil {
						byLiteral[key] = map[string]bool{}
					}
					byLiteral[key][rel] = true
				}
				return true
			})
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}

	var out []Occurrence
	for lit, files := range byLiteral {
		if len(files) < 2 {
			continue
		}
		names := make([]string, 0, len(files))
		for f := range files {
			names = append(names, f)
		}
		sort.Strings(names)
		out = append(out, Occurrence{Literal: lit, Files: names})
	}
    sort.Slice(out, func(i, j int) bool { return out[i].Literal < out[j].Literal })
	return out, nil
}
