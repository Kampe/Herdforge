package agentscope

import "context"

const (
	// TrustedJobCallbackAPIVersion identifies the versioned callback contract.
	TrustedJobCallbackAPIVersion = "herdforge.dev/v1alpha1"
	// TrustedJobCallbackKind identifies a callback submitted by a trusted-job adapter.
	TrustedJobCallbackKind = "TrustedJobCallback"
	// TrustedJobCallbackReceiptKind identifies a validation receipt for a callback.
	TrustedJobCallbackReceiptKind = "TrustedJobCallbackReceipt"
)

// ProviderModel pins an effective model to its provider. Both values are
// policy-controlled identifiers; a model is not valid merely because it is
// allowed for a different provider.
type ProviderModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// TrustedJobCorrelation binds a callback to one fenced workflow transition.
type TrustedJobCorrelation struct {
	RunID            string `json:"runId"`
	PhaseID          string `json:"phaseId"`
	Task             string `json:"task"`
	ScopeID          string `json:"scopeId"`
	CandidateSHA     string `json:"candidateSha"`
	CallbackSequence int64  `json:"callbackSequence"`
}

// TrustedJob identifies the exact revision and runtime reported by a trusted
// job adapter. It intentionally contains no branch, ref, manifest, path, or
// credential material: those moving or secret-bearing inputs are not trusted
// job authority.
type TrustedJob struct {
	CandidateSHA   string `json:"candidateSha"`
	Provider       string `json:"provider"`
	EffectiveModel string `json:"effectiveModel"`
}

// TrustedJobEvidence binds a declared AgentScope evidence kind to a content
// digest. Evidence paths and inline content are deliberately excluded.
type TrustedJobEvidence struct {
	Kind   EvidenceKind `json:"kind"`
	Digest string       `json:"digest"`
}

// TrustedJobCallbackRequest is the strict, versioned input accepted at an
// external trusted-job callback boundary. It is a validation contract only;
// accepting it neither starts nor proves execution of a job. A callback is
// bound to the admitted submission by SubmissionDigest and CallbackDestination,
// and replay is closed by the coordinator-authenticated IdempotencyKey.
type TrustedJobCallbackRequest struct {
	APIVersion          string                `json:"apiVersion"`
	Kind                string                `json:"kind"`
	Correlation         TrustedJobCorrelation `json:"correlation"`
	Scope               AgentScope            `json:"scope"`
	ScopeDigest         string                `json:"scopeDigest"`
	SubmissionDigest    string                `json:"submissionDigest"`
	CallbackDestination string                `json:"callbackDestination"`
	IdempotencyKey      string                `json:"idempotencyKey"`
	Job                 TrustedJob            `json:"job"`
	Evidence            []TrustedJobEvidence  `json:"evidence"`
}

// TrustedJobViolationCode classifies a callback rejection without relying on
// implementation-specific error messages.
type TrustedJobViolationCode string

const (
	ViolationTrustedJobContext       TrustedJobViolationCode = "trusted_job_context"
	ViolationTrustedJobSchema        TrustedJobViolationCode = "trusted_job_schema"
	ViolationTrustedJobScope         TrustedJobViolationCode = "trusted_job_scope"
	ViolationTrustedJobPath          TrustedJobViolationCode = "trusted_job_path"
	ViolationTrustedJobCredential    TrustedJobViolationCode = "trusted_job_credential"
	ViolationTrustedJobProviderModel TrustedJobViolationCode = "trusted_job_provider_model"
	ViolationTrustedJobIdentity      TrustedJobViolationCode = "trusted_job_identity"
	ViolationTrustedJobDigest        TrustedJobViolationCode = "trusted_job_digest"
	ViolationTrustedJobCandidate     TrustedJobViolationCode = "trusted_job_candidate"
	ViolationTrustedJobEvidence      TrustedJobViolationCode = "trusted_job_evidence"
	ViolationTrustedJobSequence      TrustedJobViolationCode = "trusted_job_sequence"
	ViolationTrustedJobSubmission    TrustedJobViolationCode = "trusted_job_submission"
	ViolationTrustedJobReplay        TrustedJobViolationCode = "trusted_job_replay"

	// Outbound admission violation codes. These are produced by the
	// trusted-job submission validator and share the typed violation contract.
	ViolationTrustedJobConfig              TrustedJobViolationCode = "trusted_job_config"
	ViolationTrustedJobVirtualCredential   TrustedJobViolationCode = "trusted_job_virtual_credential"
	ViolationTrustedJobCallbackDestination TrustedJobViolationCode = "trusted_job_callback_destination"
	ViolationTrustedJobIdempotency         TrustedJobViolationCode = "trusted_job_idempotency"
	ViolationTrustedJobWorkspace           TrustedJobViolationCode = "trusted_job_workspace"
	ViolationTrustedJobStatus              TrustedJobViolationCode = "trusted_job_status"
)

