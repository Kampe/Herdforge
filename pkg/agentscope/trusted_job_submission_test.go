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

func goldenTrustedJobSubmission(t *testing.T) TrustedJobSubmission {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureRoot, "trustedjob-submission.valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	submission, err := DecodeTrustedJobSubmissionStrict(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return submission
}

func submissionValidationContext(t *testing.T, submission TrustedJobSubmission) TrustedJobSubmissionValidationContext {
	t.Helper()
	return TrustedJobSubmissionValidationContext{
		Scope:                       validationContext(t),
		ExpectedScopeDigest:         submission.ScopeDigest,
		ExpectedCandidateSHA:        submission.Correlation.CandidateSHA,
		ExpectedCallbackDestination: submission.CallbackDestination,
		AllowedProviderModels: []ProviderModel{{
			Provider: submission.Provider,
			Model:    submission.Model,
		}},
	}
}

func TestTrustedJobSubmissionGoldenAdmission(t *testing.T) {
	submission := goldenTrustedJobSubmission(t)
	receipt, err := (TrustedJobSubmissionValidator{}).AdmitTrustedJobSubmission(context.Background(), submission, submissionValidationContext(t, submission))
	if err != nil {
		t.Fatalf("submission adapter admission: %v", err)
	}
	if !receipt.Admitted || receipt.Blocking || len(receipt.Violations) != 0 {
		t.Fatalf("valid submission rejected: %+v", receipt)
	}
	if receipt.Status != JobStatusAdmitted {
		t.Fatalf("admitted submission must carry admitted status: %s", receipt.Status)
	}
	if receipt.Correlation != submission.Correlation || receipt.Job != submission.Job || receipt.Workspace != submission.Workspace {
		t.Fatalf("receipt failed to preserve submission identity: %+v", receipt)
	}
	if receipt.Provider != submission.Provider || receipt.Model != submission.Model {
		t.Fatalf("receipt failed to preserve provider/model: %+v", receipt)
	}
	if receipt.CallbackDestination != submission.CallbackDestination || receipt.IdempotencyKey != submission.IdempotencyKey {
		t.Fatalf("receipt failed to preserve callback binding: %+v", receipt)
	}
	if !strings.HasPrefix(receipt.SubmissionDigest, "sha256:") || len(receipt.SubmissionDigest) != len("sha256:")+64 {
		t.Fatalf("receipt lacks canonical submission digest: %q", receipt.SubmissionDigest)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("receipt must serialize: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"admitted":true`)) || !bytes.Contains(encoded, []byte(`"blocking":false`)) || !bytes.Contains(encoded, []byte(`"status":"admitted"`)) {
		t.Fatalf("receipt lacks explicit admission fields: %s", encoded)
	}
}

func TestTrustedJobSubmissionCanonicalGolden(t *testing.T) {
	submission := goldenTrustedJobSubmission(t)
	canonical, err := CanonicalTrustedJobSubmissionJSON(submission)
	if err != nil {
		t.Fatal(err)
	}
	expectedCanonical, err := os.ReadFile(filepath.Join(fixtureRoot, "trustedjob-submission.valid.canonical.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != strings.TrimSuffix(string(expectedCanonical), "\n") {
		t.Fatalf("canonical submission golden mismatch\nwant: %s\n got: %s", expectedCanonical, canonical)
	}
	digest, err := TrustedJobSubmissionDigest(submission)
	if err != nil {
		t.Fatal(err)
	}
	expectedDigest, err := os.ReadFile(filepath.Join(fixtureRoot, "trustedjob-submission.valid.canonical.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if digest != "sha256:"+strings.TrimSpace(string(expectedDigest)) {
		t.Fatalf("submission digest mismatch: got %s", digest)
	}

	// Determinism and order-independence: reordering set-like inputs must not
	// change the canonical digest, and caller-owned data must not be mutated.
	reordered := submission
	reordered.VirtualCredentials[0].Scopes = []string{"inference"}
	reorderedDigest, err := TrustedJobSubmissionDigest(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if digest != reorderedDigest {
		t.Fatalf("submission digest changed without a policy mutation: %q != %q", digest, reorderedDigest)
	}
	mutated := submission
	mutated.IdempotencyKey = "idem-SPE-558-2"
	mutatedDigest, err := TrustedJobSubmissionDigest(mutated)
	if err != nil {
		t.Fatal(err)
	}
	if digest == mutatedDigest {
		t.Fatal("idempotency key mutation did not change submission digest")
	}
	if submission.VirtualCredentials[0].Ref != "pi-openai-SPE-566" {
		t.Fatal("canonicalization mutated caller-owned submission")
	}
}

func TestTrustedJobSubmissionReceiptSchemaGolden(t *testing.T) {
	schemaDir, err := filepath.Abs(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseRegexpEngine(ecmascriptCompile)
	schema, err := compiler.Compile("file://" + filepath.ToSlash(filepath.Join(schemaDir, "trustedjob-submission-receipt.schema.json")))
	if err != nil {
		t.Fatal(err)
	}
	golden := decodeFixtureAny(t, "trustedjob-submission-receipt.valid.json")
	if err := schema.Validate(golden); err != nil {
		t.Fatalf("valid submission receipt fixture must satisfy schema: %v", err)
	}

	submission := goldenTrustedJobSubmission(t)
	receipt := ValidateTrustedJobSubmission(submission, submissionValidationContext(t, submission))
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var actual any
	if err := json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, golden) {
		t.Fatalf("submission receipt golden mismatch\nwant: %#v\n got: %#v", golden, actual)
	}
	if err := schema.Validate(actual); err != nil {
		t.Fatalf("generated submission receipt must satisfy schema: %v", err)
	}

	invalid, ok := decodeFixtureAny(t, "trustedjob-submission-receipt.valid.json").(map[string]any)
	if !ok {
		t.Fatal("submission receipt fixture root must be an object")
	}
	invalid["violations"] = []any{map[string]any{"code": "unknown", "field": "x", "message": "x"}}
	if err := schema.Validate(invalid); err == nil {
		t.Fatal("submission receipt schema accepted an untyped violation")
	}
	invalid2, _ := decodeFixtureAny(t, "trustedjob-submission-receipt.valid.json").(map[string]any)
	invalid2["status"] = "running"
	if err := schema.Validate(invalid2); err == nil {
		t.Fatal("submission receipt schema accepted an execution status; execution is out of scope")
	}
}

func TestTrustedJobSubmissionSchemaGolden(t *testing.T) {
	schemaDir, err := filepath.Abs(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseRegexpEngine(ecmascriptCompile)
	schema, err := compiler.Compile("file://" + filepath.ToSlash(filepath.Join(schemaDir, "trustedjob-submission.schema.json")))
	if err != nil {
		t.Fatal(err)
	}
	valid := decodeFixtureAny(t, "trustedjob-submission.valid.json")
	if err := schema.Validate(valid); err != nil {
		t.Fatalf("valid submission fixture must satisfy schema: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			"inline manifest",
			func(doc map[string]any) { doc["manifest"] = "apiVersion: v1\nkind: Job" },
		},
		{
			"inline trusted config yaml",
			func(doc map[string]any) { doc["trustedConfig"].(map[string]any)["yaml"] = "steps: []" },
		},
		{
			"raw virtual credential",
			func(doc map[string]any) {
				doc["virtualCredentials"].([]any)[0].(map[string]any)["secret"] = "sk-live-material"
			},
		},
		{
			"floating config ref",
			func(doc map[string]any) {
				doc["trustedConfig"].(map[string]any)["commitSha"] = "refs/heads/main"
			},
		},
		{
			"config path escape",
			func(doc map[string]any) {
				doc["trustedConfig"].(map[string]any)["path"] = "../outside/config.yaml"
			},
		},
		{
			"master credential ref",
			func(doc map[string]any) {
				doc["virtualCredentials"].([]any)[0].(map[string]any)["ref"] = "master"
			},
		},
		{
			"wildcard credential scope",
			func(doc map[string]any) {
				doc["virtualCredentials"].([]any)[0].(map[string]any)["scopes"] = []any{"*"}
			},
		},
		{
			"missing required idempotency key",
			func(doc map[string]any) { delete(doc, "idempotencyKey") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, ok := decodeFixtureAny(t, "trustedjob-submission.valid.json").(map[string]any)
			if !ok {
				t.Fatal("submission fixture root must be an object")
			}
			tc.mutate(base)
			if err := schema.Validate(base); err == nil {
				t.Fatalf("submission schema accepted a regression: %s", tc.name)
			}
		})
	}
}

func TestDecodeTrustedJobSubmissionStrictRejectsInlineAuthority(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureRoot, "trustedjob-submission.valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	var submission map[string]any
	if err := json.Unmarshal(data, &submission); err != nil {
		t.Fatal(err)
	}
	submission["manifest"] = "apiVersion: v1\nkind: Job"
	encoded, err := json.Marshal(submission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTrustedJobSubmissionStrict(bytes.NewReader(encoded)); err == nil {
		t.Fatal("strict decoder accepted an inline manifest")
	}

	delete(submission, "manifest")
	cfg := submission["trustedConfig"].(map[string]any)
	cfg["yaml"] = "steps: []"
	encoded, err = json.Marshal(submission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTrustedJobSubmissionStrict(bytes.NewReader(encoded)); err == nil {
		t.Fatal("strict decoder accepted inline trusted configuration YAML")
	}

	delete(cfg, "yaml")
	vc := submission["virtualCredentials"].([]any)[0].(map[string]any)
	vc["secret"] = "sk-live-material"
	encoded, err = json.Marshal(submission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTrustedJobSubmissionStrict(bytes.NewReader(encoded)); err == nil {
		t.Fatal("strict decoder accepted raw credential material")
	}
}

func TestTrustedJobSubmissionRejectionsFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		code   TrustedJobViolationCode
		mutate func(*TrustedJobSubmission, *TrustedJobSubmissionValidationContext)
	}{
		{
			name: "untrusted config repository",
			code: ViolationTrustedJobConfig,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.TrustedConfig.Repository = "github.com/other/repo"
			},
		},
		{
			name: "floating config commit ref",
			code: ViolationTrustedJobConfig,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.TrustedConfig.CommitSHA = "refs/heads/main"
			},
		},
		{
			name: "config path escape",
			code: ViolationTrustedJobConfig,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.TrustedConfig.Path = "../outside/config.yaml"
			},
		},
		{
			name: "config git path",
			code: ViolationTrustedJobConfig,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.TrustedConfig.Path = ".git/config"
			},
		},
		{
			name: "malformed content digest",
			code: ViolationTrustedJobConfig,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.TrustedConfig.ContentDigest = "sha256:zzz"
			},
		},
		{
			name: "repository identity mismatch",
			code: ViolationTrustedJobIdentity,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.Repository.Identity = "github.com/other/repo"
			},
		},
		{
			name: "workspace path mismatch",
			code: ViolationTrustedJobWorkspace,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.Workspace.Path = ".worktrees/other"
			},
		},
		{
			name: "workspace head sha mismatch",
			code: ViolationTrustedJobWorkspace,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.Workspace.HeadSHA = strings.Repeat("9", 40)
			},
		},
		{
			name: "job identity mismatch",
			code: ViolationTrustedJobIdentity,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.Job.RunID = "run-other"
			},
		},
		{
			name: "candidate floating ref",
			code: ViolationTrustedJobCandidate,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.Correlation.CandidateSHA = "refs/heads/main"
				sub.Job.CandidateSHA = "refs/heads/main"
			},
		},
		{
			name: "candidate mismatch",
			code: ViolationTrustedJobCandidate,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.Job.CandidateSHA = strings.Repeat("7", 40)
			},
		},
		{
			name: "unknown provider model pair",
			code: ViolationTrustedJobProviderModel,
			mutate: func(sub *TrustedJobSubmission, ctx *TrustedJobSubmissionValidationContext) {
				ctx.AllowedProviderModels = []ProviderModel{{Provider: "other", Model: "other"}}
			},
		},
		{
			name: "virtual credential not in scope",
			code: ViolationTrustedJobVirtualCredential,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.VirtualCredentials[0].Ref = "pi-openai-SPE-999"
			},
		},
		{
			name: "raw virtual credential",
			code: ViolationTrustedJobVirtualCredential,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.VirtualCredentials[0].Ref = "sk-live-material"
			},
		},
		{
			name: "master virtual credential",
			code: ViolationTrustedJobVirtualCredential,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.VirtualCredentials[0].Ref = "master"
			},
		},
		{
			name: "wildcard virtual credential scope",
			code: ViolationTrustedJobVirtualCredential,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.VirtualCredentials[0].Scopes = []string{"*"}
			},
		},
		{
			name: "virtual credential scope not granted",
			code: ViolationTrustedJobVirtualCredential,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.VirtualCredentials[0].Scopes = []string{"admin"}
			},
		},
		{
			name: "callback destination mismatch",
			code: ViolationTrustedJobCallbackDestination,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.CallbackDestination = "other-coordinator"
			},
		},
		{
			name: "evidence kind not declared by scope",
			code: ViolationTrustedJobEvidence,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.Evidence.Kinds = []EvidenceKind{EvidenceLog}
			},
		},
		{
			name: "evidence prefix mismatch",
			code: ViolationTrustedJobEvidence,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.Evidence.Prefix = ".herd/evidence/other"
			},
		},
		{
			name: "missing idempotency key",
			code: ViolationTrustedJobIdempotency,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.IdempotencyKey = ""
			},
		},
		{
			name: "scope digest mismatch",
			code: ViolationTrustedJobDigest,
			mutate: func(sub *TrustedJobSubmission, _ *TrustedJobSubmissionValidationContext) {
				sub.ScopeDigest = "sha256:" + strings.Repeat("0", 64)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			submission := goldenTrustedJobSubmission(t)
			ctx := submissionValidationContext(t, submission)
			tc.mutate(&submission, &ctx)
			receipt := ValidateTrustedJobSubmission(submission, ctx)
			if receipt.Admitted || !receipt.Blocking || !hasTrustedJobSubmissionCode(receipt, tc.code) {
				t.Fatalf("unsafe submission was admitted: code=%s %+v", tc.code, receipt)
			}
			if receipt.Status != JobStatusRejected {
				t.Fatalf("rejected submission must carry rejected status: %s", receipt.Status)
			}
		})
	}
}

