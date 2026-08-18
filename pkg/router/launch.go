package router

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/classify"
	"github.com/Kampe/Herdforge/pkg/posture"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// Role is the launch role for a LaunchDecision. Worker and reviewer are both
// fully routed (model + effort); neither may inherit harness defaults.
type Role string

const (
	RoleWorker           Role = "worker"
	RoleReviewer         Role = "reviewer"
	RoleForgeSmith       Role = "forge-smith"
	RoleAssayer          Role = "assayer"
	RoleRecovery         Role = "recovery"
	RoleOrchestrator     Role = "orchestrator"
	RoleScoutPlanner     Role = "scout-planner"
	RoleVerificationGate Role = "verification-gate"
	RoleReviewSupervisor Role = "review-supervisor"
	RoleHarvest          Role = "harvest"
	RoleRecoverySentinel Role = "recovery-sentinel"
)

// CapabilityTier is the coarse model capability class used for coherence
// checks (flash author must not pair with a frontier-high reviewer).
type CapabilityTier string

const (
	CapFlash    CapabilityTier = "flash"
	CapStandard CapabilityTier = "standard"
	CapFrontier CapabilityTier = "frontier"
	CapUnknown  CapabilityTier = "unknown"
)

// Launch scopes distinguish a standing lane identity from a concrete task
// assignment. Scope is part of the public integrity digest; it is not inferred
// from the shape of a task reference.
const (
	ScopeGeneric   = ""
	ScopeLane      = "lane"
	ScopeTask      = "task"
	ScopeCandidate = "candidate"
)

// LaunchRequest is the input to SurfaceRouter.Decide. All policy fields that
// affect effort or coherence must be set by the caller; missing probe proof
// for probe-gated models fails closed.
type LaunchRequest struct {
	// Role selects worker vs reviewer policy (effort ladder, family gates).
	Role Role
	// NativeRole is the canonical policy role for a repository-defined standing
	// role. When set, it is validated and used for routing while Role preserves
	// the configured lane label for provenance.
	NativeRole Role
	// Shape is the herd-route task shape (implementation, qa, adversarial, …).
	// Empty defaults: worker/forge-smith → implementation; reviewer/assayer → qa.
	Shape string
	// Risk is FAC-80 classify evidence (R0–R3). Empty fails closed for reviewer
	// high-effort escalation and is treated as unknown for coherence notes.
	Risk classify.Tier
	// AuthorFamily is required for reviewer decisions (must be non-empty and
	// must differ from the selected reviewer family).
	AuthorFamily string
	// AuthorModel / AuthorCapability classify the author for flash↔frontier
	// coherence. If AuthorModel is set, CapabilityOf(AuthorModel) wins.
	AuthorModel      string
	AuthorCapability CapabilityTier
	CandidateSHA     string
	// FinalPass marks final verification of a candidate SHA.
	FinalPass bool
	// Critical marks security, concurrency, auth, infrastructure, or
	// money-touching work that may escalate review effort.
	Critical bool
	// SmallDelta marks a small additive re-review; high effort is forbidden
	// unless RiskChanged is also true with critical/final context.
	SmallDelta bool
	// RiskChanged is true when classify evidence changed since the last review.
	RiskChanged bool
	// RequestedProvider pins a single provider (tests / operator override).
	// HARD: it narrows the waterfall to exactly this surface, so an exhausted
	// pool yields no candidates at all. Never feed lane config through it —
	// use PreferredProvider.
	RequestedProvider string
	// PreferredProvider / PreferredModel are SOFT lane preferences. They bias
	// ranking toward the configured surface without removing any candidate, so
	// a healthy lane launches on what the operator configured and an exhausted
	// one still reroutes instead of failing closed.
	PreferredProvider string
	PreferredModel    string
	// RequestedModel pins the configured model when a lane policy requires it.
	RequestedModel  string
	RequestedEffort string
	TaskRef         string
	LeaseGeneration int64
	Scope           string
	// ExcludedFamily is an extra family filter (reviewers also exclude AuthorFamily).
	ExcludedFamily string
	// ProbeResults maps ProbeKey(provider, model) → PASS. Missing keys for
	// probe-gated models fail closed (unknown is not a pass).
	ProbeResults map[string]bool
	// StrictQuota when true treats missing quota ledger rows as unavailable
	// (fail closed). Default false keeps unmetered surfaces eligible for
	// local/dev; production launch (FAC-139) should set StrictQuota.
	StrictQuota bool
}

// LaunchDecision is the complete, auditable routing decision for a worker or
// reviewer launch. FAC-139 consumes this at every launch boundary; no field
// may be left to harness defaults.
type LaunchDecision struct {
	Provider        string         `json:"provider"`
	Model           string         `json:"model,omitempty"`
	Harness         string         `json:"harness"`
	HarnessArgv     []string       `json:"harness_argv,omitempty"`
	HarnessSession  string         `json:"harness_session,omitempty"`
	Effort          string         `json:"effort"`
	Pool            string         `json:"quota_pool"`
	Role            Role           `json:"role"`
	Shape           string         `json:"task_shape"`
	CandidateSHA    string         `json:"candidate_sha,omitempty"`
	Risk            classify.Tier  `json:"risk,omitempty"`
	Family          string         `json:"family"`
	CapabilityTier  CapabilityTier `json:"capability_tier"`
	ProbeKey        string         `json:"probe_key,omitempty"`
	ProbeRequired   bool           `json:"probe_required"`
	Rationale       string         `json:"rationale"`
	Availability    string         `json:"availability,omitempty"`
	QuotaPressure   int            `json:"quota_pressure"`
	Score           int            `json:"score"`
	LazerLastResort bool           `json:"lazer_last_resort"`
	Argv            []string       `json:"argv,omitempty"`
	TaskRef         string         `json:"task_ref,omitempty"`
	LeaseGeneration int64          `json:"lease_generation,omitempty"`
	Scope           string         `json:"scope,omitempty"`
	Proof           string         `json:"proof"`
	issuanceToken   [32]byte
}

