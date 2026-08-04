// Package agentscope defines the versioned, contract-only scope admitted for a
// bounded agent runtime. It does not claim to enforce the scope at runtime.
package agentscope

import "time"

const (
	APIVersion          = "herdforge.dev/v1alpha1"
	Kind                = "AgentScope"
	ReceiptAPIVersion   = "herdforge.dev/v1alpha1"
	ReceiptKind         = "AgentScopeViolationReceipt"
	HardMaxPaidUSD      = 10.0
	maxIdentifierLength = 128
)

// AgentScope is the canonical v1alpha1 runtime-scope contract.
type AgentScope struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   ScopeMetadata `json:"metadata"`
	Spec       ScopeSpec     `json:"spec"`
}

type ScopeMetadata struct {
	ID        string `json:"id"`
	RunID     string `json:"runId"`
	PhaseID   string `json:"phaseId"`
	Task      string `json:"task"`
	IssuedAt  string `json:"issuedAt"`
	ExpiresAt string `json:"expiresAt"`
}

type ScopeSpec struct {
	Subject         Subject           `json:"subject"`
	Repository      RepositoryScope   `json:"repository"`
	Worktree        WorktreeScope     `json:"worktree"`
	Paths           PathScope         `json:"paths"`
	CommandProfiles []string          `json:"commandProfiles"`
	Network         NetworkPolicy     `json:"network"`
	Git             GitAuthority      `json:"git"`
	Credentials     []CredentialGrant `json:"credentials"`
	Runtime         RuntimePolicy     `json:"runtime"`
	Evidence        EvidencePolicy    `json:"evidence"`
	Grants          InlineGrantPolicy `json:"grants"`
}

type Subject struct {
	AgentID   string `json:"agentId"`
	SessionID string `json:"sessionId"`
	Lane      string `json:"lane"`
}

type RepositoryScope struct {
	Identity string `json:"identity"`
	BaseSHA  string `json:"baseSha"`
	HeadSHA  string `json:"headSha"`
}

type WorktreeScope struct {
	Identity       string `json:"identity"`
	Path           string `json:"path"`
	RepositoryRoot string `json:"repositoryRoot"`
	Mutable        bool   `json:"mutable"`
	Shared         bool   `json:"shared"`
}

type PathScope struct {
	Readable []string `json:"readable"`
	Writable []string `json:"writable"`
}

type NetworkMode string

const (
	NetworkDeny      NetworkMode = "deny"
	NetworkAllowlist NetworkMode = "allowlist"
)

type NetworkPolicy struct {
	Mode         NetworkMode `json:"mode"`
	AllowedHosts []string    `json:"allowedHosts"`
}

type GitAction string

const (
	GitStatus   GitAction = "status"
	GitDiff     GitAction = "diff"
	GitAdd      GitAction = "add"
	GitCommit   GitAction = "commit"
	GitFetch    GitAction = "fetch"
	GitPush     GitAction = "push"
	GitCreatePR GitAction = "create_pr"
)

type GitAuthority struct {
	Actions []GitAction `json:"actions"`
}

type CredentialGrant struct {
	Ref       string   `json:"ref"`
	Provider  string   `json:"provider"`
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expiresAt"`
}

type RuntimePolicy struct {
	Provider            string  `json:"provider"`
	Model               string  `json:"model"`
	DeadlineSeconds     int64   `json:"deadlineSeconds"`
	MaxTurns            int64   `json:"maxTurns"`
	MaxTokens           int64   `json:"maxTokens"`
	StallTimeoutSeconds int64   `json:"stallTimeoutSeconds"`
	LoopWindow          int64   `json:"loopWindow"`
	LoopRepeatThreshold int64   `json:"loopRepeatThreshold"`
	MaxPaidUSD          float64 `json:"maxPaidUsd"`
}

type EvidenceKind string

const (
	EvidenceTest    EvidenceKind = "test"
	EvidenceLint    EvidenceKind = "lint"
	EvidenceDiff    EvidenceKind = "diff"
	EvidenceLog     EvidenceKind = "log"
	EvidenceReceipt EvidenceKind = "receipt"
)

