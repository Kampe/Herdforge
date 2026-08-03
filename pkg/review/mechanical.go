package review

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MechanicalClassifierVersion is bumped whenever the classification rules
// below change in a way that could flip a verdict. It is embedded in every
// MechanicalVerdict so a stored verdict can be told apart from one produced
// by stale logic.
const MechanicalClassifierVersion = "r0-mechanical-v1"

// FileChange describes one file in a candidate diff, as parsed from `git
// diff` (or an equivalent structured source) — never from a bare filename.
type FileChange struct {
	Path    string   // new path
	OldPath string   // old path, for renames; empty otherwise
	Mode    string   // new file mode, e.g. "100644", "100755"
	OldMode string   // old file mode
	Added   []string // added line content, without the leading '+'
	Removed []string // removed line content, without the leading '-'
}

// CheckStatus is the outcome of one automated verification command
// (preflight, lint, tests, secret scan, link/schema checks, ...).
type CheckStatus string

const (
	CheckPass    CheckStatus = "pass"
	CheckFail    CheckStatus = "fail"
	CheckSkipped CheckStatus = "skipped"
	CheckEmpty   CheckStatus = "empty"   // ran but produced no evidence (e.g. 0 tests executed)
	CheckUnknown CheckStatus = "unknown" // result missing or unparseable
)

// CheckResult is the recorded outcome of one required verification command.
type CheckResult struct {
	Name   string
	Status CheckStatus
	Detail string
}

// MechanicalPolicy carries the per-repository knobs for R0 eligibility.
type MechanicalPolicy struct {
	// AllowTestOnlyMechanical, when false, forces any diff touching only
	// test files to escalate past R0 rather than auto-qualifying.
	AllowTestOnlyMechanical bool
}

// DefaultMechanicalPolicy returns the repository default policy.
func DefaultMechanicalPolicy() MechanicalPolicy {
	return MechanicalPolicy{AllowTestOnlyMechanical: true}
}

// FileCategory is the deterministic classification of one changed file.
type FileCategory string

const (
	CategoryDocs       FileCategory = "docs"
	CategoryFormatting FileCategory = "formatting"
	CategoryMetadata   FileCategory = "metadata"
	CategoryTestOnly   FileCategory = "test"
	CategoryGenerated  FileCategory = "generated"
	CategoryExecutable FileCategory = "executable"
	CategoryConfig     FileCategory = "config"
	CategoryDependency FileCategory = "dependency"
	CategoryHook       FileCategory = "hook"
	CategoryWorkflow   FileCategory = "workflow"
	CategoryCode       FileCategory = "code"
	CategoryAmbiguous  FileCategory = "ambiguous"
)

// mechanicalCategories are the only categories eligible for R0.
var mechanicalCategories = map[FileCategory]bool{
	CategoryDocs:       true,
	CategoryFormatting: true,
	CategoryMetadata:   true,
	CategoryTestOnly:   true,
}

// codeSignatureRe flags lines that look like executable source, used to
// catch production code smuggled into a file whose path claims to be inert
// (docs, metadata).
var codeSignatureRe = regexp.MustCompile(`^\s*(package\s+\w|import\s+["(]|func\s+\w|type\s+\w+\s+(struct|interface)\b|class\s+\w|def\s+\w|#!\s*/|public\s+(class|static)|private\s+(class|static)|module\.exports|export\s+(function|class|default))`)

// generatedMarkerRe matches the standard "generated, do not edit" header
// convention (Go, protobuf, and most codegen tools emit a line like this).
var generatedMarkerRe = regexp.MustCompile(`(?i)code generated.*do not edit`)

// testFuncPrefixRe matches Go's recognized test-harness function prefixes.
var testFuncPrefixRe = regexp.MustCompile(`^(Test|Benchmark|Example|Fuzz)`)