const decisionProofDomain = "herdforge-fac-175-launch-decision-v1"

// Soft-preference weights. Large enough that a configured lane wins among
// comparably healthy surfaces, small enough that real quota pressure or a
// task-fit penalty still moves the launch elsewhere.
// A configured lane outranks candidates up to this many waterfall positions
// ahead of it, on any shape. Beyond that, task fit legitimately wins.
const preferProviderPositions = 3

// WorkerShape is the only thing fixed about a builder launch: builders do
// implementation work. Provider/model/effort come from the live quota-ranked
// waterfall, never a compile-time vendor tuple.
//
// This replaced codex/gpt-5.6-luna/medium literals that had been spelled out
// in three places (two here, one in cmd/herd). The pin defeated the router it
// sat on top of: chainseer's bin/herd-route has no worker tuple gate at all.
const WorkerShape = "implementation"

// KnownRoutableModel reports whether a model is one the router can actually
// produce, i.e. it appears in the ModelFor catalog for some provider/shape.
//
// Expressible preferences made this necessary. Before, lane.Model only had to
// MATCH what ModelFor produced, so a typo surfaced as loud drift and the launch
// still used a real model. Now the preference BECOMES the launched model, and
// CapabilityOf falls through to CapStandard for anything containing "claude",
// "gpt" or "deepseek" — so "claude-sonet-5" passes every gate and gets launched.
// Worse is a typo naming a different REAL model, which fails nowhere at all.
func KnownRoutableModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	for _, shape := range AllShapes() {
		providers, err := Waterfall(shape)
		if err != nil {
			continue
		}
		for _, p := range providers {
			if strings.EqualFold(ModelFor(p, shape), m) {
				return true
			}
		}
	}
	for _, fallback := range knownFallbackModels {
		if strings.EqualFold(fallback, m) {
			return true
		}
	}
	return false
}

// knownFallbackModels are substitution targets that never appear as a primary
// ModelFor result but are reachable through pool fallbacks.
var knownFallbackModels = []string{"gpt-5.3-codex-spark", "claude-sonnet-5", "claude-haiku-4-5"}

// coordinatorOnlyMarkers identify models reserved to the coordinator. Matched
// by SUBSTRING, following the CapabilityOf convention: an exact-match map was
// bypassed outright by the proxied spelling `litellm/lazer/claude-fable-5`,
// which is a real routable model.
var coordinatorOnlyMarkers = []string{"fable"}

// AuthoringModelAllowed reports whether a model may author OR review code.
// Fable is the coordinator surface and a standing operator prohibition covers
// both roles; the old codex vendor pin had been enforcing it by accident.
func AuthoringModelAllowed(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, marker := range coordinatorOnlyMarkers {
		if strings.Contains(m, marker) {
			return false
		}
	}
	return true
}

// BuilderModelAllowed is retained for callers that only mean the builder case.
func BuilderModelAllowed(model string) bool { return AuthoringModelAllowed(model) }

// authoringRole reports roles that produce or certify code, and so may never
// run a coordinator-only model.
func authoringRole(role Role) bool {
	switch role {
	case RoleWorker, RoleForgeSmith, RoleRecovery, RoleReviewer, RoleAssayer,
		RoleVerificationGate, RoleReviewSupervisor, RoleHarvest:
		return true
	}
	return false
}

var ErrWorkerPolicy = errors.New("launch.policy.worker_tuple_mismatch")
var ErrRolePolicy = errors.New("launch.policy.unknown_role")

func authoringVerb(role Role) string {
	if role == RoleReviewer || role == RoleAssayer {
		return "review"
	}
	return "build"
}

// KnownRole reports whether role has a native Herdforge launch policy.
func KnownRole(role Role) bool {
	switch role {
	case RoleWorker, RoleForgeSmith, RoleRecovery, RoleReviewer, RoleAssayer,
		RoleOrchestrator, RoleScoutPlanner, RoleVerificationGate, RoleReviewSupervisor,
		RoleHarvest, RoleRecoverySentinel:
		return true
	default:
		return false
	}
}

func knownRole(role Role) bool { return KnownRole(role) }

