package agentscope

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func goldenTrustedJobCallback(t *testing.T) TrustedJobCallbackRequest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureRoot, "trustedjob-callback.valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeTrustedJobCallbackStrict(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func trustedJobValidationContext(t *testing.T, request TrustedJobCallbackRequest) TrustedJobCallbackValidationContext {
	t.Helper()
	scopeDigest, err := Digest(request.Scope)
	if err != nil {
		t.Fatal(err)
	}
	return TrustedJobCallbackValidationContext{
		Scope:                       validationContext(t),
		ExpectedScopeDigest:         scopeDigest,
		ExpectedSubmissionDigest:    request.SubmissionDigest,
		ExpectedCandidateSHA:        request.Correlation.CandidateSHA,
		ExpectedCallbackSequence:    request.Correlation.CallbackSequence,
		ExpectedCallbackDestination: request.CallbackDestination,
		ExpectedIdempotencyKey:      request.IdempotencyKey,
		AllowedProviderModels: []ProviderModel{{
			Provider: request.Job.Provider,
			Model:    request.Job.EffectiveModel,
		}},
	}
}

func hasTrustedJobCode(receipt TrustedJobCallbackReceipt, code TrustedJobViolationCode) bool {
	for _, violation := range receipt.Violations {
		if violation.Code == code {
			return true
		}
	}
	return false
}

func TestTrustedJobCallbackGoldenContract(t *testing.T) {
	request := goldenTrustedJobCallback(t)
	receipt, err := (TrustedJobCallbackValidator{}).AcceptTrustedJobCallback(context.Background(), request, trustedJobValidationContext(t, request))
	if err != nil {
		t.Fatalf("trusted-job adapter validation: %v", err)
	}
	if !receipt.Accepted || receipt.Blocking || len(receipt.Violations) != 0 {
		t.Fatalf("valid trusted-job callback blocked: %+v", receipt)
	}
	if receipt.Correlation != request.Correlation || receipt.ScopeDigest != request.ScopeDigest || receipt.Provider != request.Job.Provider || receipt.EffectiveModel != request.Job.EffectiveModel {
		t.Fatalf("receipt failed to preserve callback correlation: %+v", receipt)
	}
	if len(receipt.Evidence) != len(request.Evidence) || receipt.Evidence[0] != request.Evidence[0] {
		t.Fatalf("receipt failed to preserve digest-bound evidence: %+v", receipt)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("receipt must serialize: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"accepted":true`)) || !bytes.Contains(encoded, []byte(`"blocking":false`)) || !bytes.Contains(encoded, []byte(`"violations":[]`)) {
		t.Fatalf("receipt lacks explicit admission fields: %s", encoded)
	}
}

func TestTrustedJobCallbackRejectionsFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		code   TrustedJobViolationCode
		mutate func(*TrustedJobCallbackRequest, *TrustedJobCallbackValidationContext)
	}{
		{
			name: "floating candidate ref",
			code: ViolationTrustedJobCandidate,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.Correlation.CandidateSHA = "refs/heads/main"
				request.Job.CandidateSHA = "refs/heads/main"
			},
		},
		{
			name: "scope path escape",
			code: ViolationTrustedJobPath,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.Scope.Spec.Paths.Writable = []string{"pkg/agentscope/../../outside"}
			},
		},
		{
			name: "master credential reference",
			code: ViolationTrustedJobCredential,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.Scope.Spec.Credentials[0].Ref = "master"
			},
		},
		{
			name: "wildcard credential scope",
			code: ViolationTrustedJobCredential,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.Scope.Spec.Credentials[0].Scopes = []string{"*"}
			},
		},
		{
			name: "unknown provider model pair",
			code: ViolationTrustedJobProviderModel,
			mutate: func(request *TrustedJobCallbackRequest, ctx *TrustedJobCallbackValidationContext) {
				request.Job.EffectiveModel = "unknown-model"
				request.Scope.Spec.Runtime.Model = "unknown-model"
				ctx.Scope.AllowedModels = []string{"unknown-model"}
			},
		},
		{
			name: "correlation identity mismatch",
			code: ViolationTrustedJobIdentity,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.Correlation.Task = "SPE-999"
			},
		},
		{
			name: "scope digest mismatch",
			code: ViolationTrustedJobDigest,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.ScopeDigest = "sha256:" + strings.Repeat("0", 64)
			},
		},
		{
			name: "candidate mismatch",
			code: ViolationTrustedJobCandidate,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.Job.CandidateSHA = strings.Repeat("5", 40)
			},
		},
		{
			name: "stale callback sequence",
			code: ViolationTrustedJobSequence,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.Correlation.CallbackSequence = 6
			},
		},
		{
			name: "missing evidence",
			code: ViolationTrustedJobEvidence,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.Evidence = nil
			},
		},
		{
			name: "stale idempotency key",
			code: ViolationTrustedJobReplay,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.IdempotencyKey = "idem-stale-SPE-558"
			},
		},
		{
			name: "duplicate callback key",
			code: ViolationTrustedJobReplay,
			mutate: func(request *TrustedJobCallbackRequest, ctx *TrustedJobCallbackValidationContext) {
				ctx.SeenKeys = []string{request.IdempotencyKey}
			},
		},
		{
			name: "submission digest mismatch",
			code: ViolationTrustedJobSubmission,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.SubmissionDigest = "sha256:" + strings.Repeat("0", 64)
			},
		},
		{
			name: "callback destination mismatch",
			code: ViolationTrustedJobSubmission,
			mutate: func(request *TrustedJobCallbackRequest, _ *TrustedJobCallbackValidationContext) {
				request.CallbackDestination = "other-coordinator"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := goldenTrustedJobCallback(t)
			ctx := trustedJobValidationContext(t, request)
			tc.mutate(&request, &ctx)
			receipt := ValidateTrustedJobCallback(request, ctx)
			if receipt.Accepted || !receipt.Blocking || !hasTrustedJobCode(receipt, tc.code) {
				t.Fatalf("unsafe callback was admitted: %+v", receipt)
			}
		})
	}
}