var docsExt = map[string]bool{".md": true, ".txt": true, ".rst": true, ".adoc": true}
var metadataBasenames = map[string]bool{
	"LICENSE": true, "LICENSE.md": true, "NOTICE": true, "CODEOWNERS": true,
	".gitignore": true, ".gitattributes": true, "AUTHORS": true,
}
var dependencyBasenames = map[string]bool{
	"go.mod": true, "go.sum": true, "package.json": true, "package-lock.json": true,
	"yarn.lock": true, "pnpm-lock.yaml": true, "Cargo.toml": true, "Cargo.lock": true,
	"requirements.txt": true, "Gemfile.lock": true, "poetry.lock": true,
}
var configExt = map[string]bool{
	".yaml": true, ".yml": true, ".json": true, ".toml": true, ".ini": true,
	".cfg": true, ".conf": true, ".env": true,
}
var configBasenames = map[string]bool{"Makefile": true, "makefile": true, "Dockerfile": true}

func isTestPath(p string) bool {
	base := path.Base(p)
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return true
	case strings.HasSuffix(base, ".test.ts"), strings.HasSuffix(base, ".test.tsx"),
		strings.HasSuffix(base, ".test.js"), strings.HasSuffix(base, ".spec.ts"),
		strings.HasSuffix(base, ".spec.js"):
		return true
	case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"):
		return true
	case strings.HasSuffix(base, "_test.py"):
		return true
	}
	return false
}

func looksGenerated(p string, added []string) bool {
	lower := strings.ToLower(p)
	if strings.Contains(lower, "/generated/") || strings.HasPrefix(lower, "generated/") ||
		strings.HasSuffix(lower, ".pb.go") || strings.HasSuffix(lower, "_pb2.py") ||
		strings.HasSuffix(lower, ".g.go") || strings.Contains(lower, "/gen/") {
		return true
	}
	for _, line := range added {
		if generatedMarkerRe.MatchString(line) {
			return true
		}
	}
	return false
}

// hasCodeSignature reports whether any added line looks like executable
// source rather than prose/metadata/config.
func hasCodeSignature(added []string) bool {
	for _, line := range added {
		if codeSignatureRe.MatchString(line) {
			return true
		}
	}
	return false
}

// hasSmuggledProductionCode reports whether a test-path file's added lines
// declare a non-test-harness top-level executable declaration — a func, a
// method, or a function-valued var, however it's spelled (grouped or not,
// single- or multi-line, compact or not) — a signal that production logic
// is being smuggled in under a test path.
//
// Go has too many equivalent ways to spell "a var holding a func literal"
// for line-oriented regex matching to enumerate reliably, so when the
// added lines form a standalone-parseable Go source fragment, they're
// parsed for real with go/parser and walked as an AST — immune to
// whitespace/grouping/line-splitting by construction. Most real test-only
// diffs are partial (e.g. one added assertion inside an existing Test
// func) and won't parse standalone; hasSmuggledProductionCodeHeuristic
// covers that case.
func hasSmuggledProductionCode(added []string) bool {
	if smuggled, parsed := hasSmuggledProductionCodeAST(added); parsed {
		return smuggled
	}
	return hasSmuggledProductionCodeHeuristic(added)
}

// hasSmuggledProductionCodeAST parses added as a standalone Go source file
// and reports whether any top-level declaration is a non-test-harness
// func, method, or function-valued var. The second return value is false
// when added doesn't parse as a standalone file — the caller falls back
// to the line-oriented heuristic in that case.
func hasSmuggledProductionCodeAST(added []string) (smuggled, parsed bool) {
	src := "package p\n" + strings.Join(added, "\n")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return false, false
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !testFuncPrefixRe.MatchString(d.Name.Name) {
				return true, true
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, val := range vs.Values {
					if _, isFuncLit := val.(*ast.FuncLit); !isFuncLit {
						continue
					}
					name := "_"
					if i < len(vs.Names) {
						name = vs.Names[i].Name
					}
					if !testFuncPrefixRe.MatchString(name) {
						return true, true
					}
				}
			}
		}
	}
	return false, true
}

// goTok is one token from a single go/scanner pass.
type goTok struct {
	tok token.Token
	lit string
}

// tokenizeGo lexes src in one continuous go/scanner pass and returns every
// non-EOF token. Scanning the whole fragment at once — not line by line —
// is what makes a multiline raw string or block comment resolve to a
// single opaque token no matter how many "added" lines it spans; per-line
// scanning can't see that continuity and mistakes a delimiter inside one
// for a real one. The scanner has no error handler, so a lexically
// unterminated fragment (a diff hunk cut mid-token) doesn't panic — it
// just stops yielding tokens at EOF.
func tokenizeGo(src string) []goTok {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var sc scanner.Scanner
	sc.Init(file, []byte(src), nil, 0)
	var out []goTok
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			return out
		}
		out = append(out, goTok{tok, lit})
	}
}