func decisionProof(d LaunchDecision) string {
	norm := func(v string) string {
		v = strings.ToLower(strings.TrimSpace(v))
		for _, prefix := range []string{"codex/", "openai/", "litellm/codex/", "litellm/openai/"} {
			v = strings.TrimPrefix(v, prefix)
		}
		return v
	}
	canonical := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%d|%s|%s|%s|%s|%s|%s|%s", decisionProofDomain, norm(string(d.Role)), norm(d.Shape), norm(d.Provider), norm(d.Model), norm(d.Harness), norm(d.Effort), d.CandidateSHA, d.LeaseGeneration, d.TaskRef, norm(d.Scope), d.ProbeKey, d.Rationale, d.HarnessSession, strings.Join(d.Argv, "\x00"), strings.Join(d.HarnessArgv, "\x00"))
	sum := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// VerifyDecision proves that Decide issued the decision and that its signed
// canonical fields have not been edited.
func VerifyDecision(d *LaunchDecision, taskRef string, leaseGeneration int64) error {
	if d == nil {
		return fmt.Errorf("missing router-issued launch proof")
	}
	return VerifyDecisionForScope(d, taskRef, leaseGeneration, d.Scope)
}

// VerifyDecisionForScope verifies both the router capability and the caller's
// declared identity scope. Task assignments always require a positive,
// durable lease generation; standing lanes intentionally remain generation 0.
func VerifyDecisionForScope(d *LaunchDecision, taskRef string, leaseGeneration int64, scope string) error {
	if d == nil || d.Proof == "" {
		return fmt.Errorf("missing router-issued launch proof")
	}
	if d.issuanceToken == ([32]byte{}) {
		return fmt.Errorf("missing router issuance capability")
	}
	if d.Scope != scope {
		return fmt.Errorf("launch proof scope mismatch")
	}
	switch scope {
	case ScopeTask:
		if leaseGeneration <= 0 {
			return fmt.Errorf("task launch requires a positive lease generation")
		}
	case ScopeLane:
		if leaseGeneration != 0 {
			return fmt.Errorf("lane launch cannot carry a task lease generation")
		}
	case ScopeCandidate:
		if strings.TrimSpace(taskRef) == "" || leaseGeneration < 0 {
			return fmt.Errorf("candidate launch requires a valid candidate context")
		}
	case ScopeGeneric:
		if taskRef != "" || leaseGeneration != 0 {
			return fmt.Errorf("generic launch cannot carry task context")
		}
	default:
		return fmt.Errorf("unknown launch scope %q", scope)
	}
	if d.TaskRef != "" || taskRef != "" {
		if d.TaskRef == "" || taskRef == "" || d.TaskRef != taskRef || d.LeaseGeneration != leaseGeneration {
			return fmt.Errorf("launch proof task/lease context mismatch")
		}
	}
	if decisionProof(*d) != d.Proof {
		return fmt.Errorf("launch decision proof mismatch")
	}
	return nil
}

// RebindDecision issues a fresh router capability after a caller learns the
// concrete task and durable lease generation. The public proof remains an
// integrity digest; the unexported issuance capability proves this decision
// came through the router rather than being reconstructed from public fields.
func RebindDecision(d *LaunchDecision, taskRef string, leaseGeneration int64) (*LaunchDecision, error) {
	if d == nil {
		return nil, fmt.Errorf("launch decision is required")
	}
	if err := VerifyDecision(d, d.TaskRef, d.LeaseGeneration); err != nil {
		return nil, err
	}
	if strings.TrimSpace(taskRef) == "" {
		return nil, fmt.Errorf("launch decision task context is required")
	}
	if leaseGeneration <= 0 {
		return nil, fmt.Errorf("task assignment requires a positive lease generation")
	}
	bound := *d
	bound.TaskRef = taskRef
	bound.LeaseGeneration = leaseGeneration
	bound.Scope = ScopeTask
	if _, err := cryptorand.Read(bound.issuanceToken[:]); err != nil {
		return nil, fmt.Errorf("issue rebound launch capability: %w", err)
	}
	bound.Proof = decisionProof(bound)
	return &bound, nil
}

func BindHarnessSession(d *LaunchDecision, sessionPath string) (*LaunchDecision, error) {
	if d == nil {
		return nil, fmt.Errorf("launch decision is required")
	}
	if err := VerifyDecision(d, d.TaskRef, d.LeaseGeneration); err != nil {
		return nil, err
	}
	if d.Harness != PiHarness {
		return nil, fmt.Errorf("only Pi harness decisions can bind a session")
	}
	if d.HarnessSession != "" {
		return nil, fmt.Errorf("launch decision already has a harness session")
	}
	sessionPath = filepath.Clean(strings.TrimSpace(sessionPath))
	if sessionPath == "." || !filepath.IsAbs(sessionPath) {
		return nil, fmt.Errorf("Pi harness session path must be absolute")
	}
	_, baseArgv, err := HarnessArgvFor(d.Provider, d.Model, d.Effort)
	if err != nil {
		return nil, err
	}
	if len(d.HarnessArgv) != len(baseArgv) {
		return nil, fmt.Errorf("launch decision does not contain base Pi harness argv")
	}
	for i := range baseArgv {
		if d.HarnessArgv[i] != baseArgv[i] {
			return nil, fmt.Errorf("launch decision does not contain base Pi harness argv")
		}
	}
	bound := *d
	bound.HarnessSession = sessionPath
	bound.HarnessArgv = append(append([]string(nil), baseArgv...), "--session", sessionPath)
	if _, err := cryptorand.Read(bound.issuanceToken[:]); err != nil {
		return nil, fmt.Errorf("issue session-bound launch capability: %w", err)
	}
	bound.Proof = decisionProof(bound)
	return &bound, nil
}

// BindVendorHarness re-issues a router decision for a lane that explicitly
// declares a direct vendor harness. It leaves the router's default Pi route
// untouched for callers without an authoritative lane configuration.
func BindVendorHarness(d *LaunchDecision, harness string) (*LaunchDecision, error) {
	if d == nil {
		return nil, fmt.Errorf("launch decision is required")
	}
	if err := VerifyDecision(d, d.TaskRef, d.LeaseGeneration); err != nil {
		return nil, err
	}
	harness = strings.ToLower(strings.TrimSpace(harness))
	provider := strings.ToLower(strings.TrimSpace(d.Provider))
	if !IsVendorHarness(harness) || harness != provider {
		return nil, fmt.Errorf("configured vendor harness %q must match routed provider %q", harness, d.Provider)
	}
	argv := ArgvFor(provider, d.Model, d.Effort)
	if len(argv) == 0 || argv[0] != harness {
		return nil, fmt.Errorf("no direct harness argv contract for %s/%s", provider, d.Model)
	}
	bound := *d
	bound.Harness = harness
	bound.HarnessArgv = append([]string(nil), argv...)
	bound.HarnessSession = ""
	if _, err := cryptorand.Read(bound.issuanceToken[:]); err != nil {
		return nil, fmt.Errorf("issue vendor-harness launch capability: %w", err)
	}
	bound.Proof = decisionProof(bound)
	return &bound, nil
}

// ProbeKey returns the stable probe identity for a provider/model tuple.
// FAC-139 binds tool-probe receipts to this key.
func ProbeKey(provider, model string) string {
	return provider + "|" + model
}

// CapabilityOf maps a model id to a coarse capability tier.
// Unknown/empty models return CapUnknown (fail closed at decision time).
func CapabilityOf(model string) CapabilityTier {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return CapUnknown
	}
	switch {
	case strings.Contains(m, "flash"),
		strings.Contains(m, "haiku"),
		strings.Contains(m, "spark"),
		strings.Contains(m, "qwen3.5"),
		strings.Contains(m, "qwen-3.5"):
		return CapFlash
	case strings.Contains(m, "opus"),
		strings.Contains(m, "fable"),
		strings.Contains(m, "terra"),
		strings.Contains(m, "-sol"),
		strings.Contains(m, "gpt-5.6-sol"),
		// premium frontier codex coordinator models
		strings.HasSuffix(m, "sol"):
		return CapFrontier
	case strings.Contains(m, "luna"),
		strings.Contains(m, "sonnet"),
		strings.Contains(m, "grok"),
		strings.Contains(m, "deepseek-v4-pro"),
		strings.Contains(m, "glm"),
		strings.Contains(m, "kimi"),
		strings.Contains(m, "gemini"):
		return CapStandard
	default:
		// Recognizable catalog fragments still standard; pure unknown fails closed.
		if strings.Contains(m, "gpt") || strings.Contains(m, "claude") || strings.Contains(m, "deepseek") {
			return CapStandard
		}
		return CapUnknown
	}
}