// TestTrustedJobCallbackBoundToAdmittedSubmission proves the inbound callback
// is bound to the admitted outbound submission digest and callback
// destination. A callback whose submission digest does not match the admitted
// submission is rejected, never silently admitted.
func TestTrustedJobCallbackBoundToAdmittedSubmission(t *testing.T) {
	submission := goldenTrustedJobSubmission(t)
	subReceipt := ValidateTrustedJobSubmission(submission, submissionValidationContext(t, submission))
	if !subReceipt.Admitted {
		t.Fatalf("submission must be admitted to bind callbacks: %+v", subReceipt)
	}

	callback := goldenTrustedJobCallback(t)
	if callback.SubmissionDigest != subReceipt.SubmissionDigest {
		t.Fatalf("callback fixture is not bound to the admitted submission digest: %q != %q", callback.SubmissionDigest, subReceipt.SubmissionDigest)
	}
	if callback.CallbackDestination != subReceipt.CallbackDestination || callback.IdempotencyKey != subReceipt.IdempotencyKey {
		t.Fatalf("callback fixture is not bound to the admitted submission binding")
	}

	ctx := trustedJobValidationContext(t, callback)
	ctx.ExpectedSubmissionDigest = subReceipt.SubmissionDigest
	ctx.ExpectedCallbackDestination = subReceipt.CallbackDestination
	ctx.ExpectedIdempotencyKey = subReceipt.IdempotencyKey
	if receipt := ValidateTrustedJobCallback(callback, ctx); !receipt.Accepted {
		t.Fatalf("callback bound to an admitted submission must be accepted: %+v", receipt)
	}

	ctx.ExpectedSubmissionDigest = "sha256:" + strings.Repeat("0", 64)
	receipt := ValidateTrustedJobCallback(callback, ctx)
	if receipt.Accepted || !receipt.Blocking || !hasTrustedJobCode(receipt, ViolationTrustedJobSubmission) {
		t.Fatalf("callback with a stale submission digest was admitted: %+v", receipt)
	}
}