// skipBalancedParen, given the index of an LPAREN, returns the index just
// past its matching RPAREN (or len(toks) if unterminated).
func skipBalancedParen(toks []goTok, open int) int {
	depth := 1
	i := open + 1
	for i < len(toks) && depth > 0 {
		switch toks[i].tok {
		case token.LPAREN:
			depth++
		case token.RPAREN:
			depth--
		}
		i++
	}
	return i
}

// hasSmuggledProductionCodeHeuristic is the fallback used when added
// doesn't parse as a standalone Go file (the common case for a partial
// diff hunk — e.g. one added assertion inside an existing Test func). It
// tokenizes the fragment once with go/scanner (see tokenizeGo) and matches
// two token sequences a real declaration produces regardless of grouping,
// spacing, or line-splitting:
//
//   - FUNC [ '(' ... ')' ] IDENT '('   — a func or method declaration.
//     Go doesn't allow a named func declaration inside a function body
//     (only anonymous literals), so this is unambiguously top-level
//     wherever it appears — no depth tracking needed.
//   - IDENT '=' FUNC '('               — a var's value is a func literal,
//     but ONLY at brace-depth <= 0 (never confirmed-nested). Package
//     scope is exited only by '{' / '}' — not '(' / ')' — so a var block's
//     own parens don't count. depth <= 0 (not strictly == 0) so a
//     fragment that opens mid-body (its own opening brace outside this
//     hunk, net depth goes negative) still fails closed rather than
//     assuming it's safely local. A local reassignment inside a
//     confirmed-open body (`mock = func(){}` after a `{` this fragment
//     did see) is depth > 0 and is left alone — the common, legitimate
//     test-mocking idiom. There's no Test/Benchmark/Example/Fuzz name
//     exemption here: unlike a func declaration, a var has no such
//     discovery convention, so any depth<=0 func-valued var is flagged
//     regardless of its name. ':=' is the distinct DEFINE token, not
//     ASSIGN, so a local `mock := func(){}` closure never matches either
//     way.
func hasSmuggledProductionCodeHeuristic(added []string) bool {
	toks := tokenizeGo(strings.Join(added, "\n"))

	braceDepth := 0
	for i, t := range toks {
		switch t.tok {
		case token.LBRACE:
			braceDepth++
		case token.RBRACE:
			braceDepth--
		case token.FUNC:
			j := i + 1
			if j < len(toks) && toks[j].tok == token.LPAREN {
				j = skipBalancedParen(toks, j) // skip a method's receiver group
			}
			if j < len(toks) && toks[j].tok == token.IDENT &&
				j+1 < len(toks) && toks[j+1].tok == token.LPAREN {
				if !testFuncPrefixRe.MatchString(toks[j].lit) {
					return true
				}
			}
		case token.ASSIGN:
			if braceDepth <= 0 &&
				i+2 < len(toks) && toks[i+1].tok == token.FUNC && toks[i+2].tok == token.LPAREN {
				return true
			}
		}
	}
	return false
}

// isFormattingOnly reports whether added/removed lines are pairwise
// whitespace-only rewrites of one another (same tokens, same order).
//
// ponytail: line-pairwise whitespace diffing, not full gofmt reformat
// comparison or reordering-aware matching. Upgrade to shelling out to
// `gofmt -l`/`gofmt -d` if reordered-line formatting diffs need R0 too.
func isFormattingOnly(added, removed []string) bool {
	if len(added) == 0 || len(added) != len(removed) {
		return false
	}
	for i := range added {
		// A line carrying a quote character may differ only inside a string
		// (or rune/backtick) literal, where whitespace is semantic, not
		// cosmetic — strings.Fields can't tell those apart, so fail closed
		// and refuse to call it formatting-only rather than risk collapsing
		// a real content change (e.g. "a  b" -> "a b").
		if strings.ContainsAny(added[i], "\"'`") || strings.ContainsAny(removed[i], "\"'`") {
			return false
		}
		a := strings.Join(strings.Fields(added[i]), " ")
		r := strings.Join(strings.Fields(removed[i]), " ")
		if a != r || a == "" {
			return false
		}
	}
	return true
}

