package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// PlannerVersion is the plan-builder identity. Bump when plan semantics change
// so consumers can invalidate cached plans.
const PlannerVersion = "1.0.0"

// Language identifies a repository profile language family.
type Language string

const (
	LangGo     Language = "go"
	LangNode   Language = "node"
	LangPython Language = "python"
	LangRust   Language = "rust"
	LangDocs   Language = "docs"
	LangCustom Language = "custom"
)

// GraphState describes local code-review-graph readiness relative to the
// comparison base. Fail-closed consumers check State before trusting hits.
type GraphState string

const (
	GraphAvailable   GraphState = "available"
	GraphStale       GraphState = "available_but_stale"
	GraphMissing     GraphState = "unavailable"
	GraphUnsupported GraphState = "unsupported_language"
)

// GraphHit is one graph query result already resolved by the caller.
// FilePath is repo-relative with forward slashes. Kind is one of
// tests_for, importers_of, callers_of.
type GraphHit struct {
	Kind     string `json:"kind"`
	Target   string `json:"target"`
	FilePath string `json:"file_path"`
	Symbol   string `json:"symbol,omitempty"`
}

// GraphEvidence is injected graph status + hits. Plan never shells out.
type GraphEvidence struct {
	// BuiltAtCommit is the SHA recorded by code-review-graph status.
	// Empty means the graph DB is missing or unbuilt.
	BuiltAtCommit string `json:"built_at_commit,omitempty"`
	// Hits are graph-derived candidates (tests_for / importers_of / callers_of).
	// Order is ignored; Plan sorts deterministically.
	Hits []GraphHit `json:"hits,omitempty"`
	// UnsupportedLanguage is true when the graph cannot index this language.
	UnsupportedLanguage bool `json:"unsupported_language,omitempty"`
}

// Stage orders verification commands in the plan.
type Stage string

const (
	StageLint     Stage = "lint"
	StageBuild    Stage = "build"
	StageTest     Stage = "test"
	StageMutation Stage = "mutation"
	StageBlackbox Stage = "blackbox"
	StageFull     Stage = "full"
)

// PlanCommand is one exact argv to run. Paths with spaces stay single elements.
type PlanCommand struct {
	Stage  Stage    `json:"stage"`
	Argv   []string `json:"argv"`
	Reason string   `json:"reason"`
	Source string   `json:"source"` // owner | graph-tests_for | graph-consumer | profile | escalation
}

// VerificationProfile holds repository-defined command arrays. Every command
// is an exact argv (no shell tokenization). Empty slices mean "not configured".
type VerificationProfile struct {
	Language Language `json:"language"`
	// Lint/Build/Test/Mutation/Blackbox/Full are profile-level defaults.
	Lint     [][]string `json:"lint,omitempty"`
	Build    [][]string `json:"build,omitempty"`
	Test     [][]string `json:"test,omitempty"`
	Mutation [][]string `json:"mutation,omitempty"`
	Blackbox [][]string `json:"blackbox,omitempty"`
	// Full is the broadest verification (e.g. go test ./...). Used on escalate.
	Full [][]string `json:"full,omitempty"`
	// PackageBuild templates one package build. Placeholder "{package}" is
	// replaced with the owner package path (e.g. "./pkg/graph").
	PackageBuild []string `json:"package_build,omitempty"`
	// PackageTest templates one package test. Placeholder "{package}".
	PackageTest []string `json:"package_test,omitempty"`
	// RequireFreshGraph when true returns an error if the graph is missing or
	// stale relative to BaseSHA (fail-closed). When false, Plan falls back to
	// owner-package filters and records caveats.
	RequireFreshGraph bool `json:"require_fresh_graph,omitempty"`
}

// DefaultGoProfile returns a Go monorepo profile matching Herdforge defaults.
func DefaultGoProfile() VerificationProfile {
	return VerificationProfile{
		Language:     LangGo,
		Lint:         [][]string{{"go", "vet", "./..."}},
		Build:        [][]string{{"go", "build", "./..."}},
		Test:         [][]string{{"go", "test", "-count=1", "./..."}},
		Full:         [][]string{{"go", "test", "-count=1", "./..."}},
		PackageBuild: []string{"go", "build", "{package}"},
		PackageTest:  []string{"go", "test", "-count=1", "{package}"},
	}
}

