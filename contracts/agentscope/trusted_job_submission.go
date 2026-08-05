package agentscope

import "context"

const (
	// TrustedJobSubmissionAPIVersion identifies the versioned outbound admission contract.
	TrustedJobSubmissionAPIVersion = "herdforge.dev/v1alpha1"
	// TrustedJobSubmissionKind identifies a submission admitted for a trusted job.
	TrustedJobSubmissionKind = "TrustedJobSubmission"
	// TrustedJobSubmissionReceiptKind identifies an admission receipt for a submission.
	TrustedJobSubmissionReceiptKind = "TrustedJobSubmissionReceipt"
)

// TrustedJobStatus is the typed lifecycle status carried by an admission
// receipt. Execution statuses (running, succeeded, failed) are deliberately
// out of scope: admission attests only that a submission was accepted, never
// that a job ran.
type TrustedJobStatus string

const (
	// JobStatusAdmitted marks a submission that satisfied the admission contract.
	JobStatusAdmitted TrustedJobStatus = "admitted"
	// JobStatusRejected marks a submission that was blocked by an admission
	// violation. A rejected submission never starts a job.
	JobStatusRejected TrustedJobStatus = "rejected"
)

// TrustedJobRepository is the repository identity a submission is scoped to.
// It must exactly match the AgentScope repository binding.
type TrustedJobRepository struct {
	Identity string `json:"identity"`
	BaseSHA  string `json:"baseSha"`
	HeadSHA  string `json:"headSha"`
}

// TrustedConfig pins the exact trusted-job configuration that governs a
// submission by immutable commit, repo-relative path, and content digest. It
// rejects inline YAML or manifests: the configuration is referenced, never
// embedded.
type TrustedConfig struct {
	Repository    string `json:"repository"`
	CommitSHA     string `json:"commitSha"`
	Path          string `json:"path"`
	ContentDigest string `json:"contentDigest"`
}

// VirtualCredentialRef is a scoped, opaque credential reference carried by a
// submission. It carries no secret material: the reference must already be
// allowed by the embedded AgentScope, and its scopes must be a subset of the
// scope's granted scopes.
type VirtualCredentialRef struct {
	Ref    string   `json:"ref"`
	Scopes []string `json:"scopes"`
}

// JobIdentity is the first-class identity of a trusted job. It is derived from
// the run-phase-task correlation and the pinned candidate.
type JobIdentity struct {
	RunID        string `json:"runId"`
	PhaseID      string `json:"phaseId"`
	ScopeID      string `json:"scopeId"`
	CandidateSHA string `json:"candidateSha"`
}

// WorkspaceIdentity is the first-class workspace identity a job runs in. It is
// derived from the coordinator-owned worktree and repository revision pair.
type WorkspaceIdentity struct {
	Repository     string `json:"repository"`
	Worktree       string `json:"worktree"`
	Path           string `json:"path"`
	RepositoryRoot string `json:"repositoryRoot"`
	BaseSHA        string `json:"baseSha"`
	HeadSHA        string `json:"headSha"`
}

// TrustedJobSubmission is the strict, versioned outbound admission contract.
// A trusted-job adapter admits a submission against coordinator-authenticated
// facts; admission neither starts nor proves execution of a job.
type TrustedJobSubmission struct {
	APIVersion          string                 `json:"apiVersion"`
	Kind                string                 `json:"kind"`
	Correlation         TrustedJobCorrelation  `json:"correlation"`
	Job                 JobIdentity            `json:"job"`
	Workspace           WorkspaceIdentity      `json:"workspace"`
	Repository          TrustedJobRepository   `json:"repository"`
	TrustedConfig       TrustedConfig          `json:"trustedConfig"`
	Scope               AgentScope             `json:"scope"`
	ScopeDigest         string                 `json:"scopeDigest"`
	Provider            string                 `json:"provider"`
	Model               string                 `json:"model"`
	VirtualCredentials  []VirtualCredentialRef `json:"virtualCredentials"`
	CallbackDestination string                 `json:"callbackDestination"`
	Evidence            EvidencePolicy         `json:"evidence"`
	IdempotencyKey      string                 `json:"idempotencyKey"`
}

// TrustedJobSubmissionValidationContext holds coordinator-authenticated facts
// used to admit a submission. It is deliberately not serializable input.
type TrustedJobSubmissionValidationContext struct {
	Scope                       ValidationContext
	ExpectedScopeDigest         string
	ExpectedCandidateSHA        string
	ExpectedCallbackDestination string
	AllowedProviderModels       []ProviderModel
}

// TrustedJobSubmissionReceipt records admission only. Admitted does not attest
// that a job ran, that execution started, or that any workflow state advanced.
type TrustedJobSubmissionReceipt struct {
	APIVersion          string                `json:"apiVersion"`
	Kind                string                `json:"kind"`
	Correlation         TrustedJobCorrelation `json:"correlation"`
	Job                 JobIdentity           `json:"job"`
	Workspace           WorkspaceIdentity     `json:"workspace"`
	SubmissionDigest    string                `json:"submissionDigest"`
	ScopeDigest         string                `json:"scopeDigest"`
	Provider            string                `json:"provider"`
	Model               string                `json:"model"`
	CallbackDestination string                `json:"callbackDestination"`
	IdempotencyKey      string                `json:"idempotencyKey"`
	Status              TrustedJobStatus      `json:"status"`
	Admitted            bool                  `json:"admitted"`
	Blocking            bool                  `json:"blocking"`
	Violations          []TrustedJobViolation `json:"violations"`
}

// TrustedJobSubmissionAdapter is the outbound seam for a trusted-job system.
// An implementation admits a submission; it must not be interpreted as a job
// launcher or executor.
type TrustedJobSubmissionAdapter interface {
	AdmitTrustedJobSubmission(context.Context, TrustedJobSubmission, TrustedJobSubmissionValidationContext) (TrustedJobSubmissionReceipt, error)
}

// TrustedJobSubmissionValidator is the contract-only adapter implementation.
type TrustedJobSubmissionValidator struct{}

// AdmitTrustedJobSubmission validates an outbound submission against
// coordinator-authenticated facts. A rejected submission is represented by a
// blocking receipt; operational errors such as cancellation are returned.
func (TrustedJobSubmissionValidator) AdmitTrustedJobSubmission(ctx context.Context, submission TrustedJobSubmission, validation TrustedJobSubmissionValidationContext) (TrustedJobSubmissionReceipt, error) {
	if err := ctx.Err(); err != nil {
		return TrustedJobSubmissionReceipt{}, err
	}
	return ValidateTrustedJobSubmission(submission, validation), nil
}