// isExecutableMode reports whether a POSIX file mode string carries any
// executable bit.
func isExecutableMode(mode string) bool {
	if len(mode) < 3 {
		return false
	}
	tail := mode[len(mode)-3:]
	for _, c := range tail {
		switch c {
		case '1', '3', '5', '7':
			return true
		}
	}
	return false
}

// namingCategory classifies a path by its own name/location alone — no diff
// content, no mode. Used for both the current path and (on a rename) the
// old path, since a rename that moves production code onto a docs/test
// path must not slip past R0 just because the destination name looks inert.
func namingCategory(p string) FileCategory {
	base := path.Base(p)
	ext := strings.ToLower(path.Ext(base))

	if strings.Contains(p, ".github/workflows/") {
		return CategoryWorkflow
	}
	if strings.Contains(p, ".githooks/") || strings.Contains(p, "/hooks/") || strings.HasPrefix(p, "hooks/") {
		return CategoryHook
	}
	if dependencyBasenames[base] {
		return CategoryDependency
	}
	if configBasenames[base] || configExt[ext] {
		return CategoryConfig
	}
	if isTestPath(p) {
		return CategoryTestOnly
	}
	if docsExt[ext] || strings.HasPrefix(p, "docs/") {
		return CategoryDocs
	}
	if metadataBasenames[base] {
		return CategoryMetadata
	}
	return CategoryCode
}

// ClassifyFile deterministically categorizes one changed file by inspecting
// both its path and its parsed content — never by filename alone. On any
// ambiguity it returns CategoryAmbiguous, which is never R0-eligible.
func ClassifyFile(fc FileChange, policy MechanicalPolicy) FileCategory {
	p := filepath.ToSlash(fc.Path)

	// Generated content always escalates — codegen output is exactly the
	// kind of change a mechanical gate must not rubber-stamp.
	if looksGenerated(p, fc.Added) {
		return CategoryGenerated
	}

	if isExecutableMode(fc.Mode) && fc.Mode != fc.OldMode {
		return CategoryExecutable
	}

	// A rename must be judged on both endpoints: a production file renamed
	// onto a docs/test-looking path is still a production change, and must
	// not inherit the destination's inert-looking category.
	if fc.OldPath != "" {
		oldP := filepath.ToSlash(fc.OldPath)
		if oldP != p && !mechanicalCategories[namingCategory(oldP)] {
			return CategoryAmbiguous
		}
	}

	switch cat := namingCategory(p); cat {
	case CategoryWorkflow, CategoryHook, CategoryDependency, CategoryConfig, CategoryMetadata:
		return cat
	case CategoryTestOnly:
		if hasSmuggledProductionCode(fc.Added) {
			return CategoryAmbiguous
		}
		if !policy.AllowTestOnlyMechanical {
			return CategoryCode
		}
		return CategoryTestOnly
	case CategoryDocs:
		if hasCodeSignature(fc.Added) {
			return CategoryAmbiguous
		}
		return CategoryDocs
	default:
		// A source-looking file whose diff is a pure whitespace/token-order
		// rewrite of itself is a formatting change.
		if isFormattingOnly(fc.Added, fc.Removed) {
			return CategoryFormatting
		}
		return CategoryCode
	}
}