// PlanInput is the pure planner request.
type PlanInput struct {
	BaseSHA      string   `json:"base_sha"`
	CandidateSHA string   `json:"candidate_sha"`
	ChangedPaths []string `json:"changed_paths"`
	// ChangedSymbols are public/exported identifiers introduced or modified.
	ChangedSymbols []string            `json:"changed_symbols,omitempty"`
	Graph          GraphEvidence       `json:"graph"`
	Profile        VerificationProfile `json:"profile"`
	// ForceEscalate, when non-empty, broadens the plan to the full profile
	// regardless of the change set. Callers set it when graph integrity cannot
	// be proven, so a rejected index yields broader verification rather than an
	// empty targeted plan.
	ForceEscalate string `json:"force_escalate,omitempty"`
	// GraphAnchorSHA is the revision the index must be built at for its hits to
	// count as fresh. Empty keeps the FAC-94 default of BaseSHA. herd tests-for
	// sets it to CandidateSHA: an edge about a symbol introduced by the
	// candidate can only exist in an index built on the candidate tree.
	GraphAnchorSHA string `json:"graph_anchor_sha,omitempty"`
}

// TestPlan is the deterministic targeted verification plan.
type TestPlan struct {
	PlannerVersion    string        `json:"planner_version"`
	BaseSHA           string        `json:"base_sha"`
	CandidateSHA      string        `json:"candidate_sha"`
	GraphState        GraphState    `json:"graph_state"`
	GraphBuiltAt      string        `json:"graph_built_at,omitempty"`
	ChangedPaths      []string      `json:"changed_paths"`
	ChangedPackages   []string      `json:"changed_packages"`
	Commands          []PlanCommand `json:"commands"`
	Escalated         bool          `json:"escalated"`
	EscalationReasons []string      `json:"escalation_reasons,omitempty"`
	Caveats           []string      `json:"caveats,omitempty"`
	Reasons           []string      `json:"reasons"`
}

// ValidFor reports whether this plan still applies to the given revision pair.
func (p TestPlan) ValidFor(baseSHA, candidateSHA string) bool {
	if p.PlannerVersion != PlannerVersion {
		return false
	}
	if p.BaseSHA == "" || p.CandidateSHA == "" {
		return false
	}
	return p.BaseSHA == baseSHA && p.CandidateSHA == candidateSHA
}

// MarshalJSON emits stable JSON for machine consumers.
func (p TestPlan) MarshalJSON() ([]byte, error) {
	type alias TestPlan
	return json.Marshal(alias(p))
}