// CapabilityOfSurface applies the provider's canonical default capability
// when its CLI intentionally owns model selection (currently Kimi).
func CapabilityOfSurface(provider, model string) CapabilityTier {
	if strings.TrimSpace(model) == "" && ModelOptional(provider) {
		return CapFrontier
	}
	return CapabilityOf(model)
}

// ModelRequiresProbe reports whether a model may only launch after a current
// tool-probe PASS. Luna is always gated; deepseek write surfaces are gated.
func ModelRequiresProbe(model string) bool {
	m := strings.ToLower(model)
	if m == "" {
		return false
	}
	if strings.Contains(m, "luna") {
		return true
	}
	// DeepSeek catalog models require probe before write-capable use.
	if strings.Contains(m, "deepseek") {
		return true
	}
	return false
}

// IsDeepSeekV4 reports whether a deepseek model is the allowed v4 line.
// Non-v4 deepseek is forbidden fleet-wide.
func IsDeepSeekV4(model string) bool {
	m := strings.ToLower(model)
	if !strings.Contains(m, "deepseek") {
		return true // not deepseek → N/A, treated as allowed by this gate
	}
	return strings.Contains(m, "deepseek-v4") || strings.Contains(m, "deepseek/v4")
}

// ForbiddenDeepSeek rejects non-v4 deepseek models.
func ForbiddenDeepSeek(model string) bool {
	m := strings.ToLower(model)
	if !strings.Contains(m, "deepseek") {
		return false
	}
	return !IsDeepSeekV4(model)
}

// FlashFrontierHighForbidden is true when a flash-tier author would be paired
// with a frontier reviewer at high (or above) effort — an incoherent cost loop.
func FlashFrontierHighForbidden(authorCap CapabilityTier, reviewerCap CapabilityTier, effort string) bool {
	if authorCap != CapFlash {
		return false
	}
	if reviewerCap != CapFrontier {
		return false
	}
	switch effort {
	case "high", "xhigh", "max":
		return true
	}
	return false
}

// EffortForRequest returns the policy effort for a launch. Reviewers default
// to medium; high is allowed only for final-pass or critical R2/R3 work.
// Small-delta re-reviews stay medium unless risk classification changed into
// a critical/final path. Workers use the shape ladder (EffortFor).
func EffortForRequest(req LaunchRequest) string {
	switch req.Role {
	case RoleReviewer, RoleAssayer:
		return reviewerEffort(req)
	case RoleWorker, RoleForgeSmith, RoleRecovery:
		if requested := strings.ToLower(strings.TrimSpace(req.RequestedEffort)); requested != "" {
			return requested
		}
		// Shape ladder, same as chainseer effort_for. This used to hard-return
		// "medium" for implementation, which no routed effort could satisfy.
		if req.Shape == "" {
			return EffortFor(WorkerShape)
		}
		return EffortFor(req.Shape)
	default:
		shape := req.Shape
		if shape == "" {
			shape = defaultShapeForRole(req.Role)
		}
		return EffortFor(shape)
	}
}

