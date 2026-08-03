package classify

import (
	"path"
	"sort"
	"strings"
)

// pathClass is the coarse category of a single path for rule evaluation.
type pathClass int

const (
	classUnknown pathClass = iota
	classDocs
	classMechanical
	classTest
	classGenerated
	classConfigBounded
	classDependency
	classWorkflowConfig
	classCI
	classProduction
	classAuthSecrets
	classDestructive
	classInfrastructure
	classCoreOrchestration
)

func evaluateRules(paths []string, changes []FileChange, symbols []string, pol Policy) []RuleMatch {
	if len(paths) == 0 && len(changes) == 0 && len(symbols) == 0 {
		return nil // caller applies unknown.conservative
	}

	byClass := map[pathClass][]string{}
	for _, p := range paths {
		c := classifyPath(p, pol)
		byClass[c] = append(byClass[c], p)
	}

	var rules []RuleMatch
	add := func(id string, tier Tier, reason string, ps []string) {
		if len(ps) == 0 && id != "r2.public_api" {
			return
		}
		cp := append([]string(nil), ps...)
		sort.Strings(cp)
		rules = append(rules, RuleMatch{ID: id, Tier: tier, Reason: reason, Paths: cp})
	}

	// R3
	add("r3.auth_secrets", TierR3, "auth, secrets, credentials, or payment-sensitive paths", byClass[classAuthSecrets])
	add("r3.destructive", TierR3, "destructive or irreversible operation paths", byClass[classDestructive])
	add("r3.core_orchestration", TierR3, "core orchestration or control-plane packages", byClass[classCoreOrchestration])
	add("r3.infrastructure", TierR3, "infrastructure, deploy, or container orchestration manifests", byClass[classInfrastructure])
	add("r3.ci_workflows", TierR3, "CI/CD workflow definitions (deploy surface)", byClass[classCI])

	// R2
	add("r2.production_source", TierR2, "production source or feature code", byClass[classProduction])
	add("r2.dependency_manifest", TierR2, "dependency or lockfile manifest change", byClass[classDependency])
	add("r2.workflow_config", TierR2, "workflow, herd, or operational configuration", byClass[classWorkflowConfig])

	// Public API / exported symbol changes always reach R2.
	if hasExportedSymbol(symbols) {
		rules = append(rules, RuleMatch{
			ID:     "r2.public_api",
			Tier:   TierR2,
			Reason: "public or exported symbol change",
			Paths:  nil,
		})
	}

	// R1
	add("r1.tests", TierR1, "test-only or testdata paths", byClass[classTest])
	add("r1.generated", TierR1, "generated or vendored artifacts (not docs-only)", byClass[classGenerated])
	add("r1.config_bounded", TierR1, "bounded tooling/editor configuration", byClass[classConfigBounded])

	// Renames of non-docs content cannot be docs-only.
	if renameNonDocs := renameNonDocPaths(changes, pol); len(renameNonDocs) > 0 {
		add("r1.rename_non_docs", TierR1, "rename of non-documentation paths", renameNonDocs)
	}

	// R0
	add("r0.docs", TierR0, "documentation or prose-only paths", byClass[classDocs])
	add("r0.mechanical", TierR0, "purely mechanical metadata", byClass[classMechanical])

	// Unknown paths: fail upward with explicit evidence.
	if unk := byClass[classUnknown]; len(unk) > 0 {
		add("unknown.path_conservative", TierR2, "unrecognized path kind; conservative R2", unk)
	}

	// Policy substring extensions.
	if extra := matchSubstrings(paths, pol.ExtraR3Substrings); len(extra) > 0 {
		add("policy.extra_r3", TierR3, "repository policy R3 path substring", extra)
	}
	if extra := matchSubstrings(paths, pol.ExtraR2Substrings); len(extra) > 0 {
		add("policy.extra_r2", TierR2, "repository policy R2 path substring", extra)
	}
	if extra := matchSubstrings(paths, pol.ExtraR1Substrings); len(extra) > 0 {
		add("policy.extra_r1", TierR1, "repository policy R1 path substring", extra)
	}

	return rules
}

func matchSubstrings(paths, subs []string) []string {
	if len(subs) == 0 {
		return nil
	}
	var hit []string
	for _, p := range paths {
		low := strings.ToLower(p)
		for _, s := range subs {
			s = strings.ToLower(strings.TrimSpace(s))
			if s == "" {
				continue
			}
			if strings.Contains(low, s) {
				hit = append(hit, p)
				break
			}
		}
	}
	return hit
}

