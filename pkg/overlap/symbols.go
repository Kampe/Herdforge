package overlap

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SymbolRef is one declaration of a symbol on an added line: the tip branch
// that added it and the file:line evidence from the hunk line counter.
type SymbolRef struct {
	Symbol   string
	Branch   string
	Location string
}

// SymbolHot is a symbol added on >= minTips distinct unmerged tips in >= 2
// different files: two lanes inventing the same mechanism under different
// paths.
type SymbolHot struct {
	Symbol string
	Tips   int
	Files  int
	Refs   []SymbolRef
}

// MarshalJSON emits the reference CLI's symbols-mode JSON contract:
// {"symbol":S,"tips":N,"files":N,"refs":[{"branch":B,"location":L}]}.
func (sh SymbolHot) MarshalJSON() ([]byte, error) {
	type refWire struct {
		Branch   string `json:"branch"`
		Location string `json:"location"`
	}
	type wire struct {
		Symbol string    `json:"symbol"`
		Tips   int       `json:"tips"`
		Files  int       `json:"files"`
		Refs   []refWire `json:"refs"`
	}
	refs := make([]refWire, 0, len(sh.Refs))
	for _, r := range sh.Refs {
		refs = append(refs, refWire{Branch: r.Branch, Location: r.Location})
	}
	return json.Marshal(wire{Symbol: sh.Symbol, Tips: sh.Tips, Files: sh.Files, Refs: refs})
}

// symbolExtensions mirrors the reference's diff filter: only sources where a
// freshly-added function is a first-class declaration.
var symbolExtensions = []string{
	"*.zsh", "*.sh", "*.ts", "*.tsx", "*.js", "*.jsx", "*.mjs", "*.cjs",
}

var (
	controlWords = map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "case": true,
		"catch": true, "do": true, "else": true, "try": true, "finally": true,
		"with": true,
	}
	// funcDeclRe mirrors awk regex 1:
	// ^[[:space:]]*(export[[:space:]]+)?(default[[:space:]]+)?(async[[:space:]]+)?function[[:space:]]+[A-Za-z_$][A-Za-z0-9_$]*
	funcDeclRe = regexp.MustCompile(`^[[:space:]]*(export[[:space:]]+)?(default[[:space:]]+)?(async[[:space:]]+)?function[[:space:]]+[A-Za-z_$][A-Za-z0-9_$]*`)
	// arrowDeclRe mirrors awk regex 2:
	// ^[[:space:]]*(export[[:space:]]+)?(async[[:space:]]+)?[A-Za-z_$][A-Za-z0-9_$]*[[:space:]]*\([^)]*\)[[:space:]]*[{=:]
	arrowDeclRe = regexp.MustCompile(`^[[:space:]]*(export[[:space:]]+)?(async[[:space:]]+)?[A-Za-z_$][A-Za-z0-9_$]*[[:space:]]*\([^)]*\)[[:space:]]*[{=:]`)
)

// isControl excludes language keywords that parse as the second regex but are
// never symbols (if (x) { ... }).
func isControl(name string) bool {
	return controlWords[name]
}

// ExtractDecls runs the reference's added-line declaration extraction for one
// unmerged tip against a base (main) ref. It returns only declarations on
// ADDED lines, each with branch and file:line evidence. It is deliberately a
// syntactic advisory, not semantic similarity.
func (o *Overlap) ExtractDecls(ctx context.Context, branch, mainRef string) ([]SymbolRef, error) {
	args := append([]string{"diff", "--unified=0", mainRef + "..." + branch, "--"}, symbolExtensions...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = o.RepoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var decls []SymbolRef
	file := ""
	line := 0
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "+++ b/"):
			// file=substr($0,7) — "+++ b/" is six bytes; rest is the path.
			file = l[6:]
			continue
		case strings.HasPrefix(l, "@@"):
			line = parseHunkHeader(l)
			continue
		case !strings.HasPrefix(l, "+") || strings.HasPrefix(l, "+++"):
			continue
		}
		text := l[1:] // awk substr($0,2)
		if m := funcDeclRe.FindString(text); m != "" {
			// sub(/^.*function[[:space:]]+/, "", decl) -> name.
			if i := strings.LastIndexAny(m, " \t"); i >= 0 {
				name := strings.TrimSpace(m[i:])
				if name != "" && !isControl(name) {
					decls = append(decls, SymbolRef{Symbol: name, Branch: branch, Location: file + ":" + strconv.Itoa(line)})
				}
			}
		} else if m := arrowDeclRe.FindString(text); m != "" {
			decl := strings.TrimSpace(m)
			if i := strings.IndexByte(decl, '('); i >= 0 {
				decl = strings.TrimSpace(decl[:i])
			}
			// strip leading export/type as the reference does
			for _, pre := range []string{"export", "async", "default"} {
				if strings.HasPrefix(decl, pre+" ") {
					decl = strings.TrimSpace(strings.TrimPrefix(decl, pre))
				}
			}
			if decl != "" && !isControl(decl) {
				decls = append(decls, SymbolRef{Symbol: decl, Branch: branch, Location: file + ":" + strconv.Itoa(line)})
			}
		}
		line++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return decls, nil
}

// SymbolOverlaps returns the symbols added on >= minTips distinct unmerged
// tips in >= 2 different files. It mirrors the reference symbols loop: same
// distinct-tip census as the file mode, then per-symbol de-dupe of tips
// (branches) and files, keeping every (branch,location) ref. Order is
// deterministic: refs follow for-each-ref order, hot symbols sort by name.
func (o *Overlap) SymbolOverlaps(ctx context.Context, mainRef string, minTips int) []SymbolHot {
	tips, err := o.unmergedTips(ctx, mainRef)
	if err != nil {
		return nil
	}

	type agg struct {
		refs  []SymbolRef
		tips  map[string]bool
		files map[string]bool
	}
	bySymbol := make(map[string]*agg)
	for _, b := range tips {
		decls, err := o.ExtractDecls(ctx, b, mainRef)
		if err != nil {
			continue
		}
		for _, d := range decls {
			a := bySymbol[d.Symbol]
			if a == nil {
				a = &agg{tips: make(map[string]bool), files: make(map[string]bool)}
				bySymbol[d.Symbol] = a
			}
			a.refs = append(a.refs, d)
			a.tips[b] = true
			if i := strings.LastIndexByte(d.Location, ':'); i >= 0 {
				a.files[d.Location[:i]] = true
			}
		}
	}

	var hot []SymbolHot
	for _, sym := range sortedKeys(bySymbol) {
		a := bySymbol[sym]
		if len(a.tips) >= minTips && len(a.files) >= 2 {
			hot = append(hot, SymbolHot{Symbol: sym, Tips: len(a.tips), Files: len(a.files), Refs: a.refs})
		}
	}
	return hot
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parseHunkHeader reconstructs the awk hunk line counter: after the LAST '+'
// in the header, drop from the first ',' and from the first space, then take
// the number. e.g. "@@ -0,0 +1,5 @@" -> 1.
func parseHunkHeader(h string) int {
	rest := h
	if i := strings.LastIndexByte(rest, '+'); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.IndexByte(rest, ','); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		rest = rest[:i]
	}
	n, _ := strconv.Atoi(strings.TrimSpace(rest))
	return n
}
