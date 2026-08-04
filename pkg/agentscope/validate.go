package agentscope

import (
	"fmt"
	"math"
	"net"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	shaPattern        = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	dnsLabelPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

type validator struct {
	violations []Violation
}

func (v *validator) add(code ViolationCode, field, message string) {
	v.violations = append(v.violations, Violation{Code: code, Field: field, Message: message})
}

// Validate checks an AgentScope against coordinator-authenticated context. It
// always returns a serializable receipt; any violation is explicitly blocking.
func Validate(scope AgentScope, ctx ValidationContext) ViolationReceipt {
	v := &validator{violations: make([]Violation, 0)}
	validateContext(v, ctx)
	validateIdentity(v, scope)
	validateRepository(v, scope, ctx)
	validateWorktree(v, scope, ctx)
	validatePaths(v, scope.Spec.Paths)
	validateCommandProfiles(v, scope.Spec.CommandProfiles, ctx)
	validateNetwork(v, scope.Spec.Network, ctx.PolicyCeiling)
	validateGit(v, scope.Spec.Git, ctx.PolicyCeiling)
	scopeExpiry, scopeExpiryOK := validateExpiry(v, scope.Metadata, ctx)
	validateCredentials(v, scope.Spec.Credentials, scope.Spec.Runtime.Provider, scopeExpiry, scopeExpiryOK, ctx)
	validateRuntime(v, scope.Spec.Runtime, scopeExpiry, scopeExpiryOK, ctx)
	validateEvidence(v, scope.Spec.Evidence, ctx.PolicyCeiling)
	validateGrants(v, scope.Spec.Grants)

	digest, err := Digest(scope)
	if err != nil {
		v.add(ViolationIdentitySchema, "$", "scope cannot be canonically encoded: "+err.Error())
	}
	blocking := len(v.violations) != 0
	return ViolationReceipt{
		APIVersion:  ReceiptAPIVersion,
		Kind:        ReceiptKind,
		ScopeID:     scope.Metadata.ID,
		RunID:       scope.Metadata.RunID,
		PhaseID:     scope.Metadata.PhaseID,
		ScopeDigest: digest,
		Valid:       !blocking,
		Blocking:    blocking,
		Violations:  v.violations,
	}
}

func validateContext(v *validator, ctx ValidationContext) {
	if !validIdentifier(ctx.RegisteredRepository) {
		v.add(ViolationContext, "context.registeredRepository", "registered repository is missing or malformed")
	}
	owned := ctx.OwnedWorktree
	if owned.Repository != ctx.RegisteredRepository || !validIdentifier(owned.Identity) || !safeRepositoryRoot(owned.RepositoryRoot) || !safeRelativePath(owned.Path) || owned.Path == owned.RepositoryRoot || !containedBy(owned.RepositoryRoot, owned.Path) || !shaPattern.MatchString(owned.BaseSHA) || !shaPattern.MatchString(owned.HeadSHA) {
		v.add(ViolationContext, "context.ownedWorktree", "owned worktree identity is incomplete or unsafe")
	}
	if !validStringSet(ctx.AllowedCommandProfiles, validIdentifier) {
		v.add(ViolationContext, "context.allowedCommandProfiles", "command profile allowlist must be non-empty, unique, and named")
	}
	if !validStringSet(ctx.AllowedCredentialRefs, validCredentialReference) {
		v.add(ViolationContext, "context.allowedCredentialRefs", "credential reference allowlist must be non-empty, unique, opaque, and non-master")
	}
	if !validStringSet(ctx.AllowedProviders, validIdentifier) {
		v.add(ViolationContext, "context.allowedProviders", "provider allowlist must be non-empty, unique, and named")
	}
	if !validStringSet(ctx.AllowedModels, validIdentifier) {
		v.add(ViolationContext, "context.allowedModels", "model allowlist must be non-empty, unique, and named")
	}
	c := ctx.PolicyCeiling
	if ctx.Now.IsZero() || c.MaxScopeTTLSeconds <= 0 || c.MaxCredentialTTLSeconds <= 0 || c.MaxDeadlineSeconds <= 0 || c.MaxTurns <= 0 || c.MaxTokens <= 0 || c.MaxStallTimeoutSeconds <= 0 || c.MaxLoopWindow <= 0 || c.MaxLoopRepeatThreshold <= 0 || c.MaxLoopRepeatThreshold > c.MaxLoopWindow || c.MaxPaidUSD <= 0 || c.MaxPaidUSD > HardMaxPaidUSD || c.MaxEvidenceBytes <= 0 || !safeRelativePath(c.EvidencePrefix) {
		v.add(ViolationContext, "context.policyCeiling", "policy ceiling is incomplete, unsafe, or exceeds a hard limit")
	}
	if !validGitActionSet(c.AllowedGitActions) || !validStringSet(c.AllowedCredentialScopes, validCredentialScope) || !validEvidenceKindSet(c.AllowedEvidenceKinds) {
		v.add(ViolationContext, "context.policyCeiling.allowlists", "policy allowlists must be non-empty, unique, recognized, and non-master")
	}
	if !validOptionalHostSet(c.AllowedNetworkHosts) {
		v.add(ViolationContext, "context.policyCeiling.allowedNetworkHosts", "network host allowlist contains an unsafe host")
	}
}

func validateIdentity(v *validator, scope AgentScope) {
	if scope.APIVersion != APIVersion {
		v.add(ViolationIdentitySchema, "apiVersion", fmt.Sprintf("must equal %q", APIVersion))
	}
	if scope.Kind != Kind {
		v.add(ViolationIdentitySchema, "kind", fmt.Sprintf("must equal %q", Kind))
	}
	identities := []struct {
		field string
		value string
	}{
		{"metadata.id", scope.Metadata.ID},
		{"metadata.runId", scope.Metadata.RunID},
		{"metadata.phaseId", scope.Metadata.PhaseID},
		{"metadata.task", scope.Metadata.Task},
		{"spec.subject.agentId", scope.Spec.Subject.AgentID},
		{"spec.subject.sessionId", scope.Spec.Subject.SessionID},
		{"spec.subject.lane", scope.Spec.Subject.Lane},
	}
	for _, identity := range identities {
		if !validIdentifier(identity.value) {
			v.add(ViolationIdentitySchema, identity.field, "must be a non-empty normalized identifier")
		}
	}
}

func validateRepository(v *validator, scope AgentScope, ctx ValidationContext) {
	repo := scope.Spec.Repository
	if repo.Identity == "" || repo.Identity != ctx.RegisteredRepository || repo.Identity != ctx.OwnedWorktree.Repository {
		v.add(ViolationRepositoryBinding, "spec.repository.identity", "must equal the registered repository and owned-worktree repository")
	}
	if !shaPattern.MatchString(repo.BaseSHA) {
		v.add(ViolationImmutableRevision, "spec.repository.baseSha", "must be an exact lowercase 40- or 64-hex immutable SHA")
	}
	if !shaPattern.MatchString(repo.HeadSHA) {
		v.add(ViolationImmutableRevision, "spec.repository.headSha", "must be an exact lowercase 40- or 64-hex immutable SHA")
	}
	if repo.BaseSHA != ctx.OwnedWorktree.BaseSHA || repo.HeadSHA != ctx.OwnedWorktree.HeadSHA {
		v.add(ViolationWorktreeOwnership, "spec.repository", "revision pair must exactly match the owned worktree")
	}
}

func validateWorktree(v *validator, scope AgentScope, ctx ValidationContext) {
	worktree := scope.Spec.Worktree
	owned := ctx.OwnedWorktree
	if worktree.Identity != owned.Identity || worktree.Path != owned.Path || worktree.RepositoryRoot != owned.RepositoryRoot {
		v.add(ViolationWorktreeOwnership, "spec.worktree", "must exactly match the coordinator-owned worktree identity")
	}
	if !worktree.Mutable || worktree.Shared {
		v.add(ViolationWorktreeState, "spec.worktree", "worktree must be mutable and non-shared")
	}
	if !safeRepositoryRoot(worktree.RepositoryRoot) || !safeRelativePath(worktree.Path) || worktree.Path == worktree.RepositoryRoot || !containedBy(worktree.RepositoryRoot, worktree.Path) {
		v.add(ViolationWorktreeState, "spec.worktree.path", "worktree must be normalized, contained, and distinct from the repository root")
	}
}

func validatePaths(v *validator, paths PathScope) {
	validatePathSet := func(field string, values []string) {
		if len(values) == 0 {
			v.add(ViolationPathSafety, field, "path set must not be empty")
			return
		}
		seen := make(map[string]struct{}, len(values))
		for i, value := range values {
			itemField := fmt.Sprintf("%s[%d]", field, i)
			if !safeRelativePath(value) || value == ".git" || strings.HasPrefix(value, ".git/") {
				v.add(ViolationPathSafety, itemField, "path must be normalized, relative, contained, and outside .git")
			}
			if _, exists := seen[value]; exists {
				v.add(ViolationPathSafety, itemField, "duplicate path is not allowed")
			}
			seen[value] = struct{}{}
		}
	}
	validatePathSet("spec.paths.readable", paths.Readable)
	validatePathSet("spec.paths.writable", paths.Writable)
}

func validateCommandProfiles(v *validator, profiles []string, ctx ValidationContext) {
	if len(profiles) == 0 {
		v.add(ViolationCommandProfile, "spec.commandProfiles", "at least one named command profile is required")
		return
	}
	allowed := stringSet(ctx.AllowedCommandProfiles)
	seen := map[string]struct{}{}
	for i, profile := range profiles {
		if !validIdentifier(profile) || !contains(allowed, profile) {
			v.add(ViolationCommandProfile, fmt.Sprintf("spec.commandProfiles[%d]", i), "unknown or malformed command profile")
		}
		if _, exists := seen[profile]; exists {
			v.add(ViolationCommandProfile, fmt.Sprintf("spec.commandProfiles[%d]", i), "duplicate command profile")
		}
		seen[profile] = struct{}{}
	}
}

func validateNetwork(v *validator, network NetworkPolicy, ceiling PolicyCeiling) {
	switch network.Mode {
	case NetworkDeny:
		if len(network.AllowedHosts) != 0 {
			v.add(ViolationNetworkPolicy, "spec.network.allowedHosts", "deny mode cannot carry hosts")
		}
	case NetworkAllowlist:
		if len(network.AllowedHosts) == 0 {
			v.add(ViolationNetworkPolicy, "spec.network.allowedHosts", "allowlist mode requires at least one host")
		}
	default:
		v.add(ViolationNetworkPolicy, "spec.network.mode", "network policy must explicitly be deny or allowlist")
	}
	allowed := stringSet(ceiling.AllowedNetworkHosts)
	seen := map[string]struct{}{}
	for i, host := range network.AllowedHosts {
		if !validHost(host) || !contains(allowed, host) {
			v.add(ViolationNetworkPolicy, fmt.Sprintf("spec.network.allowedHosts[%d]", i), "host is malformed or outside coordinator policy")
		}
		if _, exists := seen[host]; exists {
			v.add(ViolationNetworkPolicy, fmt.Sprintf("spec.network.allowedHosts[%d]", i), "duplicate host")
		}
		seen[host] = struct{}{}
	}
}

func validateGit(v *validator, git GitAuthority, ceiling PolicyCeiling) {
	if len(git.Actions) == 0 {
		v.add(ViolationGitAuthority, "spec.git.actions", "explicit bounded git authority is required")
		return
	}
	allowed := gitActionSet(ceiling.AllowedGitActions)
	seen := map[GitAction]struct{}{}
	for i, action := range git.Actions {
		if !knownGitAction(action) || !containsGit(allowed, action) {
			v.add(ViolationGitAuthority, fmt.Sprintf("spec.git.actions[%d]", i), "unknown or disallowed git authority; merge and deploy are never allowed")
		}
		if _, exists := seen[action]; exists {
			v.add(ViolationGitAuthority, fmt.Sprintf("spec.git.actions[%d]", i), "duplicate git authority")
		}
		seen[action] = struct{}{}
	}
}

func validateExpiry(v *validator, metadata ScopeMetadata, ctx ValidationContext) (time.Time, bool) {
	issued, issuedOK := normalizedTime(metadata.IssuedAt)
	expires, expiresOK := normalizedTime(metadata.ExpiresAt)
	if !issuedOK {
		v.add(ViolationExpiry, "metadata.issuedAt", "must be normalized UTC RFC3339")
	}
	if !expiresOK {
		v.add(ViolationExpiry, "metadata.expiresAt", "must be normalized UTC RFC3339")
	}
	if !issuedOK || !expiresOK {
		return time.Time{}, false
	}
	now := ctx.Now.UTC()
	if issued.After(now) || !expires.After(now) || !expires.After(issued) {
		v.add(ViolationExpiry, "metadata", "scope must be currently active with expiry after issuance")
	}
	if int64(expires.Sub(issued)/time.Second) > ctx.PolicyCeiling.MaxScopeTTLSeconds {
		v.add(ViolationExpiry, "metadata.expiresAt", "scope lifetime exceeds the policy ceiling")
	}
	return expires, true
}

func validateCredentials(v *validator, credentials []CredentialGrant, runtimeProvider string, scopeExpiry time.Time, scopeExpiryOK bool, ctx ValidationContext) {
	refs := stringSet(ctx.AllowedCredentialRefs)
	providers := stringSet(ctx.AllowedProviders)
	scopes := stringSet(ctx.PolicyCeiling.AllowedCredentialScopes)
	seenRefs := map[string]struct{}{}
	for i, credential := range credentials {
		prefix := fmt.Sprintf("spec.credentials[%d]", i)
		if !validCredentialReference(credential.Ref) || !contains(refs, credential.Ref) {
			v.add(ViolationCredentialScope, prefix+".ref", "credential must be an allowed opaque reference, never raw, wildcard, or master material")
		}
		if !validIdentifier(credential.Provider) || !contains(providers, credential.Provider) || credential.Provider != runtimeProvider {
			v.add(ViolationCredentialScope, prefix+".provider", "credential provider must be allowed and exactly match the runtime provider")
		}
		if _, exists := seenRefs[credential.Ref]; exists {
			v.add(ViolationCredentialScope, prefix+".ref", "duplicate credential reference")
		}
		seenRefs[credential.Ref] = struct{}{}
		if len(credential.Scopes) == 0 {
			v.add(ViolationCredentialScope, prefix+".scopes", "credential scopes must be explicit")
		}
		seenScopes := map[string]struct{}{}
		for j, grant := range credential.Scopes {
			if !validCredentialScope(grant) || !contains(scopes, grant) {
				v.add(ViolationCredentialScope, fmt.Sprintf("%s.scopes[%d]", prefix, j), "credential scope is unknown, wildcard, or over-broad")
			}
			if _, exists := seenScopes[grant]; exists {
				v.add(ViolationCredentialScope, fmt.Sprintf("%s.scopes[%d]", prefix, j), "duplicate credential scope")
			}
			seenScopes[grant] = struct{}{}
		}
		expires, ok := normalizedTime(credential.ExpiresAt)
		credentialTTL := int64(0)
		if ok {
			credentialTTL = int64(expires.Sub(ctx.Now.UTC()) / time.Second)
		}
		if !ok || !expires.After(ctx.Now.UTC()) || credentialTTL > ctx.PolicyCeiling.MaxCredentialTTLSeconds || (scopeExpiryOK && expires.After(scopeExpiry)) {
			v.add(ViolationExpiry, prefix+".expiresAt", "credential expiry must be normalized, future, within policy, and no later than scope expiry")
		}
	}
}

func validateRuntime(v *validator, runtime RuntimePolicy, scopeExpiry time.Time, scopeExpiryOK bool, ctx ValidationContext) {
	if !contains(stringSet(ctx.AllowedProviders), runtime.Provider) {
		v.add(ViolationRuntimeAllowlist, "spec.runtime.provider", "provider is not allowed")
	}
	if !contains(stringSet(ctx.AllowedModels), runtime.Model) {
		v.add(ViolationRuntimeAllowlist, "spec.runtime.model", "model is not allowed")
	}
	c := ctx.PolicyCeiling
	bounds := []struct {
		field string
		value int64
		max   int64
	}{
		{"deadlineSeconds", runtime.DeadlineSeconds, c.MaxDeadlineSeconds},
		{"maxTurns", runtime.MaxTurns, c.MaxTurns},
		{"maxTokens", runtime.MaxTokens, c.MaxTokens},
		{"stallTimeoutSeconds", runtime.StallTimeoutSeconds, c.MaxStallTimeoutSeconds},
		{"loopWindow", runtime.LoopWindow, c.MaxLoopWindow},
		{"loopRepeatThreshold", runtime.LoopRepeatThreshold, c.MaxLoopRepeatThreshold},
	}
	for _, bound := range bounds {
		if bound.value <= 0 || bound.value > bound.max {
			v.add(ViolationRuntimeBounds, "spec.runtime."+bound.field, "must be positive and no greater than the policy ceiling")
		}
	}
	if runtime.LoopRepeatThreshold > runtime.LoopWindow {
		v.add(ViolationRuntimeBounds, "spec.runtime.loopRepeatThreshold", "must not exceed loopWindow")
	}
	if scopeExpiryOK && runtime.DeadlineSeconds > int64(scopeExpiry.Sub(ctx.Now.UTC())/time.Second) {
		v.add(ViolationRuntimeBounds, "spec.runtime.deadlineSeconds", "deadline extends beyond scope expiry")
	}
	if math.IsNaN(runtime.MaxPaidUSD) || math.IsInf(runtime.MaxPaidUSD, 0) || runtime.MaxPaidUSD <= 0 || runtime.MaxPaidUSD > c.MaxPaidUSD || runtime.MaxPaidUSD > HardMaxPaidUSD {
		v.add(ViolationBudgetLimit, "spec.runtime.maxPaidUsd", "must be positive and no greater than both policy ceiling and 10 USD")
	}
}

func validateEvidence(v *validator, evidence EvidencePolicy, ceiling PolicyCeiling) {
	allowed := evidenceKindSet(ceiling.AllowedEvidenceKinds)
	if len(evidence.Kinds) == 0 {
		v.add(ViolationEvidencePolicy, "spec.evidence.kinds", "at least one evidence kind is required")
	}
	seen := map[EvidenceKind]struct{}{}
	for i, kind := range evidence.Kinds {
		if !knownEvidenceKind(kind) || !containsEvidence(allowed, kind) {
			v.add(ViolationEvidencePolicy, fmt.Sprintf("spec.evidence.kinds[%d]", i), "unknown or disallowed evidence kind")
		}
		if _, exists := seen[kind]; exists {
			v.add(ViolationEvidencePolicy, fmt.Sprintf("spec.evidence.kinds[%d]", i), "duplicate evidence kind")
		}
		seen[kind] = struct{}{}
	}
	if !safeRelativePath(evidence.Prefix) || evidence.Prefix != ceiling.EvidencePrefix {
		v.add(ViolationEvidencePolicy, "spec.evidence.prefix", "must exactly match the normalized coordinator evidence prefix")
	}
	if evidence.MaxBytes <= 0 || evidence.MaxBytes > ceiling.MaxEvidenceBytes {
		v.add(ViolationEvidencePolicy, "spec.evidence.maxBytes", "must be positive and no greater than the policy ceiling")
	}
}

func validateGrants(v *validator, grants InlineGrantPolicy) {
	if grants.Kubernetes {
		v.add(ViolationForbiddenCapability, "spec.grants.kubernetes", "inline Kubernetes grants are forbidden")
	}
	if grants.UnrestrictedShell {
		v.add(ViolationForbiddenCapability, "spec.grants.unrestrictedShell", "unrestricted shell grants are forbidden")
	}
	if len(grants.Extensions) != 0 {
		v.add(ViolationForbiddenCapability, "spec.grants.extensions", "inline extension grants are forbidden")
	}
}

func validIdentifier(value string) bool {
	return len(value) <= maxIdentifierLength && identifierPattern.MatchString(value) && strings.TrimSpace(value) == value
}

func validCredentialReference(value string) bool {
	return validIdentifier(value) && !unsafeCredentialValue(value)
}

func validCredentialScope(value string) bool {
	return validIdentifier(value) && !unsafeCredentialValue(value)
}

func unsafeCredentialValue(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(value, "*") || strings.Contains(lower, "master") || strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "ghp_") || strings.HasPrefix(lower, "github_pat_") || strings.HasPrefix(lower, "xoxb-")
}