// Plan derives a deterministic targeted test plan from the change set and
// injected graph evidence. Same input always yields the same ordered plan.
//
// Fail-closed rules:
//   - empty base or candidate SHA → error
//   - unsupported language with no usable profile commands → error
//   - Profile.RequireFreshGraph and graph missing/stale → error
//
// Graph hits are advisory: absence never proves missing coverage; presence
// only adds candidate tests. High-risk or uncertain impact escalates to Full.
func Plan(in PlanInput) (*TestPlan, error) {
	base := strings.TrimSpace(in.BaseSHA)
	cand := strings.TrimSpace(in.CandidateSHA)
	if base == "" {
		return nil, errors.New("base_sha is required")
	}
	if cand == "" {
		return nil, errors.New("candidate_sha is required")
	}

	prof := in.Profile
	if prof.Language == "" {
		prof.Language = LangCustom
	}
	if err := validateProfile(prof); err != nil {
		return nil, err
	}

	paths := uniqueSortedPaths(in.ChangedPaths)
	symbols := uniqueSortedStrings(in.ChangedSymbols)
	packages := packagesForPaths(paths, prof.Language)

	anchor := strings.TrimSpace(in.GraphAnchorSHA)
	if anchor == "" {
		anchor = base
	}
	graphState, graphBuilt := classifyGraph(in.Graph, anchor)
	if prof.RequireFreshGraph {
		switch graphState {
		case GraphMissing:
			return nil, errors.New("graph unavailable: require_fresh_graph is set")
		case GraphStale:
			return nil, fmt.Errorf("graph stale (built_at=%s anchor=%s): require_fresh_graph is set", graphBuilt, anchor)
		case GraphUnsupported:
			return nil, errors.New("graph unsupported_language: require_fresh_graph is set")
		}
	}

	var caveats []string
	var escalateReasons []string
	var reasons []string

	// Caveats for non-fresh graph.
	switch graphState {
	case GraphMissing:
		caveats = append(caveats, "graph evidence unavailable; owner-package filters only; missing edges never prove coverage")
	case GraphStale:
		caveats = append(caveats, fmt.Sprintf("graph available but stale (built_at=%s, anchor=%s); refresh before trusting hits", graphBuilt, anchor))
	case GraphUnsupported:
		caveats = append(caveats, "graph does not support this language; owner-package filters only")
	default:
		caveats = append(caveats, "graph results are candidates; missing direct or indirect edges never prove coverage")
	}

	// Escalation: high-risk paths, public symbols, or uncertain impact.
	if reasons := escalationFor(paths, symbols, graphState); len(reasons) > 0 {
		escalateReasons = append(escalateReasons, reasons...)
	}
	if forced := strings.TrimSpace(in.ForceEscalate); forced != "" {
		escalateReasons = append(escalateReasons, "forced escalation: "+forced)
	}

	// Build command list deterministically: stages in fixed order, packages
	// sorted, argv keys unique.
	var cmds []PlanCommand
	seen := map[string]struct{}{}
	add := func(stage Stage, argv []string, reason, source string) {
		if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
			return
		}
		// Defensive copy so callers cannot mutate plan after return.
		cp := append([]string(nil), argv...)
		key := stage.String() + "\x00" + strings.Join(cp, "\x00")
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		cmds = append(cmds, PlanCommand{
			Stage:  stage,
			Argv:   cp,
			Reason: reason,
			Source: source,
		})
		reasons = append(reasons, fmt.Sprintf("%s:%s:%s", stage, source, reason))
	}

	// Profile-level lint (once, always first when configured).
	for _, argv := range prof.Lint {
		add(StageLint, argv, "profile lint", "profile")
	}

	// Owner package builds (sorted packages).
	for _, pkg := range packages {
		if len(prof.PackageBuild) > 0 {
			add(StageBuild, expandPackageArgv(prof.PackageBuild, pkg),
				fmt.Sprintf("owner package build for %s", pkg), "owner")
		}
	}
	// If no package builds were produced, fall back to profile Build once.
	if len(packages) == 0 || len(prof.PackageBuild) == 0 {
		for _, argv := range prof.Build {
			add(StageBuild, argv, "profile build", "profile")
		}
	}

	// Owner package tests.
	for _, pkg := range packages {
		if len(prof.PackageTest) > 0 {
			add(StageTest, expandPackageArgv(prof.PackageTest, pkg),
				fmt.Sprintf("owner package test for %s", pkg), "owner")
		}
	}
	// No packages (docs-only / unknown paths): use profile Test if any.
	if len(packages) == 0 {
		for _, argv := range prof.Test {
			add(StageTest, argv, "profile test (no owner package)", "profile")
		}
	}

	// Graph-derived tests only when graph is available and fresh.
	if graphState == GraphAvailable {
		testHits, consumerPkgs := partitionHits(in.Graph.Hits, packages, prof.Language)
		// A complete, revision-bound index that still has no tests_for edge for
		// a changed exported production symbol is uncovered surface, not proof
		// of coverage: broaden to consumer/blackbox/full instead of shipping a
		// targeted plan that exercises nothing.
		if len(testHits) == 0 && looksExported(symbols) && hasProductionCode(paths) {
			escalateReasons = append(escalateReasons,
				"no graph tests_for edges for changed exported production symbols")
		}
		for _, hit := range testHits {
			pkg := packageForPath(hit.FilePath, prof.Language)
			if pkg == "" || len(prof.PackageTest) == 0 {
				continue
			}
			add(StageTest, expandPackageArgv(prof.PackageTest, pkg),
				fmt.Sprintf("graph tests_for %s → %s", hit.Target, hit.FilePath),
				"graph-tests_for")
		}
		for _, cpkg := range consumerPkgs {
			if len(prof.PackageTest) == 0 {
				continue
			}
			add(StageTest, expandPackageArgv(prof.PackageTest, cpkg),
				fmt.Sprintf("graph consumer package %s", cpkg),
				"graph-consumer")
		}
	}

	// Mutation from profile when present.
	for _, argv := range prof.Mutation {
		add(StageMutation, argv, "profile mutation", "profile")
	}

	// Escalation: full suite + blackbox when required. Re-normalize because
	// the graph pass may append after escalationFor already sorted.
	escalateReasons = uniqueSortedStrings(escalateReasons)
	escalated := len(escalateReasons) > 0
	if escalated {
		for _, argv := range prof.Full {
			add(StageFull, argv, "escalated full suite: "+strings.Join(escalateReasons, "; "), "escalation")
		}
		// If Full empty, fall back to profile Test as broadest available.
		if len(prof.Full) == 0 {
			for _, argv := range prof.Test {
				add(StageFull, argv, "escalated full suite (profile test): "+strings.Join(escalateReasons, "; "), "escalation")
			}
		}
		for _, argv := range prof.Blackbox {
			add(StageBlackbox, argv, "escalated blackbox/consumer: "+strings.Join(escalateReasons, "; "), "escalation")
		}
	} else if hasPublicBehavior(paths, symbols) && len(prof.Blackbox) > 0 {
		// Public behavior change without other escalation still pulls blackbox
		// when the profile configures it (AC: black-box/consumer when configured).
		for _, argv := range prof.Blackbox {
			add(StageBlackbox, argv, "public behavior change includes configured blackbox", "profile")
		}
	}

	// Stable sort of reasons (commands already insertion-ordered by stage+pkg).
	// Re-sort commands by stage rank then argv join for absolute byte stability
	// even if future stages insert out of order.
	sort.SliceStable(cmds, func(i, j int) bool {
		ri, rj := stageRank(cmds[i].Stage), stageRank(cmds[j].Stage)
		if ri != rj {
			return ri < rj
		}
		return strings.Join(cmds[i].Argv, "\x00") < strings.Join(cmds[j].Argv, "\x00")
	})

	plan := &TestPlan{
		PlannerVersion:    PlannerVersion,
		BaseSHA:           base,
		CandidateSHA:      cand,
		GraphState:        graphState,
		GraphBuiltAt:      graphBuilt,
		ChangedPaths:      paths,
		ChangedPackages:   packages,
		Commands:          cmds,
		Escalated:         escalated,
		EscalationReasons: escalateReasons,
		Caveats:           caveats,
		Reasons:           reasons,
	}
	// Recompute reasons from final sorted commands for byte stability.
	plan.Reasons = make([]string, 0, len(cmds))
	for _, c := range cmds {
		plan.Reasons = append(plan.Reasons, fmt.Sprintf("%s:%s:%s", c.Stage, c.Source, c.Reason))
	}
	return plan, nil
}