type EvidencePolicy struct {
	Kinds    []EvidenceKind `json:"kinds"`
	Prefix   string         `json:"prefix"`
	MaxBytes int64          `json:"maxBytes"`
}

// InlineGrantPolicy is intentionally deny-only in v1alpha1. Named command
// profiles are the sole command intent surface.
type InlineGrantPolicy struct {
	Kubernetes        bool     `json:"kubernetes"`
	UnrestrictedShell bool     `json:"unrestrictedShell"`
	Extensions        []string `json:"extensions"`
}

// WorktreeIdentity is the exact coordinator-authenticated checkout binding.
type WorktreeIdentity struct {
	Repository     string
	Identity       string
	Path           string
	RepositoryRoot string
	BaseSHA        string
	HeadSHA        string
}

// PolicyCeiling is coordinator policy, not agent-supplied scope data.
type PolicyCeiling struct {
	AllowedNetworkHosts     []string
	AllowedGitActions       []GitAction
	AllowedCredentialScopes []string
	AllowedEvidenceKinds    []EvidenceKind
	EvidencePrefix          string
	MaxScopeTTLSeconds      int64
	MaxCredentialTTLSeconds int64
	MaxDeadlineSeconds      int64
	MaxTurns                int64
	MaxTokens               int64
	MaxStallTimeoutSeconds  int64
	MaxLoopWindow           int64
	MaxLoopRepeatThreshold  int64
	MaxPaidUSD              float64
	MaxEvidenceBytes        int64
}

// ValidationContext contains only coordinator-authenticated facts and policy.
type ValidationContext struct {
	RegisteredRepository   string
	OwnedWorktree          WorktreeIdentity
	AllowedCommandProfiles []string
	AllowedCredentialRefs  []string
	AllowedProviders       []string
	AllowedModels          []string
	PolicyCeiling          PolicyCeiling
	Now                    time.Time
}

type ViolationCode string

const (
	ViolationContext             ViolationCode = "validation_context"
	ViolationIdentitySchema      ViolationCode = "identity_schema"
	ViolationRepositoryBinding   ViolationCode = "repository_binding"
	ViolationWorktreeOwnership   ViolationCode = "worktree_ownership"
	ViolationWorktreeState       ViolationCode = "worktree_state"
	ViolationImmutableRevision   ViolationCode = "immutable_revision"
	ViolationPathSafety          ViolationCode = "path_safety"
	ViolationCommandProfile      ViolationCode = "command_profile"
	ViolationNetworkPolicy       ViolationCode = "network_policy"
	ViolationGitAuthority        ViolationCode = "git_authority"
	ViolationCredentialScope     ViolationCode = "credential_scope"
	ViolationExpiry              ViolationCode = "expiry"
	ViolationRuntimeAllowlist    ViolationCode = "runtime_allowlist"
	ViolationRuntimeBounds       ViolationCode = "runtime_bounds"
	ViolationBudgetLimit         ViolationCode = "budget_limit"
	ViolationEvidencePolicy      ViolationCode = "evidence_policy"
	ViolationForbiddenCapability ViolationCode = "forbidden_capability"
)

type Violation struct {
	Code    ViolationCode `json:"code"`
	Field   string        `json:"field"`
	Message string        `json:"message"`
}

// ViolationReceipt is serializable admission evidence. Blocking is true for
// every violation; downstream lifecycle code need not reinterpret messages.
type ViolationReceipt struct {
	APIVersion  string      `json:"apiVersion"`
	Kind        string      `json:"kind"`
	ScopeID     string      `json:"scopeId"`
	RunID       string      `json:"runId,omitempty"`
	PhaseID     string      `json:"phaseId,omitempty"`
	ScopeDigest string      `json:"scopeDigest,omitempty"`
	Valid       bool        `json:"valid"`
	Blocking    bool        `json:"blocking"`
	Violations  []Violation `json:"violations"`
}
