package agentscope

import "fmt"

// ValidateTrustedJobSubmission validates an outbound submission without
// starting, polling, or otherwise interacting with an external job runner. It
// preserves the strong inbound callback guarantees on the embedded AgentScope
// and adds the outbound admission contract: repository identity, pinned
// trusted configuration, scoped virtual credentials, callback destination,
// evidence policy, and idempotency.
func ValidateTrustedJobSubmission(submission TrustedJobSubmission, ctx TrustedJobSubmissionValidationContext) TrustedJobSubmissionReceipt {
	v := &trustedJobValidator{violations: make([]TrustedJobViolation, 0)}
	validateTrustedJobSubmissionContext(v, ctx)
	validateTrustedJobSubmissionEnvelope(v, submission)

	scopeReceipt := Validate(submission.Scope, ctx.Scope)
	for _, violation := range scopeReceipt.Violations {
		v.add(scopeViolationCode(violation.Code), "scope."+violation.Field, violation.Message)
	}

	actualScopeDigest, err := Digest(submission.Scope)
	if err != nil {
		v.add(ViolationTrustedJobSchema, "scope", "scope cannot be canonically encoded: "+err.Error())
	} else {
		validateTrustedJobSubmissionScopeDigest(v, submission.ScopeDigest, actualScopeDigest, ctx.ExpectedScopeDigest)
	}
	validateTrustedJobSubmissionRepository(v, submission, ctx)
	validateTrustedJobSubmissionConfig(v, submission)
	validateTrustedJobSubmissionWorkspace(v, submission, ctx)
	validateTrustedJobJobIdentity(v, submission)
	validateTrustedJobSubmissionCorrelation(v, submission)
	validateTrustedJobSubmissionCandidate(v, submission, ctx.ExpectedCandidateSHA)
	validateTrustedJobSubmissionProviderModel(v, submission, ctx.AllowedProviderModels)
	validateTrustedJobSubmissionVirtualCredentials(v, submission)
	validateTrustedJobSubmissionCallbackDestination(v, submission, ctx)
	validateTrustedJobSubmissionEvidence(v, submission, ctx)
	validateTrustedJobSubmissionIdempotency(v, submission)

	submissionDigest, digestErr := TrustedJobSubmissionDigest(submission)
	if digestErr != nil {
		v.add(ViolationTrustedJobSchema, "$", "submission cannot be canonically encoded: "+digestErr.Error())
	}

	blocking := len(v.violations) != 0
	status := JobStatusAdmitted
	if blocking {
		status = JobStatusRejected
	}
	return TrustedJobSubmissionReceipt{
		APIVersion:          TrustedJobSubmissionAPIVersion,
		Kind:                TrustedJobSubmissionReceiptKind,
		Correlation:         submission.Correlation,
		Job:                 submission.Job,
		Workspace:           submission.Workspace,
		SubmissionDigest:    submissionDigest,
		ScopeDigest:         submission.ScopeDigest,
		Provider:            submission.Provider,
		Model:               submission.Model,
		CallbackDestination: submission.CallbackDestination,
		IdempotencyKey:      submission.IdempotencyKey,
		Status:              status,
		Admitted:            !blocking,
		Blocking:            blocking,
		Violations:          v.violations,
	}
}

func validateTrustedJobSubmissionContext(v *trustedJobValidator, ctx TrustedJobSubmissionValidationContext) {
	if !digestPattern.MatchString(ctx.ExpectedScopeDigest) {
		v.add(ViolationTrustedJobContext, "context.expectedScopeDigest", "expected scope digest must be a canonical sha256 digest")
	}
	if !shaPattern.MatchString(ctx.ExpectedCandidateSHA) {
		v.add(ViolationTrustedJobContext, "context.expectedCandidateSha", "expected candidate must be an exact lowercase immutable SHA")
	}
	if !validIdentifier(ctx.ExpectedCallbackDestination) {
		v.add(ViolationTrustedJobContext, "context.expectedCallbackDestination", "expected callback destination must be a normalized identifier")
	}
	if !validProviderModelSet(ctx.AllowedProviderModels) {
		v.add(ViolationTrustedJobContext, "context.allowedProviderModels", "provider-model allowlist must be non-empty, unique, and normalized")
	}
}

func validateTrustedJobSubmissionEnvelope(v *trustedJobValidator, submission TrustedJobSubmission) {
	if submission.APIVersion != TrustedJobSubmissionAPIVersion {
		v.add(ViolationTrustedJobSchema, "apiVersion", fmt.Sprintf("must equal %q", TrustedJobSubmissionAPIVersion))
	}
	if submission.Kind != TrustedJobSubmissionKind {
		v.add(ViolationTrustedJobSchema, "kind", fmt.Sprintf("must equal %q", TrustedJobSubmissionKind))
	}
}