func reviewerEffort(req LaunchRequest) string {
	// Small additive re-review cannot escalate unless risk changed.
	if req.SmallDelta && !req.RiskChanged {
		return "medium"
	}
	// High only: final verification OR critical security/auth/infra/money paths
	// on R2/R3 (or explicit Critical flag with R2/R3 evidence).
	if req.FinalPass || req.Critical {
		switch req.Risk {
		case classify.TierR2, classify.TierR3:
			return "high"
		case classify.TierR0, classify.TierR1:
			// Final pass on low risk stays medium unless Critical is also set
			// with R2/R3 — Critical alone on R0/R1 does not escalate.
			if req.Critical && req.Risk == classify.TierR3 {
				return "high"
			}
			return "medium"
		default:
			// Unknown risk fails closed: no silent high escalation.
			return "medium"
		}
	}
	return "medium"
}

func defaultShapeForRole(role Role) string {
	switch role {
	case RoleReviewer, RoleAssayer:
		return "qa"
	case RoleForgeSmith:
		return "implementation"
	case RoleWorker, RoleRecovery:
		return "implementation"
	default:
		return "coordinator"
	}
}

// authorCapability resolves the author tier from request fields.
func authorCapability(req LaunchRequest) CapabilityTier {
	if req.AuthorModel != "" {
		return CapabilityOf(req.AuthorModel)
	}
	if req.AuthorCapability != "" {
		return req.AuthorCapability
	}
	return CapUnknown
}

// weeklyAtOrOverCap reports whether the binding or any nested window is the
// weekly window at/over the exhausted threshold (or class exhausted).
func weeklyAtOrOverCap(st usage.BurnState) bool {
	if isWeeklyCap(st) {
		return true
	}
	for _, w := range st.Windows {
		if isWeeklyCap(w) {
			return true
		}
	}
	// Nested pool rows may carry weekly windows.
	for _, p := range st.Pools {
		if isWeeklyCap(p) {
			return true
		}
		for _, w := range p.Windows {
			if isWeeklyCap(w) {
				return true
			}
		}
	}
	return false
}

func isWeeklyCap(st usage.BurnState) bool {
	if st.Window != "weekly" && st.WindowSeconds != usage.WindowWeekly {
		return false
	}
	if st.Class == usage.BurnExhausted {
		return true
	}
	if st.Used >= usage.DefaultExhaustedPct {
		return true
	}
	if !st.Available && strings.Contains(strings.ToLower(st.Reason), "weekly") {
		return true
	}
	return false
}

// effectivePressure folds weekly-window pressure into the ranking signal so a
// surface near its weekly cap loses to a healthier compatible alternative.
func effectivePressure(st usage.BurnState, have bool) int {
	if !have || st.Reason == "no-quota-data" {
		return 50
	}
	p := int(st.Pressure)
	bump := func(w usage.BurnState) {
		if w.Window == "weekly" || w.WindowSeconds == usage.WindowWeekly {
			wp := int(w.Pressure)
			if wp > p {
				p = wp
			}
			if isWeeklyCap(w) {
				p = 200
			}
		}
	}
	bump(st)
	for _, w := range st.Windows {
		bump(w)
	}
	return p
}

