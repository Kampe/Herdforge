package agentscope

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const fixtureRoot = "../../contracts/agentscope/v1alpha1"

func goldenScope(t *testing.T, name string) AgentScope {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureRoot, name))
	if err != nil {
		t.Fatal(err)
	}
	scope, err := DecodeStrict(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func validationContext(t *testing.T) ValidationContext {
	t.Helper()
	now, err := time.Parse(time.RFC3339, "2026-08-03T12:05:00Z")
	if err != nil {
		t.Fatal(err)
	}
	return ValidationContext{
		RegisteredRepository: "github.com/Kampe/Herdforge",
		OwnedWorktree: WorktreeIdentity{
			Repository:     "github.com/Kampe/Herdforge",
			Identity:       "worktree-SPE-566",
			Path:           ".worktrees/SPE-566-agentscope-contract",
			RepositoryRoot: ".",
			BaseSHA:        "1111111111111111111111111111111111111111",
			HeadSHA:        "2222222222222222222222222222222222222222",
		},
		AllowedCommandProfiles: []string{"go-test", "git-commit"},
		AllowedCredentialRefs:  []string{"pi-openai-SPE-566"},
		AllowedProviders:       []string{"openai"},
		AllowedModels:          []string{"gpt-5.6-luna"},
		PolicyCeiling: PolicyCeiling{
			AllowedNetworkHosts:     []string{"api.openai.com"},
			AllowedGitActions:       []GitAction{GitStatus, GitDiff, GitAdd, GitCommit, GitPush, GitCreatePR},
			AllowedCredentialScopes: []string{"inference"},
			AllowedEvidenceKinds:    []EvidenceKind{EvidenceTest, EvidenceLint, EvidenceDiff, EvidenceReceipt},
			EvidencePrefix:          ".herd/evidence/SPE-566",
			MaxScopeTTLSeconds:      3600,
			MaxCredentialTTLSeconds: 3600,
			MaxDeadlineSeconds:      2400,
			MaxTurns:                32,
			MaxTokens:               150000,
			MaxStallTimeoutSeconds:  300,
			MaxLoopWindow:           16,
			MaxLoopRepeatThreshold:  4,
			MaxPaidUSD:              9,
			MaxEvidenceBytes:        2 * 1024 * 1024,
		},
		Now: now,
	}
}

func hasCode(receipt ViolationReceipt, code ViolationCode) bool {
	for _, violation := range receipt.Violations {
		if violation.Code == code {
			return true
		}
	}
	return false
}

func TestGoldenCompatibility(t *testing.T) {
	schemaData, err := os.ReadFile(filepath.Join(fixtureRoot, "agentscope.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		t.Fatalf("schema must be valid JSON: %v", err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("unexpected schema dialect: %v", schema["$schema"])
	}

	valid := Validate(goldenScope(t, "valid.json"), validationContext(t))
	if !valid.Valid || valid.Blocking || len(valid.Violations) != 0 {
		t.Fatalf("valid golden blocked: %+v", valid)
	}
	if !strings.HasPrefix(valid.ScopeDigest, "sha256:") || len(valid.ScopeDigest) != len("sha256:")+64 {
		t.Fatalf("invalid canonical digest %q", valid.ScopeDigest)
	}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("valid receipt must serialize: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"blocking":false`)) || !bytes.Contains(encoded, []byte(`"violations":[]`)) {
		t.Fatalf("valid receipt lacks explicit lifecycle fields: %s", encoded)
	}

	invalid := Validate(goldenScope(t, "invalid-budget.json"), validationContext(t))
	if invalid.Valid || !invalid.Blocking || !hasCode(invalid, ViolationBudgetLimit) {
		t.Fatalf("invalid golden was not blocked by budget code: %+v", invalid)
	}
	if _, err := json.Marshal(invalid); err != nil {
		t.Fatalf("invalid receipt must serialize: %v", err)
	}
}

func TestValidationCategoriesFailClosed(t *testing.T) {
	ctx := validationContext(t)
	baseline := goldenScope(t, "valid.json")
	if receipt := Validate(baseline, ctx); !receipt.Valid {
		t.Fatalf("test baseline must be valid: %+v", receipt)
	}

	tests := []struct {
		name   string
		code   ViolationCode
		mutate func(*AgentScope)
	}{
		{"identity schema", ViolationIdentitySchema, func(s *AgentScope) { s.APIVersion = "herdforge.dev/v2" }},
		{"metadata run id", ViolationIdentitySchema, func(s *AgentScope) { s.Metadata.RunID = "" }},
		{"metadata phase id", ViolationIdentitySchema, func(s *AgentScope) { s.Metadata.PhaseID = "bad phase" }},
		{"repository binding", ViolationRepositoryBinding, func(s *AgentScope) { s.Spec.Repository.Identity = "github.com/other/repo" }},
		{"immutable base SHA", ViolationImmutableRevision, func(s *AgentScope) { s.Spec.Repository.BaseSHA = "main" }},
		{"exact owned revision", ViolationWorktreeOwnership, func(s *AgentScope) { s.Spec.Repository.HeadSHA = strings.Repeat("3", 40) }},
		{"worktree identity", ViolationWorktreeOwnership, func(s *AgentScope) { s.Spec.Worktree.Identity = "worktree-other" }},
		{"shared worktree", ViolationWorktreeState, func(s *AgentScope) { s.Spec.Worktree.Shared = true }},
		{"immutable worktree", ViolationWorktreeState, func(s *AgentScope) { s.Spec.Worktree.Mutable = false }},
		{"repository root checkout", ViolationWorktreeState, func(s *AgentScope) { s.Spec.Worktree.Path = "." }},
		{"parent path traversal", ViolationPathSafety, func(s *AgentScope) { s.Spec.Paths.Writable[0] = "../outside" }},
		{"git metadata path", ViolationPathSafety, func(s *AgentScope) { s.Spec.Paths.Writable[0] = ".git/config" }},
		{"unknown command profile", ViolationCommandProfile, func(s *AgentScope) { s.Spec.CommandProfiles[0] = "shell-inline" }},
		{"implicit network mode", ViolationNetworkPolicy, func(s *AgentScope) { s.Spec.Network.Mode = "" }},
		{"wildcard network host", ViolationNetworkPolicy, func(s *AgentScope) { s.Spec.Network.AllowedHosts[0] = "*.example.com" }},
		{"network deny with hosts", ViolationNetworkPolicy, func(s *AgentScope) { s.Spec.Network.Mode = NetworkDeny }},
		{"git merge authority", ViolationGitAuthority, func(s *AgentScope) { s.Spec.Git.Actions[0] = GitAction("merge") }},
		{"git deploy authority", ViolationGitAuthority, func(s *AgentScope) { s.Spec.Git.Actions[0] = GitAction("deploy") }},
		{"raw credential ref", ViolationCredentialScope, func(s *AgentScope) { s.Spec.Credentials[0].Ref = "sk-secret-material" }},
		{"master credential scope", ViolationCredentialScope, func(s *AgentScope) { s.Spec.Credentials[0].Scopes[0] = "master" }},
		{"credential provider allowlist", ViolationCredentialScope, func(s *AgentScope) { s.Spec.Credentials[0].Provider = "other" }},
		{"expired credential", ViolationExpiry, func(s *AgentScope) { s.Spec.Credentials[0].ExpiresAt = "2026-08-03T12:00:00Z" }},
		{"expired scope", ViolationExpiry, func(s *AgentScope) { s.Metadata.ExpiresAt = "2026-08-03T12:00:00Z" }},
		{"provider allowlist", ViolationRuntimeAllowlist, func(s *AgentScope) { s.Spec.Runtime.Provider = "other" }},
		{"model allowlist", ViolationRuntimeAllowlist, func(s *AgentScope) { s.Spec.Runtime.Model = "other-model" }},
		{"deadline positive", ViolationRuntimeBounds, func(s *AgentScope) { s.Spec.Runtime.DeadlineSeconds = 0 }},
		{"turn positive", ViolationRuntimeBounds, func(s *AgentScope) { s.Spec.Runtime.MaxTurns = 0 }},
		{"token positive", ViolationRuntimeBounds, func(s *AgentScope) { s.Spec.Runtime.MaxTokens = 0 }},
		{"stall positive", ViolationRuntimeBounds, func(s *AgentScope) { s.Spec.Runtime.StallTimeoutSeconds = 0 }},
		{"loop window positive", ViolationRuntimeBounds, func(s *AgentScope) { s.Spec.Runtime.LoopWindow = 0 }},
		{"loop threshold bounded", ViolationRuntimeBounds, func(s *AgentScope) { s.Spec.Runtime.LoopRepeatThreshold = 13 }},
		{"budget positive", ViolationBudgetLimit, func(s *AgentScope) { s.Spec.Runtime.MaxPaidUSD = 0 }},
		{"budget context ceiling", ViolationBudgetLimit, func(s *AgentScope) { s.Spec.Runtime.MaxPaidUSD = 9.5 }},
		{"budget hard ceiling", ViolationBudgetLimit, func(s *AgentScope) { s.Spec.Runtime.MaxPaidUSD = 12 }},
		{"evidence kind", ViolationEvidencePolicy, func(s *AgentScope) { s.Spec.Evidence.Kinds[0] = "archive" }},
		{"evidence prefix", ViolationEvidencePolicy, func(s *AgentScope) { s.Spec.Evidence.Prefix = ".herd/evidence/other" }},
		{"evidence size", ViolationEvidencePolicy, func(s *AgentScope) { s.Spec.Evidence.MaxBytes = 3 * 1024 * 1024 }},
		{"inline kubernetes", ViolationForbiddenCapability, func(s *AgentScope) { s.Spec.Grants.Kubernetes = true }},
		{"unrestricted shell", ViolationForbiddenCapability, func(s *AgentScope) { s.Spec.Grants.UnrestrictedShell = true }},
		{"extension grant", ViolationForbiddenCapability, func(s *AgentScope) { s.Spec.Grants.Extensions = []string{"unsafe-extension"} }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scope := cloneScope(t, baseline)
			test.mutate(&scope)
			receipt := Validate(scope, ctx)
			if receipt.Valid || !receipt.Blocking {
				t.Fatalf("mutated scope was admitted: %+v", receipt)
			}
			if !hasCode(receipt, test.code) {
				t.Fatalf("missing violation %q in %+v", test.code, receipt.Violations)
			}
		})
	}
}

type ecmascriptRegexp regexp2.Regexp

func (re *ecmascriptRegexp) MatchString(s string) bool {
	matched, err := (*regexp2.Regexp)(re).MatchString(s)
	return err == nil && matched
}

func (re *ecmascriptRegexp) String() string {
	return (*regexp2.Regexp)(re).String()
}

func ecmascriptCompile(s string) (jsonschema.Regexp, error) {
	re, err := regexp2.Compile(s, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	return (*ecmascriptRegexp)(re), nil
}

func compileAgentScopeSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	schemaBytes, err := os.ReadFile(filepath.Join(fixtureRoot, "agentscope.schema.json"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schemaDoc any
	if err := json.Unmarshal(schemaBytes, &schemaDoc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseRegexpEngine(ecmascriptCompile)
	if err := compiler.AddResource("agentscope.schema.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := compiler.Compile("agentscope.schema.json")
	if err != nil {
		t.Fatalf("compile Draft 2020-12 schema: %v", err)
	}
	return schema
}

func decodeFixtureAny(t *testing.T, name string) any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureRoot, name))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("fixture %s is not valid JSON: %v", name, err)
	}
	return doc
}

// TestJSONSchemaFixtureConformance proves the checked-in fixtures agree with
// the Draft 2020-12 schema via the santhosh-tekuri/jsonschema validator. The
// negative branch and the in-test regressions prove the validator is not
// rubber-stamping: each mutation removes a required field or introduces a
// value the schema must reject, and the test fails closed if the validator
// accepts it.
func TestJSONSchemaFixtureConformance(t *testing.T) {
	schema := compileAgentScopeSchema(t)

	validDoc := decodeFixtureAny(t, "valid.json")
	if err := schema.Validate(validDoc); err != nil {
		t.Fatalf("valid.json must satisfy the schema: %v", err)
	}

	invalidDoc := decodeFixtureAny(t, "invalid-budget.json")
	if err := schema.Validate(invalidDoc); err == nil {
		t.Fatal("invalid-budget.json must fail the schema (maxPaidUsd=12 exceeds maximum 10), but the validator accepted it")
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			"missing required runId",
			func(doc map[string]any) {
				meta := doc["metadata"].(map[string]any)
				delete(meta, "runId")
			},
		},
		{
			"missing required phaseId",
			func(doc map[string]any) {
				meta := doc["metadata"].(map[string]any)
				delete(meta, "phaseId")
			},
		},
		{
			"git merge action",
			func(doc map[string]any) {
				spec := doc["spec"].(map[string]any)
				git := spec["git"].(map[string]any)
				git["actions"] = []any{"status", "merge"}
			},
		},
		{
			"git deploy action",
			func(doc map[string]any) {
				spec := doc["spec"].(map[string]any)
				git := spec["git"].(map[string]any)
				git["actions"] = []any{"deploy"}
			},
		},
		{
			"inline kubernetes grant",
			func(doc map[string]any) {
				spec := doc["spec"].(map[string]any)
				grants := spec["grants"].(map[string]any)
				grants["kubernetes"] = true
			},
		},
		{
			"unknown top-level field",
			func(doc map[string]any) {
				doc["rawToken"] = "secret"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, ok := decodeFixtureAny(t, "valid.json").(map[string]any)
			if !ok {
				t.Fatal("valid.json root must be an object")
			}
			tc.mutate(base)
			if err := schema.Validate(base); err == nil {
				t.Fatalf("schema accepted a regression: %s", tc.name)
			}
		})
	}
}