func validateTrustedJobSubmissionScopeDigest(v *trustedJobValidator, given, actual, expected string) {
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

func validateTrustedJobSubmissionRepository(v *trustedJobValidator, submission TrustedJobSubmission, ctx TrustedJobSubmissionValidationContext) {
	repo := submission.Repository
	if !validIdentifier(repo.Identity) || repo.Identity != submission.Scope.Spec.Repository.Identity || repo.Identity != ctx.Scope.RegisteredRepository {
		v.add(ViolationTrustedJobIdentity, "repository.identity", "must equal the AgentScope repository and the registered repository")
	}
	if !shaPattern.MatchString(repo.BaseSHA) || repo.BaseSHA != submission.Scope.Spec.Repository.BaseSHA || repo.BaseSHA != ctx.Scope.OwnedWorktree.BaseSHA {
		v.add(ViolationTrustedJobIdentity, "repository.baseSha", "must be an exact immutable SHA matching the AgentScope and owned worktree")
	}
	if !shaPattern.MatchString(repo.HeadSHA) || repo.HeadSHA != submission.Scope.Spec.Repository.HeadSHA || repo.HeadSHA != ctx.Scope.OwnedWorktree.HeadSHA {
		v.add(ViolationTrustedJobIdentity, "repository.headSha", "must be an exact immutable SHA matching the AgentScope and owned worktree")
	}
}

// validateTrustedJobSubmissionConfig rejects inline YAML or manifests, floating
// refs, path escapes, and an untrusted configuration repository. The trusted
// configuration is referenced by immutable commit, repo-relative path, and
// content digest; it is never embedded.
func validateTrustedJobSubmissionConfig(v *trustedJobValidator, submission TrustedJobSubmission) {
	cfg := submission.TrustedConfig
	if cfg.Repository != submission.Repository.Identity {
		v.add(ViolationTrustedJobConfig, "trustedConfig.repository", "trusted configuration repository must exactly match the submission repository identity")
	}
	if !shaPattern.MatchString(cfg.CommitSHA) {
		v.add(ViolationTrustedJobConfig, "trustedConfig.commitSha", "trusted configuration must pin an exact lowercase immutable commit SHA, never a floating ref")
	}
	if !safeRelativePath(cfg.Path) || cfg.Path == ".git" || hasGitPathPrefix(cfg.Path) {
		v.add(ViolationTrustedJobConfig, "trustedConfig.path", "trusted configuration path must be repo-relative, normalized, contained, and outside .git")
	}
	if !digestPattern.MatchString(cfg.ContentDigest) {
		v.add(ViolationTrustedJobConfig, "trustedConfig.contentDigest", "trusted configuration content digest must be a canonical sha256 digest")
	}
}

func validateTrustedJobSubmissionWorkspace(v *trustedJobValidator, submission TrustedJobSubmission, ctx TrustedJobSubmissionValidationContext) {
	ws := submission.Workspace
	owned := ctx.Scope.OwnedWorktree
	if ws.Repository != submission.Repository.Identity || ws.Repository != owned.Repository {
		v.add(ViolationTrustedJobWorkspace, "workspace.repository", "must equal the submission repository and the owned worktree repository")
	}
	if ws.Worktree != submission.Scope.Spec.Worktree.Identity || ws.Worktree != owned.Identity {
		v.add(ViolationTrustedJobWorkspace, "workspace.worktree", "must equal the AgentScope worktree identity and the owned worktree identity")
	}
	if ws.Path != submission.Scope.Spec.Worktree.Path || ws.Path != owned.Path {
		v.add(ViolationTrustedJobWorkspace, "workspace.path", "must equal the AgentScope worktree path and the owned worktree path")
	}
	if ws.RepositoryRoot != submission.Scope.Spec.Worktree.RepositoryRoot || ws.RepositoryRoot != owned.RepositoryRoot {
		v.add(ViolationTrustedJobWorkspace, "workspace.repositoryRoot", "must equal the AgentScope worktree repository root and the owned worktree repository root")
	}
	if !shaPattern.MatchString(ws.BaseSHA) || ws.BaseSHA != submission.Repository.BaseSHA {
		v.add(ViolationTrustedJobWorkspace, "workspace.baseSha", "must be an exact immutable SHA matching the submission repository base SHA")
	}
	if !shaPattern.MatchString(ws.HeadSHA) || ws.HeadSHA != submission.Repository.HeadSHA {
		v.add(ViolationTrustedJobWorkspace, "workspace.headSha", "must be an exact immutable SHA matching the submission repository head SHA")
	}
}

func validateTrustedJobJobIdentity(v *trustedJobValidator, submission TrustedJobSubmission) {
	job := submission.Job
	identities := []struct {
		field string
		value string
	}{
		{"job.runId", job.RunID},
		{"job.phaseId", job.PhaseID},
		{"job.scopeId", job.ScopeID},
	}
	for _, identity := range identities {
		if !validIdentifier(identity.value) {
			v.add(ViolationTrustedJobIdentity, identity.field, "must be a non-empty normalized identifier")
		}
	}
	if !shaPattern.MatchString(job.CandidateSHA) {
		v.add(ViolationTrustedJobCandidate, "job.candidateSha", "candidate must be an exact lowercase immutable SHA, never a branch or ref")
	}
	if job.RunID != submission.Scope.Metadata.RunID || job.PhaseID != submission.Scope.Metadata.PhaseID || job.ScopeID != submission.Scope.Metadata.ID {
		v.add(ViolationTrustedJobIdentity, "job", "run, phase, and scope identity must exactly match AgentScope metadata")
	}
}

func validateTrustedJobSubmissionCorrelation(v *trustedJobValidator, submission TrustedJobSubmission) {
	correlation := submission.Correlation
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
	if correlation.RunID != submission.Scope.Metadata.RunID || correlation.PhaseID != submission.Scope.Metadata.PhaseID || correlation.Task != submission.Scope.Metadata.Task || correlation.ScopeID != submission.Scope.Metadata.ID {
		v.add(ViolationTrustedJobIdentity, "correlation", "run, phase, task, and scope identity must exactly match AgentScope metadata")
	}
	if correlation.RunID != submission.Job.RunID || correlation.PhaseID != submission.Job.PhaseID || correlation.ScopeID != submission.Job.ScopeID {
		v.add(ViolationTrustedJobIdentity, "correlation", "run, phase, and scope identity must exactly match the job identity")
	}
	if correlation.CallbackSequence <= 0 {
		v.add(ViolationTrustedJobSequence, "correlation.callbackSequence", "callback sequence must be positive")
	}
}

func validateTrustedJobSubmissionCandidate(v *trustedJobValidator, submission TrustedJobSubmission, expected string) {
	candidate := submission.Correlation.CandidateSHA
	if !shaPattern.MatchString(candidate) {
		v.add(ViolationTrustedJobCandidate, "correlation.candidateSha", "candidate must be an exact lowercase immutable SHA, never a branch or ref")
	}
	if candidate != submission.Job.CandidateSHA || candidate != expected {
		v.add(ViolationTrustedJobCandidate, "candidateSha", "submission candidate must exactly match the coordinator-pinned candidate SHA")
	}
}

func validateTrustedJobSubmissionProviderModel(v *trustedJobValidator, submission TrustedJobSubmission, allowed []ProviderModel) {
	if !validIdentifier(submission.Provider) || !validIdentifier(submission.Model) {
		v.add(ViolationTrustedJobProviderModel, "provider", "provider and model must be normalized identifiers")
	}
	if submission.Provider != submission.Scope.Spec.Runtime.Provider || submission.Model != submission.Scope.Spec.Runtime.Model {
		v.add(ViolationTrustedJobProviderModel, "provider", "provider and model must exactly match AgentScope runtime policy")
	}
	if !containsProviderModel(allowed, submission.Provider, submission.Model) {
		v.add(ViolationTrustedJobProviderModel, "provider", "provider and model are not an allowed exact pair")
	}
}

// validateTrustedJobSubmissionVirtualCredentials enforces that virtual
// credential references are scoped, opaque, and already allowed by the
// embedded AgentScope. Raw, wildcard, or master material and references outside
// the scope are blocking.
func validateTrustedJobSubmissionVirtualCredentials(v *trustedJobValidator, submission TrustedJobSubmission) {
	if len(submission.VirtualCredentials) == 0 {
		v.add(ViolationTrustedJobVirtualCredential, "virtualCredentials", "at least one scoped virtual credential reference is required")
		return
	}
	allowed := make(map[string]CredentialGrant, len(submission.Scope.Spec.Credentials))
	for _, grant := range submission.Scope.Spec.Credentials {
		allowed[grant.Ref] = grant
	}
	seen := make(map[string]struct{}, len(submission.VirtualCredentials))
	for i, ref := range submission.VirtualCredentials {
		field := fmt.Sprintf("virtualCredentials[%d]", i)
		if !validCredentialReference(ref.Ref) {
			v.add(ViolationTrustedJobVirtualCredential, field+".ref", "virtual credential must be an opaque reference, never raw, wildcard, or master material")
		}
		grant, ok := allowed[ref.Ref]
		if !ok {
			v.add(ViolationTrustedJobVirtualCredential, field+".ref", "virtual credential reference must already be allowed by the embedded AgentScope")
		}
		if _, exists := seen[ref.Ref]; exists {
			v.add(ViolationTrustedJobVirtualCredential, field+".ref", "duplicate virtual credential reference")
		}
		seen[ref.Ref] = struct{}{}
		if ok && grant.Provider != submission.Provider {
			v.add(ViolationTrustedJobVirtualCredential, field+".ref", "virtual credential provider must match the submission provider")
		}
		if len(ref.Scopes) == 0 {
			v.add(ViolationTrustedJobVirtualCredential, field+".scopes", "virtual credential scopes must be explicit")
		}
		grantedScopes := stringSet(grant.Scopes)
		seenScopes := map[string]struct{}{}
		for j, scope := range ref.Scopes {
			if !validCredentialScope(scope) {
				v.add(ViolationTrustedJobVirtualCredential, fmt.Sprintf("%s.scopes[%d]", field, j), "virtual credential scope is unknown, wildcard, or over-broad")
			}
			if ok && !contains(grantedScopes, scope) {
				v.add(ViolationTrustedJobVirtualCredential, fmt.Sprintf("%s.scopes[%d]", field, j), "virtual credential scope must already be granted by the embedded AgentScope")
			}
			if _, exists := seenScopes[scope]; exists {
				v.add(ViolationTrustedJobVirtualCredential, fmt.Sprintf("%s.scopes[%d]", field, j), "duplicate virtual credential scope")
			}
			seenScopes[scope] = struct{}{}
		}
	}
}

func validateTrustedJobSubmissionCallbackDestination(v *trustedJobValidator, submission TrustedJobSubmission, ctx TrustedJobSubmissionValidationContext) {
	if !validIdentifier(submission.CallbackDestination) {
		v.add(ViolationTrustedJobCallbackDestination, "callbackDestination", "callback destination must be a normalized identifier")
	}
	if submission.CallbackDestination != ctx.ExpectedCallbackDestination {
		v.add(ViolationTrustedJobCallbackDestination, "callbackDestination", "callback destination must exactly match the coordinator-recorded destination")
	}
}

func validateTrustedJobSubmissionEvidence(v *trustedJobValidator, submission TrustedJobSubmission, ctx TrustedJobSubmissionValidationContext) {
	allowed := evidenceKindSet(ctx.Scope.PolicyCeiling.AllowedEvidenceKinds)
	if len(submission.Evidence.Kinds) == 0 {
		v.add(ViolationTrustedJobEvidence, "evidence.kinds", "at least one evidence kind is required")
	}
	seen := map[EvidenceKind]struct{}{}
	for i, kind := range submission.Evidence.Kinds {
		if !knownEvidenceKind(kind) || !containsEvidence(allowed, kind) {
			v.add(ViolationTrustedJobEvidence, fmt.Sprintf("evidence.kinds[%d]", i), "unknown or disallowed evidence kind")
		}
		if _, exists := seen[kind]; exists {
			v.add(ViolationTrustedJobEvidence, fmt.Sprintf("evidence.kinds[%d]", i), "duplicate evidence kind")
		}
		seen[kind] = struct{}{}
	}
	if !safeRelativePath(submission.Evidence.Prefix) || submission.Evidence.Prefix != ctx.Scope.PolicyCeiling.EvidencePrefix {
		v.add(ViolationTrustedJobEvidence, "evidence.prefix", "must exactly match the normalized coordinator evidence prefix")
	}
	if submission.Evidence.MaxBytes <= 0 || submission.Evidence.MaxBytes > ctx.Scope.PolicyCeiling.MaxEvidenceBytes {
		v.add(ViolationTrustedJobEvidence, "evidence.maxBytes", "must be positive and no greater than the policy ceiling")
	}
	if len(submission.Evidence.Kinds) == 0 {
		return
	}
	// Evidence kinds carried by the submission must be a subset of the scope's
	// declared evidence kinds so a callback cannot demand evidence the scope never
	// permitted.
	scopeKinds := evidenceKindSet(submission.Scope.Spec.Evidence.Kinds)
	for i, kind := range submission.Evidence.Kinds {
		if !containsEvidence(scopeKinds, kind) {
			v.add(ViolationTrustedJobEvidence, fmt.Sprintf("evidence.kinds[%d]", i), "evidence kind must be declared by the embedded AgentScope")
		}
	}
}

func validateTrustedJobSubmissionIdempotency(v *trustedJobValidator, submission TrustedJobSubmission) {
	if !validIdentifier(submission.IdempotencyKey) {
		v.add(ViolationTrustedJobIdempotency, "idempotencyKey", "idempotency key must be a normalized identifier")
	}
}

// hasGitPathPrefix reports whether a repo-relative path is the .git dir or
// lives beneath it.
func hasGitPathPrefix(value string) bool {
	return value == ".git" || len(value) >= 5 && value[:5] == ".git/"
}