func renameNonDocPaths(changes []FileChange, pol Policy) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, c := range changes {
		// Treat as rename when Kind says so or OldPath is present.
		if c.Kind != ChangeRename && c.OldPath == "" {
			continue
		}
		for _, p := range []string{c.Path, c.OldPath} {
			p = normalizePath(p)
			if p == "" {
				continue
			}
			cl := classifyPath(p, pol)
			if cl == classDocs || cl == classMechanical {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func hasExportedSymbol(symbols []string) bool {
	for _, s := range symbols {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// Go-style exported: leading uppercase unicode letter, or explicit export markers.
		r := []rune(s)
		if len(r) > 0 && r[0] >= 'A' && r[0] <= 'Z' {
			return true
		}
		if strings.HasPrefix(s, "export ") || strings.Contains(s, "::") {
			return true
		}
	}
	return false
}

func classifyPath(p string, pol Policy) pathClass {
	low := strings.ToLower(p)
	base := strings.ToLower(path.Base(p))
	ext := strings.ToLower(path.Ext(p))

	// Policy core orchestration first (R3).
	for _, pref := range pol.CoreOrchestrationPrefixes {
		pref = strings.ToLower(strings.TrimSpace(pref))
		if pref == "" {
			continue
		}
		pref = strings.ReplaceAll(pref, "\\", "/")
		if strings.HasPrefix(low, strings.TrimPrefix(pref, "./")) {
			return classCoreOrchestration
		}
	}

	// Auth / secrets / payment (R3).
	if isAuthSecretsPath(low, base) {
		return classAuthSecrets
	}

	// Destructive (R3).
	if isDestructivePath(low, base) {
		return classDestructive
	}

	// Infrastructure (R3).
	if isInfrastructurePath(low, base) {
		return classInfrastructure
	}

	// CI workflows (R3).
	if isCIPath(low) {
		return classCI
	}

	// Dependency manifests (R2).
	if isDependencyManifest(base) {
		return classDependency
	}

	// Workflow / operational config (R2).
	if isWorkflowConfig(low, base) {
		return classWorkflowConfig
	}

	// Generated (R1 floor).
	if isGeneratedPath(low, base, ext) {
		return classGenerated
	}

	// Tests (R1).
	if isTestPath(low, base, ext) {
		return classTest
	}

	// Docs (R0).
	if isDocsPath(low, base, ext) {
		return classDocs
	}

	// Bounded tooling config (R1).
	if isBoundedConfig(base, ext) {
		return classConfigBounded
	}

	// Mechanical metadata (R0).
	if isMechanical(base) {
		return classMechanical
	}

	// Production source (R2).
	if isProductionSource(low, ext) {
		return classProduction
	}

	return classUnknown
}

func isAuthSecretsPath(low, base string) bool {
	needles := []string{
		"/auth/", "auth.", "pkg/auth", "pkg/security",
		"secret", "credential", "password", "passwd",
		"/jwt", "oauth", "oidc", "saml",
		"payment", "billing", "/money/", "pkg/money",
		"apikey", "api_key", "private_key", "id_rsa",
	}
	for _, n := range needles {
		if strings.Contains(low, n) {
			return true
		}
	}
	switch base {
	case ".env", ".env.local", ".env.production", ".env.development":
		return true
	}
	if strings.HasPrefix(base, ".env.") {
		return true
	}
	if strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
		// Public certs are still credential material; treat conservatively.
		return true
	}
	if strings.Contains(base, "secret") || strings.Contains(base, "credential") {
		return true
	}
	return false
}

func isDestructivePath(low, base string) bool {
	needles := []string{
		"destroy", "teardown", "force_delete", "force-delete",
		"rm_rf", "rm-rf", "wipe_", "wipe-", "truncate",
		"drop_table", "drop-table", "hard_delete", "hard-delete",
	}
	for _, n := range needles {
		if strings.Contains(low, n) || strings.Contains(base, n) {
			return true
		}
	}
	return false
}

func isInfrastructurePath(low, base string) bool {
	if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") ||
		base == "containerfile" || strings.HasPrefix(base, "docker-compose") ||
		base == "compose.yaml" || base == "compose.yml" ||
		base == "skaffold.yaml" || base == "chart.yaml" {
		return true
	}
	prefixes := []string{
		"deploy/", "infra/", "infrastructure/", "k8s/", "kubernetes/",
		"manifests/", "charts/", "helm/", "terraform/", "tf/",
	}
	for _, pref := range prefixes {
		if strings.HasPrefix(low, pref) || strings.Contains(low, "/"+pref) {
			return true
		}
	}
	if strings.HasSuffix(low, ".tf") || strings.HasSuffix(low, ".tfvars") {
		return true
	}
	return false
}

func isCIPath(low string) bool {
	return strings.HasPrefix(low, ".github/workflows/") ||
		strings.HasPrefix(low, ".gitlab-ci") ||
		low == ".gitlab-ci.yml" ||
		strings.HasPrefix(low, ".circleci/") ||
		strings.HasPrefix(low, "buildkite/") ||
		strings.HasPrefix(low, ".buildkite/")
}

func isDependencyManifest(base string) bool {
	switch base {
	case "go.mod", "go.sum",
		"package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "npm-shrinkwrap.json",
		"cargo.toml", "cargo.lock",
		"requirements.txt", "pipfile", "pipfile.lock", "poetry.lock", "pyproject.toml",
		"gemfile", "gemfile.lock",
		"composer.json", "composer.lock",
		"mix.exs", "mix.lock",
		"pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle":
		return true
	}
	return false
}

func isWorkflowConfig(low, base string) bool {
	if base == "makefile" || base == "gnumakefile" || base == "justfile" {
		return true
	}
	if base == "herd.yaml" || strings.HasSuffix(low, "/herd.yaml") ||
		low == ".herd/herd.yaml" || strings.HasPrefix(low, ".herd/") && (strings.HasSuffix(low, ".yaml") || strings.HasSuffix(low, ".yml")) {
		// .herd/prompts are docs; exclude prompts/skills markdown handled elsewhere.
		if strings.Contains(low, "/prompts/") || strings.Contains(low, "/skills/") {
			return false
		}
		if strings.HasSuffix(low, ".md") {
			return false
		}
		return true
	}
	// Operational YAML at repo root often drives workflow.
	if (base == "docker-compose.yml" || base == "docker-compose.yaml") && !isInfrastructurePath(low, base) {
		return true
	}
	return false
}

func isGeneratedPath(low, base, ext string) bool {
	if strings.Contains(low, "/generated/") || strings.HasPrefix(low, "generated/") ||
		strings.Contains(low, "/vendor/") || strings.HasPrefix(low, "vendor/") ||
		strings.Contains(low, "/node_modules/") || strings.HasPrefix(low, "node_modules/") ||
		strings.Contains(low, "/dist/") || strings.HasPrefix(low, "dist/") {
		return true
	}
	if strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, "_gen.go") ||
		strings.HasSuffix(base, ".gen.go") || strings.HasSuffix(base, "_generated.go") ||
		strings.HasSuffix(base, ".generated.go") {
		return true
	}
	if strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") {
		return true
	}
	_ = ext
	return false
}