func safeRepositoryRoot(value string) bool {
	return value == "." || safeRelativePath(value)
}

func safeRelativePath(value string) bool {
	if value == "" || value == "." || value == ".." || path.IsAbs(value) || strings.Contains(value, "\\") || strings.IndexFunc(value, unicode.IsSpace) >= 0 || strings.TrimSpace(value) != value {
		return false
	}
	clean := path.Clean(value)
	return clean == value && !strings.HasPrefix(clean, "../") && !strings.Contains(value, "//")
}

func containedBy(root, child string) bool {
	if root == "." {
		return safeRelativePath(child)
	}
	return child != root && strings.HasPrefix(child, root+"/")
}

func normalizedTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, value)
	return parsed, err == nil && strings.HasSuffix(value, "Z") && parsed.Format(time.RFC3339) == value
}

func validHost(value string) bool {
	if value == "" || value != strings.ToLower(value) || strings.ContainsAny(value, "/@* \\") {
		return false
	}
	host := value
	if strings.HasPrefix(value, "[") {
		var port string
		var err error
		host, port, err = net.SplitHostPort(value)
		if err != nil || !validPort(port) || net.ParseIP(host) == nil {
			return false
		}
	} else if strings.Count(value, ":") == 1 {
		parts := strings.SplitN(value, ":", 2)
		host = parts[0]
		if !validPort(parts[1]) {
			return false
		}
	} else if strings.Contains(value, ":") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if !dnsLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func validPort(value string) bool {
	port, err := strconv.Atoi(value)
	return err == nil && port > 0 && port <= 65535
}

func validStringSet(values []string, validate func(string) bool) bool {
	if len(values) == 0 {
		return false
	}
	seen := map[string]struct{}{}
	for _, value := range values {
		if !validate(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validOptionalHostSet(values []string) bool {
	seen := map[string]struct{}{}
	for _, value := range values {
		if !validHost(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func knownGitAction(action GitAction) bool {
	switch action {
	case GitStatus, GitDiff, GitAdd, GitCommit, GitFetch, GitPush, GitCreatePR:
		return true
	default:
		return false
	}
}

func validGitActionSet(values []GitAction) bool {
	if len(values) == 0 {
		return false
	}
	seen := map[GitAction]struct{}{}
	for _, value := range values {
		if !knownGitAction(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func knownEvidenceKind(kind EvidenceKind) bool {
	switch kind {
	case EvidenceTest, EvidenceLint, EvidenceDiff, EvidenceLog, EvidenceReceipt:
		return true
	default:
		return false
	}
}

func validEvidenceKindSet(values []EvidenceKind) bool {
	if len(values) == 0 {
		return false
	}
	seen := map[EvidenceKind]struct{}{}
	for _, value := range values {
		if !knownEvidenceKind(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func gitActionSet(values []GitAction) map[GitAction]struct{} {
	set := make(map[GitAction]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func evidenceKindSet(values []EvidenceKind) map[EvidenceKind]struct{} {
	set := make(map[EvidenceKind]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func contains(set map[string]struct{}, value string) bool {
	_, ok := set[value]
	return ok
}

func containsGit(set map[GitAction]struct{}, value GitAction) bool {
	_, ok := set[value]
	return ok
}

func containsEvidence(set map[EvidenceKind]struct{}, value EvidenceKind) bool {
	_, ok := set[value]
	return ok
}