func TestAgentScopeRejectsMasterAndWildcardCredentials(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AgentScope, *ValidationContext)
	}{
		{
			name: "master reference remains forbidden when allowlisted",
			mutate: func(scope *AgentScope, ctx *ValidationContext) {
				scope.Spec.Credentials[0].Ref = "master"
				ctx.AllowedCredentialRefs = []string{"master"}
			},
		},
		{
			name: "wildcard scope remains forbidden when allowlisted",
			mutate: func(scope *AgentScope, ctx *ValidationContext) {
				scope.Spec.Credentials[0].Scopes = []string{"*"}
				ctx.PolicyCeiling.AllowedCredentialScopes = []string{"*"}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scope := goldenScope(t, "valid.json")
			ctx := validationContext(t)
			tc.mutate(&scope, &ctx)
			receipt := Validate(scope, ctx)
			if receipt.Valid || !hasCode(receipt, ViolationCredentialScope) {
				t.Fatalf("unsafe credential was not explicitly rejected: %+v", receipt)
			}
		})
	}
}

func TestDecodeTrustedJobCallbackStrictRejectsInlineAuthority(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureRoot, "trustedjob-callback.valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	request["manifest"] = "apiVersion: v1\nkind: Job"
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTrustedJobCallbackStrict(bytes.NewReader(encoded)); err == nil {
		t.Fatal("strict decoder accepted an inline manifest")
	}

	delete(request, "manifest")
	scope := request["scope"].(map[string]any)
	spec := scope["spec"].(map[string]any)
	spec["manifest"] = "apiVersion: v1\nkind: Job"
	encoded, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTrustedJobCallbackStrict(bytes.NewReader(encoded)); err == nil {
		t.Fatal("strict decoder accepted an inline scope manifest")
	}

	delete(spec, "manifest")
	job := request["job"].(map[string]any)
	job["credential"] = "sk-live-not-a-reference"
	encoded, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTrustedJobCallbackStrict(bytes.NewReader(encoded)); err == nil {
		t.Fatal("strict decoder accepted raw credential material")
	}
}