// TestTrustedJobCallbackReplayNonVacuous closes replay behavior. A fresh key
// is admitted; the same key seen again is a duplicate; a stale key is rejected.
// The duplicate rejection is non-vacuous: without the seen-key guard the same
// callback would be admitted.
func TestTrustedJobCallbackReplayNonVacuous(t *testing.T) {
	callback := goldenTrustedJobCallback(t)

	freshCtx := trustedJobValidationContext(t, callback)
	if receipt := ValidateTrustedJobCallback(callback, freshCtx); !receipt.Accepted {
		t.Fatalf("fresh callback must be admitted: %+v", receipt)
	}

	duplicateCtx := trustedJobValidationContext(t, callback)
	duplicateCtx.SeenKeys = []string{callback.IdempotencyKey}
	duplicate := ValidateTrustedJobCallback(callback, duplicateCtx)
	if duplicate.Accepted || !duplicate.Blocking || !hasTrustedJobCode(duplicate, ViolationTrustedJobReplay) {
		t.Fatalf("duplicate callback (seen key) was admitted: %+v", duplicate)
	}
	if !strings.Contains(violationMessage(duplicate, ViolationTrustedJobReplay), "duplicate") {
		t.Fatalf("duplicate callback must be classified as a duplicate, not stale: %+v", duplicate)
	}

	staleCtx := trustedJobValidationContext(t, callback)
	stale := callback
	stale.IdempotencyKey = "idem-stale-SPE-558"
	staleReceipt := ValidateTrustedJobCallback(stale, staleCtx)
	if staleReceipt.Accepted || !staleReceipt.Blocking || !hasTrustedJobCode(staleReceipt, ViolationTrustedJobReplay) {
		t.Fatalf("stale callback (unknown key) was admitted: %+v", staleReceipt)
	}
	if !strings.Contains(violationMessage(staleReceipt, ViolationTrustedJobReplay), "stale") {
		t.Fatalf("stale callback must be classified as stale, not a duplicate: %+v", staleReceipt)
	}
}

func hasTrustedJobSubmissionCode(receipt TrustedJobSubmissionReceipt, code TrustedJobViolationCode) bool {
	for _, violation := range receipt.Violations {
		if violation.Code == code {
			return true
		}
	}
	return false
}

func violationMessage(receipt TrustedJobCallbackReceipt, code TrustedJobViolationCode) string {
	for _, v := range receipt.Violations {
		if v.Code == code {
			return v.Message
		}
	}
	return ""
}