func (s Stage) String() string { return string(s) }

func stageRank(s Stage) int {
	switch s {
	case StageLint:
		return 0
	case StageBuild:
		return 1
	case StageTest:
		return 2
	case StageMutation:
		return 3
	case StageBlackbox:
		return 4
	case StageFull:
		return 5
	default:
		return 99
	}
}

func validateProfile(p VerificationProfile) error {
	switch p.Language {
	case LangGo, LangNode, LangPython, LangRust, LangDocs, LangCustom:
	default:
		return fmt.Errorf("unsupported language %q", p.Language)
	}
	// Fail-closed: non-docs profiles need at least one runnable command family.
	if p.Language != LangDocs {
		if len(p.PackageTest) == 0 && len(p.Test) == 0 && len(p.Full) == 0 {
			return errors.New("verification profile has no test or full commands")
		}
	}
	return nil
}

func classifyGraph(g GraphEvidence, base string) (GraphState, string) {
	if g.UnsupportedLanguage {
		return GraphUnsupported, strings.TrimSpace(g.BuiltAtCommit)
	}
	built := strings.TrimSpace(g.BuiltAtCommit)
	if built == "" {
		return GraphMissing, ""
	}
	if built != base {
		return GraphStale, built
	}
	return GraphAvailable, built
}

