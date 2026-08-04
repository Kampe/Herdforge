package agentscope

import (
	"fmt"
	"regexp"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type trustedJobValidator struct {
	violations []TrustedJobViolation
}

func (v *trustedJobValidator) add(code TrustedJobViolationCode, field, message string) {
	v.violations = append(v.violations, TrustedJobViolation{Code: code, Field: field, Message: message})
}

// ValidateTrustedJobCallback validates a callback without starting, polling,
// or otherwise interacting with an external job runner. A callback is bound to
// the admitted submission by SubmissionDigest and CallbackDestination, and
// replay is closed by the coordinator-authenticated IdempotencyKey.
func ValidateTrustedJobCallback(request TrustedJobCallbackRequest, ctx TrustedJobCallbackValidationContext) TrustedJobCallbackReceipt {
	v := &trustedJobValidator{violations: make([]TrustedJobViolation, 0)}
	validateTrustedJobContext(v, ctx)
	validateTrustedJobEnvelope(v, request)

	scopeReceipt := Validate(request.Scope, ctx.Scope)
	for _, violation := range scopeReceipt.Violations {
		v.add(scopeViolationCode(violation.Code), "scope."+violation.Field, violation.Message)
	}

	actualScopeDigest, err := Digest(request.Scope)
	if err != nil {
		v.add(ViolationTrustedJobSchema, "scope", "scope cannot be canonically encoded: "+err.Error())
	} else {
		validateTrustedJobDigest(v, request.ScopeDigest, actualScopeDigest, ctx.ExpectedScopeDigest)
	}
	validateTrustedJobSubmissionBinding(v, request, ctx)
	validateTrustedJobReplay(v, request, ctx)
	validateTrustedJobCorrelation(v, request)
	validateTrustedJobCandidate(v, request, ctx.ExpectedCandidateSHA)
	validateTrustedJobSequence(v, request.Correlation.CallbackSequence, ctx.ExpectedCallbackSequence)
	validateTrustedJobProviderModel(v, request, ctx.AllowedProviderModels)
	validateTrustedJobEvidence(v, request.Evidence, request.Scope.Spec.Evidence.Kinds)

	blocking := len(v.violations) != 0
	return TrustedJobCallbackReceipt{
		APIVersion:          TrustedJobCallbackAPIVersion,
		Kind:                TrustedJobCallbackReceiptKind,
		Correlation:         request.Correlation,
		ScopeDigest:         request.ScopeDigest,
		SubmissionDigest:    request.SubmissionDigest,
		CallbackDestination: request.CallbackDestination,
		IdempotencyKey:      request.IdempotencyKey,
		Provider:            request.Job.Provider,
		EffectiveModel:      request.Job.EffectiveModel,
		Evidence:            request.Evidence,
		Accepted:            !blocking,
		Blocking:            blocking,
		Violations:          v.violations,
	}
}

func validateTrustedJobContext(v *trustedJobValidator, ctx TrustedJobCallbackValidationContext) {
	if !digestPattern.MatchString(ctx.ExpectedScopeDigest) {
		v.add(ViolationTrustedJobContext, "context.expectedScopeDigest", "expected scope digest must be a canonical sha256 digest")
	}
	if !shaPattern.MatchString(ctx.ExpectedCandidateSHA) {
		v.add(ViolationTrustedJobContext, "context.expectedCandidateSha", "expected candidate must be an exact lowercase immutable SHA")
	}
	if ctx.ExpectedCallbackSequence <= 0 {
		v.add(ViolationTrustedJobContext, "context.expectedCallbackSequence", "expected callback sequence must be positive")
	}
	if !validProviderModelSet(ctx.AllowedProviderModels) {
		v.add(ViolationTrustedJobContext, "context.allowedProviderModels", "provider-model allowlist must be non-empty, unique, and normalized")
	}
	if !digestPattern.MatchString(ctx.ExpectedSubmissionDigest) {
		v.add(ViolationTrustedJobContext, "context.expectedSubmissionDigest", "expected submission digest must be a canonical sha256 digest")
	}
	if !validIdentifier(ctx.ExpectedCallbackDestination) {
		v.add(ViolationTrustedJobContext, "context.expectedCallbackDestination", "expected callback destination must be a normalized identifier")
	}
	if !validIdentifier(ctx.ExpectedIdempotencyKey) {
		v.add(ViolationTrustedJobContext, "context.expectedIdempotencyKey", "expected idempotency key must be a normalized identifier")
	}
	if !validIdentifierSet(ctx.SeenKeys) {
		v.add(ViolationTrustedJobContext, "context.seenKeys", "seen keys must be unique normalized identifiers")
	}
}

func validateTrustedJobEnvelope(v *trustedJobValidator, request TrustedJobCallbackRequest) {
	if request.APIVersion != TrustedJobCallbackAPIVersion {
		v.add(ViolationTrustedJobSchema, "apiVersion", fmt.Sprintf("must equal %q", TrustedJobCallbackAPIVersion))
	}
	if request.Kind != TrustedJobCallbackKind {
		v.add(ViolationTrustedJobSchema, "kind", fmt.Sprintf("must equal %q", TrustedJobCallbackKind))
	}
}

func validateTrustedJobDigest(v *trustedJobValidator, given, actual, expected string) {
	if !digestPattern.MatchString(given) {
		v.add(ViolationTrustedJobDigest, "scopeDigest", "scope digest must be a canonical sha256 digest")
	}
	if given != actual {
		v.add(ViolationTrustedJobDigest, "scopeDigest", "scope digest does not match the canonical AgentScope digest")
	}
	if actual != expected {
		v.add(ViolationTrustedJobDigest, "scopeDigest", "scope digest does not match the coordinator-pinned scope digest")
	}
}

// validateTrustedJobSubmissionBinding closes the callback-to-submission link.
// The callback must name the exact admitted submission and the exact callback
// destination the coordinator recorded for that submission.
func validateTrustedJobSubmissionBinding(v *trustedJobValidator, request TrustedJobCallbackRequest, ctx TrustedJobCallbackValidationContext) {
	if !digestPattern.MatchString(request.SubmissionDigest) {
		v.add(ViolationTrustedJobSubmission, "submissionDigest", "submission digest must be a canonical sha256 digest")
	}
	if request.SubmissionDigest != ctx.ExpectedSubmissionDigest {
		v.add(ViolationTrustedJobSubmission, "submissionDigest", "submission digest must exactly match the coordinator-admitted submission digest")
	}
	if !validIdentifier(request.CallbackDestination) {
		v.add(ViolationTrustedJobSubmission, "callbackDestination", "callback destination must be a normalized identifier")
	}
	if request.CallbackDestination != ctx.ExpectedCallbackDestination {
		v.add(ViolationTrustedJobSubmission, "callbackDestination", "callback destination must exactly match the coordinator-recorded destination")
	}
}

// validateTrustedJobReplay closes replay behavior. A callback whose
// idempotency key is not the coordinator-authenticated expected key is stale; a
// callback whose key was already seen is a duplicate. Both are blocking.
func validateTrustedJobReplay(v *trustedJobValidator, request TrustedJobCallbackRequest, ctx TrustedJobCallbackValidationContext) {
	if !validIdentifier(request.IdempotencyKey) {
		v.add(ViolationTrustedJobReplay, "idempotencyKey", "idempotency key must be a normalized identifier")
		return
	}
	if request.IdempotencyKey != ctx.ExpectedIdempotencyKey {
		v.add(ViolationTrustedJobReplay, "idempotencyKey", "idempotency key is stale or unknown to the coordinator")
	}
	for _, seen := range ctx.SeenKeys {
		if request.IdempotencyKey == seen {
			v.add(ViolationTrustedJobReplay, "idempotencyKey", "duplicate callback: idempotency key already seen")
			return
		}
	}
}

func validateTrustedJobCorrelation(v *trustedJobValidator, request TrustedJobCallbackRequest) {
	correlation := request.Correlation
	identities := []struct {
		field string
		value string
	}{
		{"correlation.runId", correlation.RunID},
		{"correlation.phaseId", correlation.PhaseID},
		{"correlation.task", correlation.Task},
		{"correlation.scopeId", correlation.ScopeID},
	}
	for _, identity := range identities {
		if !validIdentifier(identity.value) {
			v.add(ViolationTrustedJobIdentity, identity.field, "must be a non-empty normalized identifier")
		}
	}
	if correlation.RunID != request.Scope.Metadata.RunID || correlation.PhaseID != request.Scope.Metadata.PhaseID || correlation.Task != request.Scope.Metadata.Task || correlation.ScopeID != request.Scope.Metadata.ID {
		v.add(ViolationTrustedJobIdentity, "correlation", "run, phase, task, and scope identity must exactly match AgentScope metadata")
	}
}

func validateTrustedJobCandidate(v *trustedJobValidator, request TrustedJobCallbackRequest, expected string) {
	candidate := request.Correlation.CandidateSHA
	if !shaPattern.MatchString(candidate) {
		v.add(ViolationTrustedJobCandidate, "correlation.candidateSha", "candidate must be an exact lowercase immutable SHA, never a branch or ref")
	}
	if !shaPattern.MatchString(request.Job.CandidateSHA) {
		v.add(ViolationTrustedJobCandidate, "job.candidateSha", "candidate must be an exact lowercase immutable SHA, never a branch or ref")
	}
	if candidate != request.Job.CandidateSHA || candidate != expected {
		v.add(ViolationTrustedJobCandidate, "candidateSha", "callback candidate must exactly match the coordinator-pinned candidate SHA")
	}
}

func validateTrustedJobSequence(v *trustedJobValidator, actual, expected int64) {
	if actual <= 0 || actual != expected {
		v.add(ViolationTrustedJobSequence, "correlation.callbackSequence", "callback sequence must exactly match the coordinator-pinned sequence")
	}
}

func validateTrustedJobProviderModel(v *trustedJobValidator, request TrustedJobCallbackRequest, allowed []ProviderModel) {
	job := request.Job
	if !validIdentifier(job.Provider) || !validIdentifier(job.EffectiveModel) {
		v.add(ViolationTrustedJobProviderModel, "job", "provider and effective model must be normalized identifiers")
	}
	if job.Provider != request.Scope.Spec.Runtime.Provider || job.EffectiveModel != request.Scope.Spec.Runtime.Model {
		v.add(ViolationTrustedJobProviderModel, "job", "provider and effective model must exactly match AgentScope runtime policy")
	}
	if !containsProviderModel(allowed, job.Provider, job.EffectiveModel) {
		v.add(ViolationTrustedJobProviderModel, "job", "provider and effective model are not an allowed exact pair")
	}
}

func validateTrustedJobEvidence(v *trustedJobValidator, evidence []TrustedJobEvidence, allowed []EvidenceKind) {
	if len(evidence) == 0 {
		v.add(ViolationTrustedJobEvidence, "evidence", "at least one digest-bound evidence record is required")
		return
	}
	allowedKinds := evidenceKindSet(allowed)
	seen := make(map[string]struct{}, len(evidence))
	for i, item := range evidence {
		field := fmt.Sprintf("evidence[%d]", i)
		if !knownEvidenceKind(item.Kind) || !containsEvidence(allowedKinds, item.Kind) {
			v.add(ViolationTrustedJobEvidence, field+".kind", "evidence kind is not declared by AgentScope")
		}
		if !digestPattern.MatchString(item.Digest) {
			v.add(ViolationTrustedJobEvidence, field+".digest", "evidence digest must be a canonical sha256 digest")
		}
		key := string(item.Kind) + "\x00" + item.Digest
		if _, ok := seen[key]; ok {
			v.add(ViolationTrustedJobEvidence, field, "duplicate evidence record")
		}
		seen[key] = struct{}{}
	}
}

func scopeViolationCode(code ViolationCode) TrustedJobViolationCode {
	switch code {
	case ViolationPathSafety, ViolationWorktreeState:
		return ViolationTrustedJobPath
	case ViolationCredentialScope:
		return ViolationTrustedJobCredential
	case ViolationRuntimeAllowlist:
		return ViolationTrustedJobProviderModel
	case ViolationRepositoryBinding, ViolationWorktreeOwnership, ViolationImmutableRevision:
		return ViolationTrustedJobIdentity
	default:
		return ViolationTrustedJobScope
	}
}

func validProviderModelSet(values []ProviderModel) bool {
	if len(values) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validIdentifier(value.Provider) || !validIdentifier(value.Model) {
			return false
		}
		key := value.Provider + "\x00" + value.Model
		if _, ok := seen[key]; ok {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

func containsProviderModel(values []ProviderModel, provider, model string) bool {
	for _, value := range values {
		if value.Provider == provider && value.Model == model {
			return true
		}
	}
	return false
}

// validIdentifierSet reports whether values are a set of unique normalized
// identifiers. An empty set is valid (for example, no callback keys seen yet).
func validIdentifierSet(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validIdentifier(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}