// Decide selects a complete LaunchDecision for a worker or reviewer request.
// Unlike Pick, Decide is fail-closed on unknown family, capability, forbidden
// deepseek, probe-gated models without PASS, flash↔frontier-high incoherence,
// and (when StrictQuota) missing quota ledger rows.
func (r *SurfaceRouter) Decide(req LaunchRequest) (*LaunchDecision, error) {
	if r.Probes == nil {
		r.Probes = defaultProbes()
	}
	if req.Role == "" {
		return nil, fmt.Errorf("herd-route: launch decision requires role")
	}
	if req.Scope != ScopeGeneric && req.Scope != ScopeLane && req.Scope != ScopeTask && req.Scope != ScopeCandidate {
		return nil, fmt.Errorf("herd-route: unknown launch scope %q", req.Scope)
	}
	if req.Scope == ScopeTask && req.LeaseGeneration <= 0 {
		return nil, fmt.Errorf("herd-route: task launch requires a positive lease generation")
	}
	if req.Scope == ScopeLane && req.LeaseGeneration != 0 {
		return nil, fmt.Errorf("herd-route: lane launch cannot carry a task lease generation")
	}
	if (req.Scope == ScopeLane || req.Scope == ScopeTask || req.Scope == ScopeCandidate) && strings.TrimSpace(req.TaskRef) == "" {
		return nil, fmt.Errorf("herd-route: scoped launch requires context")
	}
	if req.NativeRole != "" {
		if !KnownRole(req.NativeRole) {
			return nil, fmt.Errorf("%w: native role %s", ErrRolePolicy, req.NativeRole)
		}
		req.Role = req.NativeRole
	}
	if !knownRole(req.Role) {
		return nil, fmt.Errorf("%w: %s", ErrRolePolicy, req.Role)
	}
	// Worker/forge-smith/recovery are NOT pinned to a vendor tuple. They route
	// like every other role: the shape's Waterfall re-ranked by live quota
	// pressure. The previous codex/gpt-5.6-luna/medium pin defeated the router
	// it sat on top of and stranded the fleet whenever that one pool was spent.
	// Requested provider/model/effort from lane config are SOFT hints, exactly
	// as .herd/herd.yaml documents them.
	if req.Role == RoleWorker || req.Role == RoleForgeSmith || req.Role == RoleRecovery {
		if req.Shape != "" && req.Shape != WorkerShape {
			return nil, fmt.Errorf("%w: worker/forge-smith/recovery task shape must be %s, got %q", ErrWorkerPolicy, WorkerShape, req.Shape)
		}
	}

	shape := req.Shape
	if shape == "" {
		shape = defaultShapeForRole(req.Role)
	}
	if !validShapes[shape] {
		return nil, fmt.Errorf("herd-route: unknown task shape: %s", shape)
	}

	effort := EffortForRequest(req)
	if !effortValid(effort) {
		return nil, fmt.Errorf("herd-route: invalid effort %q", effort)
	}

	excluded := req.ExcludedFamily
	isReviewer := req.Role == RoleReviewer || req.Role == RoleAssayer
	if isReviewer {
		if strings.TrimSpace(req.AuthorFamily) == "" {
			return nil, fmt.Errorf("herd-route: reviewer decision requires author_family")
		}
		// Author family is always excluded for reviewer independence.
		if excluded == "" {
			excluded = req.AuthorFamily
		}
	}

	authCap := authorCapability(req)

	// Build candidate list (same waterfall / family posture as Pick).
	mode, modeErr := familyPostureMode()
	if modeErr != nil {
		return nil, fmt.Errorf("herd-route: family posture: %w", modeErr)
	}
	candidates, err := r.candidateProviders(mode, shape, req.RequestedProvider)
	if err != nil {
		return nil, err
	}
	for _, provider := range candidates {
		if !IsLaneLaunchable(provider) {
			if req.RequestedProvider != "" {
				return nil, fmt.Errorf("herd-route: provider %q is not launchable as a Herdr lane; choose a supported harness (codex, claude, grok, agy, or opencode)", provider)
			}
		}
	}
	launchable := candidates[:0]
	for _, provider := range candidates {
		if IsLaneLaunchable(provider) {
			launchable = append(launchable, provider)
		}
	}
	if len(launchable) == 0 {
		return nil, fmt.Errorf("herd-route: no launchable Herdr surface for shape %q", shape)
	}
	candidates = launchable

	fitWeight := fitWeightFor(shape)
	floor := pressureFloor()

	type scored struct {
		rank     int
		provider string
		model    string
		pressure int
		detail   string
		family   string
		cap      CapabilityTier
		pool     string
		probeKey string
		probeReq bool
	}
	var picks []scored
	modelOverride := map[string]string{}
	var launchabilityFailure string

	for pref, provider := range candidates {
		model := ModelFor(provider, shape)
		if req.RequestedModel != "" {
			model = req.RequestedModel
		} else if req.PreferredModel != "" && strings.EqualFold(provider, req.PreferredProvider) &&
			KnownRoutableModel(req.PreferredModel) {
			// A soft preference has to be EXPRESSIBLE, not merely rankable. A
			// rank bonus alone gave a lane its configured model only when that
			// model already was the waterfall head, so six of nine lanes still
			// launched on something the operator never chose — including the
			// Sonnet->Opus reviewer escalation this was meant to fix. The
			// preferred model is a candidate like any other from here: it still
			// passes the availability, family, capability, probe and
			// builder-reserved gates below, and a candidate that fails one is
			// dropped exactly as ModelFor's would be.
			model = req.PreferredModel
		}
		// Spark / AGY fallbacks reuse Pick's available() + overrides.
		family := FamilyFor(provider, model)
		if ok, _ := posture.Allow(mode, provider, model, family); !ok {
			continue
		}
		pool := QuotaPoolFor(provider, model)
		ok, detail := r.available(provider, model, pool)

		if !ok && req.RequestedModel == "" && provider == "codex" && model != "gpt-5.3-codex-spark" &&
			strings.Contains(detail, "exhausted") {
			if ok2, d2 := r.available("codex", "gpt-5.3-codex-spark", "spark"); ok2 {
				model = "gpt-5.3-codex-spark"
				modelOverride["codex"] = model
				pool = "spark"
				family = FamilyFor(provider, model)
				ok, detail = true, d2
			}
		}
		if !ok && req.RequestedModel == "" && provider == "agy" && !strings.HasPrefix(strings.ToLower(model), "gemini") &&
			(strings.Contains(detail, "exhausted") || strings.Contains(strings.ToLower(detail), "quota")) {
			if fb := AgyGeminiPoolFallback(shape); fb != "" {
				if ok2, d2 := r.available("agy", fb, "gemini"); ok2 {
					model = fb
					modelOverride["agy"] = fb
					pool = "gemini"
					family = FamilyFor(provider, model)
					ok, detail = true, d2
				}
			}
		}
		if !ok {
			continue
		}
		if r.Probes != nil && r.Probes.Launchable != nil {
			if launchable, reason := r.Probes.Launchable(provider, model); !launchable {
				launchabilityFailure = fmt.Sprintf("%s/%s: %s", provider, model, reason)
				continue
			}
		}

		// Weekly-cap hard skip: lose to a healthy compatible alternative.
		st, haveQuota := r.quotaState(provider, pool)
		if haveQuota && weeklyAtOrOverCap(st) {
			continue
		}
		if req.StrictQuota && !haveQuota {
			continue
		}

		// Family gates.
		if family == "" {
			continue // unknown family fails closed
		}
		if excluded != "" && family == excluded {
			continue
		}
		if isReviewer && family == req.AuthorFamily {
			continue
		}

		// Model policy gates.
		if ForbiddenDeepSeek(model) {
			continue
		}
		cap := CapabilityOfSurface(provider, model)
		if cap == CapUnknown {
			continue
		}
		if model == "" && !modelDefaultSurface(provider) {
			continue
		}

		probeReq := ModelRequiresProbe(model)
		probeKey := ""
		if probeReq {
			probeKey = ProbeKey(provider, model)
			pass, known := false, false
			if req.ProbeResults != nil {
				pass, known = req.ProbeResults[probeKey]
			}
			if !known || !pass {
				// Unknown or failed probe → fail closed for this candidate.
				continue
			}
		}

		// Flash author + frontier-high reviewer is incoherent.
		if isReviewer && FlashFrontierHighForbidden(authCap, cap, effort) {
			continue
		}

		// Workers default to capable non-flash tiers when alternatives exist;
		// flash workers remain eligible when they are the only healthy pick
		// and (if probe-gated) have PASS. Preference is encoded in rank.
		pressure := effectivePressure(st, haveQuota)

		fit := pref
		if shape == "coordinator" {
			fit = 0
		}
		// Prefer non-flash for workers when pressure-comparable.
		flashPenalty := 0
		if !isReviewer && cap == CapFlash {
			flashPenalty = 15 * fitWeight // soft demotion, not a hard ban
		}
		// A lane's configured provider/model is a SOFT preference: it wins ties
		// and near-ties so a healthy lane launches on what the operator
		// configured, but it never narrows the candidate set the way the hard
		// RequestedProvider channel does. Feeding lane config through the hard
		// channel meant an exhausted preferred pool left ZERO candidates and
		// the lane could not launch at all; dropping it entirely meant every
		// lane silently launched on the waterfall head instead (reviewer, harvest
		// and orchestrator lanes all moved off their configured models).
		// Proportional to fitWeight so the bonus means the same number of
		// waterfall positions on every shape. An absolute value cleared two
		// positions on implementation and none at all on qa (fitWeight 60),
		// which made the assayer's preference undeclinable-by-config.
		preferBonus := 0
		if req.PreferredProvider != "" && strings.EqualFold(provider, req.PreferredProvider) {
			preferBonus -= preferProviderPositions * fitWeight * 100
			if req.PreferredModel != "" && strings.EqualFold(model, req.PreferredModel) {
				preferBonus -= fitWeight * 100
			}
		}
		flashPenalty += preferBonus
		var rank int
		switch {
		case shape == "coordinator" && haveQuota && st.ExhaustsBeforeReset != nil && *st.ExhaustsBeforeReset && st.RunwayMinutes != nil:
			runway := *st.RunwayMinutes
			if runway > 100000 {
				runway = 100000
			}
			rank = (100000-runway)*100 + pref + flashPenalty
		case shape == "coordinator" && haveQuota && st.ExhaustsBeforeReset != nil && !*st.ExhaustsBeforeReset:
			rank = pressure*100 + pref + flashPenalty
		case shape == "coordinator":
			rank = 20000000 + pressure*100 + pref + flashPenalty
		default:
			effective := pressure
			if effective < floor {
				effective = 0
			}
			rank = effective*100 + fit*fitWeight*100 + pref + flashPenalty
		}
		picks = append(picks, scored{
			rank: rank, provider: provider, model: model, pressure: pressure,
			detail: detail, family: family, cap: cap, pool: pool,
			probeKey: probeKey, probeReq: probeReq,
		})
	}

	// Lazer precedence (same as Pick). Claude-only already restricted the set.
	var nonLazer []scored
	for _, s := range picks {
		if s.provider != "lazer" {
			nonLazer = append(nonLazer, s)
		}
	}
	if len(nonLazer) > 0 {
		picks = nonLazer
	}

	if len(picks) == 0 {
		if mode == posture.ModeClaudeOnly {
			return nil, fmt.Errorf("herd-route: claude-only posture has no healthy native Claude route for role=%s shape=%s", req.Role, shape)
		}
		if mode == posture.ModeNoClaude {
			return nil, fmt.Errorf("herd-route: no-claude posture has no healthy non-Anthropic route for role=%s shape=%s", req.Role, shape)
		}
		if launchabilityFailure != "" {
			return nil, fmt.Errorf("herd-route: no launchable candidate for role=%s shape=%s: %s", req.Role, shape, launchabilityFailure)
		}
		return nil, fmt.Errorf("herd-route: no healthy launch candidate for role=%s shape=%s", req.Role, shape)
	}

	// Deterministic: lowest rank wins; equal rank → provider name ASC.
	sort.SliceStable(picks, func(i, j int) bool {
		if picks[i].rank != picks[j].rank {
			return picks[i].rank < picks[j].rank
		}
		return picks[i].provider < picks[j].provider
	})
	best := picks[0]
	model := best.model
	if ov, ok := modelOverride[best.provider]; ok {
		model = ov
		best.family = FamilyFor(best.provider, model)
		best.cap = CapabilityOfSurface(best.provider, model)
		best.pool = QuotaPoolFor(best.provider, model)
		best.probeReq = ModelRequiresProbe(model)
		if best.probeReq {
			best.probeKey = ProbeKey(best.provider, model)
		}
	}
	// No final vendor tuple for builders — herd-route (chainseer bin/herd-route)
	// has no such gate. The shape is still bound; the surface is whatever the
	// live quota ranking selected from the implementation waterfall.
	if req.Role == RoleWorker || req.Role == RoleForgeSmith || req.Role == RoleRecovery {
		if shape != WorkerShape {
			return nil, fmt.Errorf("%w: worker/forge-smith/recovery task shape must be %s, got %q", ErrWorkerPolicy, WorkerShape, shape)
		}
		// Capability guard, NOT a vendor pin. Removing the codex tuple left
		// RequestedModel able to put any catalog model on a builder, including
		// Fable — which is orchestrator-only and must never build or review.
		// This is about what a model is FOR, so it survives any reroute.
	}
	// Applies to reviewers too: the prohibition is on authoring or certifying
	// code with the coordinator surface, not on the builder role specifically.
	if authoringRole(req.Role) && !AuthoringModelAllowed(model) {
		return nil, fmt.Errorf("%w: %s is coordinator-only and may not %s", ErrWorkerPolicy, model, authoringVerb(req.Role))
	}
	// Final coherence re-check (mutation-safe).
	if isReviewer {
		if best.family == "" || best.family == req.AuthorFamily {
			return nil, fmt.Errorf("herd-route: reviewer family %q not independent of author family %q", best.family, req.AuthorFamily)
		}
		if FlashFrontierHighForbidden(authCap, best.cap, effort) {
			return nil, fmt.Errorf("herd-route: flash author + frontier-high reviewer forbidden")
		}
	}
	if best.family == "" || best.cap == CapUnknown {
		return nil, fmt.Errorf("herd-route: unknown family or capability for %s/%s", best.provider, model)
	}
	if ForbiddenDeepSeek(model) {
		return nil, fmt.Errorf("herd-route: non-v4 deepseek forbidden: %s", model)
	}

	rationale := fmt.Sprintf(
		"role=%s shape=%s risk=%s effort=%s provider=%s model=%s family=%s cap=%s pool=%s pressure=%d score=%d",
		req.Role, shape, req.Risk, effort, best.provider, model, best.family, best.cap, best.pool, best.pressure, best.rank,
	)
	if isReviewer {
		rationale += fmt.Sprintf(" author_family=%s", req.AuthorFamily)
	}
	if best.probeReq {
		rationale += " probe=required+pass"
	}
	if mode == posture.ModeClaudeOnly {
		rationale = "claude-only; " + rationale
	}
	if mode == posture.ModeNoClaude {
		rationale = "no-claude; " + rationale
	}

	harness, harnessArgv, err := HarnessArgvFor(best.provider, model, effort)
	if err != nil {
		return nil, fmt.Errorf("herd-route: Pi harness: %w", err)
	}
	d := &LaunchDecision{
		Provider:        best.provider,
		Model:           model,
		Harness:         harness,
		HarnessArgv:     harnessArgv,
		Effort:          effort,
		Pool:            best.pool,
		Role:            req.Role,
		Shape:           shape,
		CandidateSHA:    req.CandidateSHA,
		Risk:            req.Risk,
		Family:          best.family,
		CapabilityTier:  best.cap,
		ProbeKey:        best.probeKey,
		ProbeRequired:   best.probeReq,
		Rationale:       rationale,
		Availability:    best.detail,
		QuotaPressure:   best.pressure,
		Score:           best.rank,
		LazerLastResort: best.provider == "lazer",
		Argv:            ArgvFor(best.provider, model, effort),
		TaskRef:         req.TaskRef, LeaseGeneration: req.LeaseGeneration, Scope: req.Scope,
	}
	if _, err := cryptorand.Read(d.issuanceToken[:]); err != nil {
		return nil, fmt.Errorf("issue launch capability: %w", err)
	}
	d.Proof = decisionProof(*d)
	return d, nil
}