func isTestPath(low, base, ext string) bool {
	if strings.Contains(low, "/testdata/") || strings.HasPrefix(low, "testdata/") ||
		strings.Contains(low, "/__tests__/") || strings.Contains(low, "/fixtures/") {
		return true
	}
	// *_test.go, *.test.ts, *.spec.ts, etc.
	if strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.rs") ||
		strings.HasSuffix(base, "_test.py") || strings.HasSuffix(base, "_spec.rb") {
		return true
	}
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	// test/ or tests/ directory with source-like files only — still tests.
	if strings.HasPrefix(low, "test/") || strings.HasPrefix(low, "tests/") ||
		strings.Contains(low, "/test/") || strings.Contains(low, "/tests/") {
		return true
	}
	_ = ext
	return false
}

func isDocsPath(low, base, ext string) bool {
	switch ext {
	case ".md", ".mdx", ".rst", ".adoc", ".txt":
		// requirements.txt is a dependency manifest, already handled.
		if base == "requirements.txt" {
			return false
		}
		return true
	}
	if strings.HasPrefix(low, "docs/") || strings.Contains(low, "/docs/") {
		return true
	}
	switch base {
	case "readme", "readme.md", "license", "license.md", "licence", "licence.md",
		"changelog", "changelog.md", "authors", "authors.md",
		"contributing", "contributing.md", "code_of_conduct.md",
		"security.md", "copying", "notice":
		return true
	}
	if strings.HasPrefix(base, "readme.") {
		return true
	}
	// Prompt/skill markdown under .herd is documentation for agents.
	if strings.HasPrefix(low, ".herd/prompts/") || strings.HasPrefix(low, ".herd/skills/") ||
		strings.HasPrefix(low, "examples/prompts/") {
		return true
	}
	return false
}

func isBoundedConfig(base, ext string) bool {
	switch base {
	case ".editorconfig", ".gitignore", ".gitattributes", ".gitmodules",
		".prettierrc", ".prettierrc.js", ".prettierrc.json", ".prettierrc.yaml",
		".eslintrc", ".eslintrc.js", ".eslintrc.json", ".eslintrc.yml",
		".golangci.yml", ".golangci.yaml",
		"tsconfig.json", "jsconfig.json",
		".npmrc", ".nvmrc", ".node-version", ".ruby-version", ".python-version",
		".dockerignore", ".shellcheckrc":
		return true
	}
	if strings.HasPrefix(base, ".eslintrc.") || strings.HasPrefix(base, ".prettierrc.") {
		return true
	}
	_ = ext
	return false
}

func isMechanical(base string) bool {
	switch base {
	case ".gitkeep", ".keep", ".ds_store", "thumbs.db":
		return true
	}
	return false
}

func isProductionSource(low, ext string) bool {
	switch ext {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
		".rs", ".py", ".java", ".kt", ".rb", ".c", ".cc", ".cpp", ".h", ".hpp",
		".cs", ".swift", ".php", ".scala", ".ex", ".exs", ".erl",
		".vue", ".svelte", ".sol":
		return true
	}
	// Shell used as product automation is feature code.
	if ext == ".sh" || ext == ".bash" || ext == ".zsh" {
		return true
	}
	// Proto / OpenAPI often define public contracts → production-level.
	if ext == ".proto" || strings.HasSuffix(low, "openapi.yaml") || strings.HasSuffix(low, "openapi.yml") ||
		strings.HasSuffix(low, "openapi.json") || strings.HasSuffix(low, "swagger.yaml") {
		return true
	}
	return false
}