// RequiredChecks returns the set of verification command names a candidate
// diff must have a passing CheckResult for, derived from which categories
// are actually present. The baseline (preflight, secret scan) always
// applies; docs/test presence adds their specific checks.
func RequiredChecks(files []FileChange, policy MechanicalPolicy) []string {
	required := map[string]bool{"preflight": true, "secret-scan": true}
	for _, fc := range files {
		switch ClassifyFile(fc, policy) {
		case CategoryDocs:
			required["link-check"] = true
		case CategoryTestOnly, CategoryFormatting, CategoryCode:
			required["format-lint"] = true
			required["tests"] = true
			required["non-vacuity"] = true
		}
	}
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MechanicalVerdict is the structured, evidence-bound decision artifact
// produced by EvaluateMechanical. It is only valid for the exact candidate
// SHA/patch ID it was computed against.
type MechanicalVerdict struct {
	SHA                string
	PatchID            string
	ClassifierVersion  string
	Tier               RiskTier
	Approved           bool
	Reasons            []string
	Checks             []CheckResult
	VerificationDigest string
}

// StaleFor reports whether this verdict no longer binds the given candidate
// SHA — any new commit invalidates a prior mechanical verdict.
func (v MechanicalVerdict) StaleFor(currentSHA string) bool {
	return v.SHA == "" || v.SHA != currentSHA
}

// verificationDigest computes a deterministic digest over the check results
// so the verdict can be bound to the exact evidence that produced it.
func verificationDigest(sha, patchID string, checks []CheckResult) string {
	sorted := make([]CheckResult, len(checks))
	copy(sorted, checks)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	h := sha256.New()
	h.Write([]byte(sha))
	h.Write([]byte{0})
	h.Write([]byte(patchID))
	for _, c := range sorted {
		h.Write([]byte{0})
		h.Write([]byte(c.Name))
		h.Write([]byte{0})
		h.Write([]byte(c.Status))
		h.Write([]byte{0})
		h.Write([]byte(c.Detail))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// EvaluateMechanical is the fail-closed R0 mechanical gate. It approves a
// candidate only when every changed file classifies into an R0-eligible
// category AND every required verification check is present with status
// CheckPass. Any missing binding, mixed-tier file, ambiguous classification,
// or non-passing/missing check causes it to fail closed: Approved=false and
// Tier escalated to the highest risk observed.
func EvaluateMechanical(sha, patchID string, files []FileChange, checks []CheckResult, policy MechanicalPolicy) MechanicalVerdict {
	v := MechanicalVerdict{
		SHA:               sha,
		PatchID:           patchID,
		ClassifierVersion: MechanicalClassifierVersion,
		Checks:            checks,
	}

	if sha == "" || patchID == "" {
		v.Reasons = append(v.Reasons, "missing SHA/patch-id binding")
	}
	if len(files) == 0 {
		v.Reasons = append(v.Reasons, "no files in candidate diff")
	}

	tier := TierR0RiskMechanical
	var escalatedPaths []string
	for _, fc := range files {
		cat := ClassifyFile(fc, policy)
		if !mechanicalCategories[cat] {
			escalatedPaths = append(escalatedPaths, fc.Path)
			v.Reasons = append(v.Reasons, fc.Path+": category "+string(cat)+" is not R0-eligible")
		}
	}
	if len(escalatedPaths) > 0 {
		escTier := ClassifyRiskTier(escalatedPaths)
		if escTier == TierR0RiskMechanical {
			// Escalated for structural reasons (generated/executable/config/
			// hook/workflow/dependency/ambiguous), not by file extension —
			// ClassifyRiskTier doesn't know those categories, so floor at R1.
			escTier = TierR1RiskStandard
		}
		tier = escTier
	}
	if tier != TierR0RiskMechanical {
		v.Tier = tier
		v.Approved = false
		v.VerificationDigest = verificationDigest(sha, patchID, checks)
		return v
	}

	required := RequiredChecks(files, policy)
	byName := make(map[string]CheckResult, len(checks))
	duplicate := false
	for _, c := range checks {
		if _, exists := byName[c.Name]; exists {
			// Duplicate evidence for the same check is malformed: whichever
			// result "wins" would depend on ordering, and a caller could
			// smuggle a passing duplicate in behind a failing one. Reject
			// outright rather than pick a winner.
			v.Reasons = append(v.Reasons, "duplicate check result for: "+c.Name+" — malformed evidence")
			duplicate = true
			continue
		}
		byName[c.Name] = c
	}
	allPass := len(required) > 0 && !duplicate
	for _, name := range required {
		c, ok := byName[name]
		if !ok {
			v.Reasons = append(v.Reasons, "missing required check: "+name)
			allPass = false
			continue
		}
		if c.Status != CheckPass {
			v.Reasons = append(v.Reasons, "required check "+name+" is "+string(c.Status))
			allPass = false
		}
	}

	v.Tier = TierR0RiskMechanical
	v.Approved = allPass && len(v.Reasons) == 0
	v.VerificationDigest = verificationDigest(sha, patchID, checks)
	return v
}