// candidateProviders is the single candidate construction for both Pick and
// Decide, including the shared family-posture filter so they cannot diverge
// (FAC-102). An explicit requestedProvider the posture forbids is a hard error:
// silently substituting native claude would record provider_pin=codex on a
// route that actually ran claude.
func (r *SurfaceRouter) candidateProviders(mode posture.Mode, shape, requestedProvider string) ([]string, error) {
	if requestedProvider != "" {
		if ok, reason := posture.Allow(mode, requestedProvider, "", ""); !ok {
			return nil, fmt.Errorf("herd-route: %s posture forbids requested provider %q: %s",
				posture.ModeLabel(mode), requestedProvider, reason)
		}
		return []string{requestedProvider}, nil
	}
	candidates, err := Waterfall(shape)
	if err != nil {
		return nil, err
	}
	if era := strings.TrimSpace(os.Getenv("HERD_ERA_PROVIDERS")); era != "" {
		eraSet := map[string]bool{}
		for _, p := range strings.Fields(era) {
			eraSet[p] = true
		}
		var kept []string
		for _, c := range candidates {
			if eraSet[c] {
				kept = append(kept, c)
			}
		}
		if len(kept) > 0 {
			candidates = kept
		}
	}
	candidates, _ = posture.FilterProviders(mode, candidates)
	if mode == posture.ModeNoClaude && len(candidates) == 0 {
		return nil, fmt.Errorf("herd-route: no-claude posture leaves NO candidate for shape %q", shape)
	}
	if mode == posture.ModeClaudeOnly && len(candidates) == 0 {
		return nil, fmt.Errorf("herd-route: claude-only posture leaves NO candidate for shape %q", shape)
	}
	return candidates, nil
}
