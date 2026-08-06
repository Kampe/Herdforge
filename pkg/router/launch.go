package router

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/classify"
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
	RequestedProvider string
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

// The single approved worker/forge-smith/recovery launch tuple. This was
// previously spelled as codex/gpt-5.6-luna/medium literals in three places
// (two here, one in cmd/herd), which pinned the whole builder fleet to one
// vendor and made every other routed pick fail as drift. Keep it equal to
// what the live router picks for WorkerShape.
// ponytail: still a compile-time pin — derive from fleet config/router.
const (
	WorkerProvider = "grok"
	WorkerModel    = "grok-4.5"
	WorkerEffort   = "high"
	WorkerShape    = "implementation"
)

var ErrWorkerPolicy = errors.New("launch.policy.worker_tuple_mismatch")
var ErrRolePolicy = errors.New("launch.policy.unknown_role")

func knownRole(role Role) bool {
	switch role {
	case RoleWorker, RoleForgeSmith, RoleRecovery, RoleReviewer, RoleAssayer,
		RoleOrchestrator, RoleScoutPlanner, RoleVerificationGate, RoleReviewSupervisor,
		RoleHarvest, RoleRecoverySentinel:
		return true
	default:
		return false
	}
}

func decisionProof(d LaunchDecision) string {
	norm := func(v string) string {
		v = strings.ToLower(strings.TrimSpace(v))
		for _, prefix := range []string{"codex/", "openai/", "litellm/codex/", "litellm/openai/"} {
			v = strings.TrimPrefix(v, prefix)
		}
		return v
	}
	canonical := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%d|%s|%s|%s|%s|%s", decisionProofDomain, norm(string(d.Role)), norm(d.Shape), norm(d.Provider), norm(d.Model), norm(d.Effort), d.CandidateSHA, d.LeaseGeneration, d.TaskRef, norm(d.Scope), d.ProbeKey, d.Rationale, strings.Join(d.Argv, "\x00"))
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
		if req.Shape == "" || req.Shape == WorkerShape {
			return WorkerEffort
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
	if !knownRole(req.Role) {
		return nil, fmt.Errorf("%w: %s", ErrRolePolicy, req.Role)
	}
	if req.Role == RoleWorker || req.Role == RoleForgeSmith || req.Role == RoleRecovery {
		if req.Shape != WorkerShape || req.RequestedProvider != WorkerProvider || req.RequestedModel != WorkerModel || req.RequestedEffort != WorkerEffort {
			return nil, fmt.Errorf("%w: worker/forge-smith/recovery requires %s/%s/%s %s", ErrWorkerPolicy, WorkerProvider, WorkerModel, WorkerEffort, WorkerShape)
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

	// Build candidate list (same waterfall / env postures as Pick).
	candidates, err := r.candidateProviders(shape, req.RequestedProvider)
	if err != nil {
		return nil, err
	}

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

	for pref, provider := range candidates {
		model := ModelFor(provider, shape)
		if req.RequestedModel != "" {
			model = req.RequestedModel
		}
		// Spark / AGY fallbacks reuse Pick's available() + overrides.
		family := FamilyFor(provider, model)
		pool := QuotaPoolFor(provider, model)
		ok, detail := r.available(provider, model, pool)

		if !ok && provider == "codex" && model != "gpt-5.3-codex-spark" &&
			strings.Contains(detail, "exhausted") {
			if ok2, d2 := r.available("codex", "gpt-5.3-codex-spark", "spark"); ok2 {
				model = "gpt-5.3-codex-spark"
				modelOverride["codex"] = model
				pool = "spark"
				family = FamilyFor(provider, model)
				ok, detail = true, d2
			}
		}
		if !ok && provider == "agy" && !strings.HasPrefix(strings.ToLower(model), "gemini") &&
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
		cap := CapabilityOf(model)
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

	// Claude-only / lazer precedence (same as Pick).
	if envSet("HERD_CLAUDE_ONLY") {
		var cl []scored
		for _, s := range picks {
			if s.provider == "claude" {
				cl = append(cl, s)
			}
		}
		if len(cl) > 0 {
			picks = cl
		}
	}
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
		best.cap = CapabilityOf(model)
		best.pool = QuotaPoolFor(best.provider, model)
		best.probeReq = ModelRequiresProbe(model)
		if best.probeReq {
			best.probeKey = ProbeKey(best.provider, model)
		}
	}
	if req.Role == RoleWorker || req.Role == RoleForgeSmith || req.Role == RoleRecovery {
		if best.provider != WorkerProvider || model != WorkerModel || effort != WorkerEffort || shape != WorkerShape {
			return nil, fmt.Errorf("%w: worker/forge-smith/recovery final tuple must remain %s/%s/%s %s", ErrWorkerPolicy, WorkerProvider, WorkerModel, WorkerEffort, WorkerShape)
		}
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
	if envSet("HERD_CLAUDE_ONLY") {
		rationale = "claude-only; " + rationale
	}
	if envSet("HERD_NO_CLAUDE") {
		rationale = "no-claude; " + rationale
	}

	d := &LaunchDecision{
		Provider:        best.provider,
		Model:           model,
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

// candidateProviders mirrors Pick's candidate construction.
func (r *SurfaceRouter) candidateProviders(shape, requestedProvider string) ([]string, error) {
	if requestedProvider != "" {
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
	if envSet("HERD_CLAUDE_ONLY") {
		var cl, rest []string
		for _, c := range candidates {
			if c == "claude" {
				cl = append(cl, c)
			} else {
				rest = append(rest, c)
			}
		}
		if len(cl) == 0 {
			cl = []string{"claude"}
		}
		candidates = append(cl, rest...)
	}
	if envSet("HERD_NO_CLAUDE") {
		var nc []string
		for _, c := range candidates {
			if c != "claude" {
				nc = append(nc, c)
			}
		}
		if len(nc) == 0 {
			return nil, fmt.Errorf("herd-route: no-claude posture leaves NO candidate for shape %q", shape)
		}
		candidates = nc
	}
	return candidates, nil
}