func expandPackageArgv(tmpl []string, pkg string) []string {
	out := make([]string, len(tmpl))
	for i, part := range tmpl {
		out[i] = strings.ReplaceAll(part, "{package}", pkg)
	}
	return out
}

func uniqueSortedPaths(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, p := range in {
		p = normalizeRepoPath(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func uniqueSortedStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func normalizeRepoPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	p = filepath.ToSlash(p)
	// Reject absolute paths (repo-relative only). Empty signals drop.
	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return ""
	}
	// path.Clean resolves . and .. segments. After cleaning, any path that
	// still starts with ".." has escaped the repo root and must be rejected
	// (e.g. "../x", "foo/../../x"). Intra-repo rewrites like "a/../b" → "b"
	// remain valid.
	cleaned := path.Clean(p)
	cleaned = strings.TrimPrefix(cleaned, "./")
	if cleaned == "" || cleaned == "." {
		return ""
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	// Defense in depth: never keep a residual ".." segment after Clean.
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." {
			return ""
		}
	}
	return cleaned
}

// packagesForPaths returns sorted unique owner package identifiers for paths.
func packagesForPaths(paths []string, lang Language) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, p := range paths {
		pkg := packageForPath(p, lang)
		if pkg == "" {
			continue
		}
		if _, ok := seen[pkg]; ok {
			continue
		}
		seen[pkg] = struct{}{}
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}

// packageForPath maps a repo-relative file to its testable package path.
// Go: pkg/graph/foo.go → ./pkg/graph
// Node monorepo: packages/foo/src/x.ts → packages/foo (when package.json root)
// Docs/markdown: empty (no package).
func packageForPath(filePath string, lang Language) string {
	p := normalizeRepoPath(filePath)
	if p == "" {
		return ""
	}
	// Skip pure docs / config that are not package code for Go/Rust defaults.
	base := path.Base(p)
	ext := path.Ext(base)
	switch lang {
	case LangDocs:
		return ""
	case LangGo:
		if ext != ".go" {
			return ""
		}
		dir := path.Dir(p)
		if dir == "." || dir == "" {
			return ""
		}
		// Skip testdata and vendor trees as owner packages.
		parts := strings.Split(dir, "/")
		for _, part := range parts {
			if part == "testdata" || part == "vendor" {
				return ""
			}
		}
		return "./" + dir
	case LangRust:
		if ext != ".rs" {
			return ""
		}
		// Cargo package root: first segment under crates/ or .
		if strings.HasPrefix(p, "crates/") {
			rest := strings.TrimPrefix(p, "crates/")
			seg := strings.SplitN(rest, "/", 2)[0]
			if seg != "" {
				return "crates/" + seg
			}
		}
		dir := path.Dir(p)
		if dir == "." || dir == "" {
			return ""
		}
		return dir
	case LangNode:
		// packages|apps|tools / <name> / ...
		parts := strings.Split(p, "/")
		if len(parts) >= 2 {
			switch parts[0] {
			case "packages", "apps", "tools":
				return parts[0] + "/" + parts[1]
			}
		}
		dir := path.Dir(p)
		if dir == "." || dir == "" {
			return ""
		}
		return dir
	case LangPython:
		if ext != ".py" {
			return ""
		}
		dir := path.Dir(p)
		if dir == "." || dir == "" {
			return ""
		}
		return dir
	default: // custom: directory of the file when it looks like source
		dir := path.Dir(p)
		if dir == "." || dir == "" {
			return ""
		}
		return dir
	}
}