// TrustedJobViolation is a blocking validation finding for a trusted-job
// callback.
type TrustedJobViolation struct {
	Code    TrustedJobViolationCode `json:"code"`
	Field   string                  `json:"field"`
	Message string                  `json:"message"`
}

// TrustedJobCallbackReceipt records contract admission only. Accepted does
// not attest that a job ran, that its evidence was consumed, or that any
// workflow state advanced. The receipt echoes the submission binding and
// idempotency key so callers can audit callback provenance.
type TrustedJobCallbackReceipt struct {
	APIVersion          string                `json:"apiVersion"`
	Kind                string                `json:"kind"`
	Correlation         TrustedJobCorrelation `json:"correlation"`
	ScopeDigest         string                `json:"scopeDigest"`
	SubmissionDigest    string                `json:"submissionDigest"`
	CallbackDestination string                `json:"callbackDestination"`
	IdempotencyKey      string                `json:"idempotencyKey"`
	Provider            string                `json:"provider"`
	EffectiveModel      string                `json:"effectiveModel"`
	Evidence            []TrustedJobEvidence  `json:"evidence"`
	Accepted            bool                  `json:"accepted"`
	Blocking            bool                  `json:"blocking"`
	Violations          []TrustedJobViolation `json:"violations"`
}

// TrustedJobCallbackValidationContext holds coordinator-authenticated facts
// used to validate a callback. It is deliberately not serializable input. The
// submission binding (ExpectedSubmissionDigest, ExpectedCallbackDestination)
// and replay guard (ExpectedIdempotencyKey, SeenKeys) close callback replay:
// a callback whose key is stale or already-seen is rejected as blocking.
type TrustedJobCallbackValidationContext struct {
	Scope                       ValidationContext
	ExpectedScopeDigest         string
	ExpectedSubmissionDigest    string
	ExpectedCandidateSHA        string
	ExpectedCallbackSequence    int64
	ExpectedCallbackDestination string
	ExpectedIdempotencyKey      string
	SeenKeys                    []string
	AllowedProviderModels       []ProviderModel
}

// TrustedJobCallbackAdapter is the inbound seam for a trusted-job system. An
// implementation validates a callback; it must not be interpreted as a job
// launcher or executor.
type TrustedJobCallbackAdapter interface {
	AcceptTrustedJobCallback(context.Context, TrustedJobCallbackRequest, TrustedJobCallbackValidationContext) (TrustedJobCallbackReceipt, error)
}

// TrustedJobCallbackValidator is the contract-only adapter implementation.
type TrustedJobCallbackValidator struct{}

// AcceptTrustedJobCallback validates an inbound callback against
// coordinator-authenticated facts. A rejected callback is represented by a
// blocking receipt; operational errors such as cancellation are returned.
func (TrustedJobCallbackValidator) AcceptTrustedJobCallback(ctx context.Context, request TrustedJobCallbackRequest, validation TrustedJobCallbackValidationContext) (TrustedJobCallbackReceipt, error) {
	if err := ctx.Err(); err != nil {
		return TrustedJobCallbackReceipt{}, err
	}
	return ValidateTrustedJobCallback(request, validation), nil
}