func TestTrustedJobCallbackCanonicalGolden(t *testing.T) {
	request := goldenTrustedJobCallback(t)
	canonical, err := CanonicalTrustedJobCallbackJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	expectedCanonical, err := os.ReadFile(filepath.Join(fixtureRoot, "trustedjob-callback.valid.canonical.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != strings.TrimSuffix(string(expectedCanonical), "\n") {
		t.Fatalf("canonical callback golden mismatch\nwant: %s\n got: %s", expectedCanonical, canonical)
	}
	digest, err := TrustedJobCallbackDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	expectedDigest, err := os.ReadFile(filepath.Join(fixtureRoot, "trustedjob-callback.valid.canonical.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if digest != "sha256:"+strings.TrimSpace(string(expectedDigest)) {
		t.Fatalf("callback digest mismatch: got %s", digest)
	}
}

func TestTrustedJobCallbackReceiptSchemaGolden(t *testing.T) {
	schemaBytes, err := os.ReadFile(filepath.Join(fixtureRoot, "trustedjob-callback-receipt.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	schemaDir, err := filepath.Abs(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseRegexpEngine(ecmascriptCompile)
	schema, err := compiler.Compile("file://" + filepath.ToSlash(filepath.Join(schemaDir, "trustedjob-callback-receipt.schema.json")))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDoc map[string]any
	if err := json.Unmarshal(schemaBytes, &schemaDoc); err != nil {
		t.Fatal(err)
	}
	if schemaDoc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("unexpected schema dialect: %v", schemaDoc["$schema"])
	}
	golden := decodeFixtureAny(t, "trustedjob-callback-receipt.valid.json")
	if err := schema.Validate(golden); err != nil {
		t.Fatalf("valid callback receipt fixture must satisfy schema: %v", err)
	}

	request := goldenTrustedJobCallback(t)
	receipt := ValidateTrustedJobCallback(request, trustedJobValidationContext(t, request))
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var actual any
	if err := json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, golden) {
		t.Fatalf("receipt golden mismatch\nwant: %#v\n got: %#v", golden, actual)
	}
	if err := schema.Validate(actual); err != nil {
		t.Fatalf("generated callback receipt must satisfy schema: %v", err)
	}

	invalid, ok := decodeFixtureAny(t, "trustedjob-callback-receipt.valid.json").(map[string]any)
	if !ok {
		t.Fatal("trusted-job callback receipt fixture root must be an object")
	}
	invalid["violations"] = []any{map[string]any{"code": "unknown", "field": "x", "message": "x"}}
	if err := schema.Validate(invalid); err == nil {
		t.Fatal("callback receipt schema accepted an untyped violation")
	}
}

func TestTrustedJobCallbackSchemaRejectsInlineAuthority(t *testing.T) {
	callbackSchemaBytes, err := os.ReadFile(filepath.Join(fixtureRoot, "trustedjob-callback.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var callbackSchemaDoc map[string]any
	if err := json.Unmarshal(callbackSchemaBytes, &callbackSchemaDoc); err != nil {
		t.Fatal(err)
	}
	if callbackSchemaDoc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("unexpected schema dialect: %v", callbackSchemaDoc["$schema"])
	}
	schemaDir, err := filepath.Abs(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	callbackURL := "file://" + filepath.ToSlash(filepath.Join(schemaDir, "trustedjob-callback.schema.json"))
	compiler := jsonschema.NewCompiler()
	compiler.UseRegexpEngine(ecmascriptCompile)
	schema, err := compiler.Compile(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	valid := decodeFixtureAny(t, "trustedjob-callback.valid.json")
	if err := schema.Validate(valid); err != nil {
		t.Fatalf("valid callback fixture must satisfy schema: %v", err)
	}

	invalid, ok := decodeFixtureAny(t, "trustedjob-callback.valid.json").(map[string]any)
	if !ok {
		t.Fatal("trusted-job callback fixture root must be an object")
	}
	invalid["manifest"] = "apiVersion: v1\nkind: Job"
	if err := schema.Validate(invalid); err == nil {
		t.Fatal("callback schema accepted an inline manifest")
	}

	delete(invalid, "manifest")
	scope := invalid["scope"].(map[string]any)
	spec := scope["spec"].(map[string]any)
	spec["manifest"] = "apiVersion: v1\nkind: Job"
	if err := schema.Validate(invalid); err == nil {
		t.Fatal("callback schema accepted an inline scope manifest")
	}
}