func TestInvalidValidationContextBlocks(t *testing.T) {
	ctx := validationContext(t)
	ctx.PolicyCeiling.MaxPaidUSD = 11
	receipt := Validate(goldenScope(t, "valid.json"), ctx)
	if receipt.Valid || !receipt.Blocking || !hasCode(receipt, ViolationContext) {
		t.Fatalf("unsafe context must block: %+v", receipt)
	}
}

func TestDecodeStrictRejectsUntypedAuthorityAndCredentialMaterial(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureRoot, "valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"raw credential":    bytes.Replace(data, []byte(`"ref": "pi-openai-SPE-566"`), []byte(`"rawToken": "secret", "ref": "pi-openai-SPE-566"`), 1),
		"master credential": bytes.Replace(data, []byte(`"ref": "pi-openai-SPE-566"`), []byte(`"masterCredential": true, "ref": "pi-openai-SPE-566"`), 1),
		"inline command":    bytes.Replace(data, []byte(`"commandProfiles":`), []byte(`"command": "kubectl apply -f -", "commandProfiles":`), 1),
		"trailing object":   append(append([]byte(nil), data...), []byte(` {}`)...),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeStrict(bytes.NewReader(payload)); err == nil {
				t.Fatal("strict decoder accepted untyped authority or trailing JSON")
			}
		})
	}
}