// partitionHits splits graph hits into sorted tests_for hits and sorted
// consumer package names (importers/callers outside owner packages).
func partitionHits(hits []GraphHit, ownerPkgs []string, lang Language) ([]GraphHit, []string) {
	ownerSet := make(map[string]struct{}, len(ownerPkgs))
	for _, p := range ownerPkgs {
		ownerSet[p] = struct{}{}
	}

	var tests []GraphHit
	consumerSet := make(map[string]struct{})

	for _, h := range hits {
		kind := strings.TrimSpace(h.Kind)
		fp := normalizeRepoPath(h.FilePath)
		if fp == "" {
			continue
		}
		h.FilePath = fp
		h.Target = strings.TrimSpace(h.Target)
		switch kind {
		case "tests_for":
			tests = append(tests, h)
		case "importers_of", "callers_of":
			pkg := packageForPath(fp, lang)
			if pkg == "" {
				continue
			}
			if _, isOwner := ownerSet[pkg]; isOwner {
				continue
			}
			consumerSet[pkg] = struct{}{}
		}
	}

	sort.Slice(tests, func(i, j int) bool {
		if tests[i].FilePath != tests[j].FilePath {
			return tests[i].FilePath < tests[j].FilePath
		}
		if tests[i].Target != tests[j].Target {
			return tests[i].Target < tests[j].Target
		}
		return tests[i].Kind < tests[j].Kind
	})

	consumers := make([]string, 0, len(consumerSet))
	for p := range consumerSet {
		consumers = append(consumers, p)
	}
	sort.Strings(consumers)
	return tests, consumers
}

// escalationFor returns sorted reasons to expand verification beyond owners.
func escalationFor(paths, symbols []string, state GraphState) []string {
	var reasons []string
	add := func(r string) { reasons = append(reasons, r) }

	for _, p := range paths {
		lp := strings.ToLower(p)
		switch {
		case strings.Contains(lp, "/auth") || strings.HasPrefix(lp, "auth") ||
			strings.Contains(lp, "secret") || strings.Contains(lp, "credential"):
			add("auth/secrets path: " + p)
		case strings.Contains(lp, "schema") || strings.HasSuffix(lp, ".proto") ||
			strings.HasSuffix(lp, ".graphql") || strings.Contains(lp, "migration"):
			add("schema/migration path: " + p)
		case strings.HasPrefix(lp, "deploy/") || strings.HasPrefix(lp, "infra/") ||
			strings.HasPrefix(lp, "terraform/") || strings.HasPrefix(lp, "helm/") ||
			strings.HasPrefix(lp, "manifests/") || strings.HasPrefix(lp, ".github/workflows/"):
			add("infrastructure path: " + p)
		case strings.HasPrefix(p, "cmd/"):
			add("command entrypoint: " + p)
		case strings.HasPrefix(p, "pkg/daemon/") || strings.HasPrefix(p, "pkg/dispatch/") ||
			strings.HasPrefix(p, "pkg/claim/") || strings.HasPrefix(p, "pkg/router/") ||
			strings.HasPrefix(p, "pkg/lifecycle/") || strings.HasPrefix(p, "pkg/outbox/"):
			add("core orchestration path: " + p)
		}
	}

	if len(symbols) > 0 && looksExported(symbols) {
		add("changed public/exported symbols")
	}

	// Uncertain impact: production code changed but graph not trustworthy.
	if hasProductionCode(paths) {
		switch state {
		case GraphMissing, GraphStale, GraphUnsupported:
			add("uncertain impact: production code changed without fresh graph")
		}
	}

	return uniqueSortedStrings(reasons)
}

func looksExported(symbols []string) bool {
	for _, s := range symbols {
		if s == "" {
			continue
		}
		// Go export: leading uppercase; also accept explicit "export " prefixes.
		r := []rune(s)
		if len(r) > 0 && r[0] >= 'A' && r[0] <= 'Z' {
			return true
		}
		if strings.HasPrefix(s, "export ") {
			return true
		}
	}
	return false
}

func hasPublicBehavior(paths, symbols []string) bool {
	if looksExported(symbols) {
		return true
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "pkg/") && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			return true
		}
		if strings.HasPrefix(p, "cmd/") {
			return true
		}
	}
	return false
}

func hasProductionCode(paths []string) bool {
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, ".md") {
			continue
		}
		ext := path.Ext(p)
		switch ext {
		case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".toml", ".yaml", ".yml":
			// Skip pure docs-ish yaml under docs/
			if strings.HasPrefix(p, "docs/") {
				continue
			}
			return true
		}
	}
	return false
}