func TestCanonicalDigestDeterministicAndOrderIndependent(t *testing.T) {
	original := goldenScope(t, "valid.json")
	first, err := Digest(original)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Digest(original)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same scope produced different digests: %q != %q", first, second)
	}

	reordered := cloneScope(t, original)
	slices.Reverse(reordered.Spec.Paths.Readable)
	slices.Reverse(reordered.Spec.Paths.Writable)
	slices.Reverse(reordered.Spec.CommandProfiles)
	slices.Reverse(reordered.Spec.Git.Actions)
	slices.Reverse(reordered.Spec.Evidence.Kinds)
	orderedDigest, err := Digest(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if first != orderedDigest {
		t.Fatalf("set ordering changed digest: %q != %q", first, orderedDigest)
	}
	if original.Spec.Paths.Readable[0] != "go.mod" {
		t.Fatal("canonicalization mutated caller-owned scope")
	}

	changed := cloneScope(t, original)
	changed.Spec.Runtime.MaxTurns++
	changedDigest, err := Digest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if first == changedDigest {
		t.Fatal("policy mutation did not change digest")
	}
}

func TestCanonicalDigestMatchesGoldenRFC8785(t *testing.T) {
	scope := goldenScope(t, "valid.json")
	hexDigest, canonical, err := CanonicalDigestHex(scope)
	if err != nil {
		t.Fatalf("canonical digest failed: %v", err)
	}

	goldenCanonical, err := os.ReadFile(filepath.Join(fixtureRoot, "valid.canonical.json"))
	if err != nil {
		t.Fatalf("missing checked-in golden canonical fixture: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(goldenCanonical), canonical) {
		t.Fatalf("canonical bytes diverged from golden RFC 8785 fixture.\n got: %s\nwant: %s", canonical, bytes.TrimSpace(goldenCanonical))
	}

	goldenHexBytes, err := os.ReadFile(filepath.Join(fixtureRoot, "valid.canonical.sha256"))
	if err != nil {
		t.Fatalf("missing checked-in golden SHA-256 fixture: %v", err)
	}
	goldenHex := strings.TrimSpace(string(goldenHexBytes))
	if hexDigest != goldenHex {
		t.Fatalf("canonical SHA-256 diverged from golden: got %q want %q", hexDigest, goldenHex)
	}

	if bytes.Contains(canonical, []byte("\\/")) {
		t.Fatalf("canonical bytes escaped solidus, violating RFC 8785 §3.2.4: %s", canonical)
	}
	if bytes.ContainsAny(canonical, " \t\n\r") {
		t.Fatalf("canonical bytes contain insignificant whitespace, violating RFC 8785 §3.2.5: %s", canonical)
	}
	var sanity map[string]any
	if err := json.Unmarshal(canonical, &sanity); err != nil {
		t.Fatalf("canonical bytes are not valid JSON: %v\n%s", err, canonical)
	}
	meta, ok := sanity["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("canonical bytes lost metadata: %s", canonical)
	}
	keys := nonNilKeys(sanity)
	if keys[0] != "apiVersion" {
		t.Fatalf("top-level keys are not RFC 8785 UTF-16 sorted: first key %q", keys[0])
	}
	if _, ok := meta["runId"]; !ok {
		t.Fatalf("canonical bytes dropped metadata.runId: %s", canonical)
	}
	if _, ok := meta["phaseId"]; !ok {
		t.Fatalf("canonical bytes dropped metadata.phaseId: %s", canonical)
	}
	if _, ok := sanity["spec"].(map[string]any)["git"].(map[string]any)["actions"]; !ok {
		t.Fatalf("canonical bytes dropped git.actions: %s", canonical)
	}
}

func nonNilKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestDigestRejectsNonJSONNumber(t *testing.T) {
	scope := goldenScope(t, "valid.json")
	scope.Spec.Runtime.MaxPaidUSD = math.NaN()
	if _, err := Digest(scope); err == nil {
		t.Fatal("NaN must not have a canonical digest")
	}
	receipt := Validate(scope, validationContext(t))
	if receipt.Valid || !receipt.Blocking || !hasCode(receipt, ViolationBudgetLimit) || !hasCode(receipt, ViolationIdentitySchema) {
		t.Fatalf("non-JSON budget must block with typed evidence: %+v", receipt)
	}
}

func cloneScope(t *testing.T, scope AgentScope) AgentScope {
	t.Helper()
	data, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := DecodeStrict(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return cloned
}
