// Package resolve resolves provider-agnostic lane definitions into a concrete
// provider + model + effort at launch time, deterministically, with no LLM call.
//
// A lane in the registry declares only its JOB SHAPE (role, route_shape,
// risk_class) and an optional soft prefer, never a provider or a model.
// This package turns that definition into a launch decision by combining:
//
//  1. The declarative CONSTRAINT REGISTRY (provider_constraints + risk_classes)
//     in lane-registry.json, which encodes learned operational rules as data.
//  2. A scoring interface that the caller provides (analogous to herd-route),
//     applied over the eligible surface derived from constraints.
//
// The resolver is deterministic: given the same registry, lane id, and score
// returns, it produces the same result. It does NOT make LLM calls.
package resolve

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// DefaultAdapter wraps an external scoring function as a RouteScorer.
// This is the adapter used by the herd CLI for the live router.
type DefaultAdapter struct {
	ScoreFn func(shape string, preferProvider string) *RouteScore
}

func (a *DefaultAdapter) Score(shape string, preferProvider string) *RouteScore {
	return a.ScoreFn(shape, preferProvider)
}

// LaneRegistry is the top-level JSON structure of lane-registry.json.
type LaneRegistry struct {
	Version              int                             `json:"version"`
	ProviderConstraints  map[string]ProviderConstraint   `json:"provider_constraints"`
	RiskClasses          map[string]RiskClass            `json:"risk_classes"`
	Lanes                []LaneDef                       `json:"lanes"`
	AdvisoryModels       map[string]AdvisoryModel        `json:"advisory_models,omitempty"`
	NetworkCapability    NetworkCapability               `json:"network_capability,omitempty"`
	OutputValidation     OutputValidation                `json:"output_validation,omitempty"`
}

// ProviderConstraint encodes operational rules for a specific provider.
type ProviderConstraint struct {
	ForbidStanding *bool  `json:"forbid_standing,omitempty"`
	FloorTo        string `json:"floor_to,omitempty"`
	Reason         string `json:"reason,omitempty"`
	LastResort     *bool  `json:"last_resort,omitempty"`
	Banned         *bool  `json:"banned,omitempty"`
	ForbidBackend  string `json:"forbid_backend,omitempty"`
	HeadlessOK     *bool  `json:"headless_ok,omitempty"`
	DefaultPool    string `json:"default_pool,omitempty"`
}

// RiskClass encodes the constraint profile for a risk category.
type RiskClass struct {
	ProviderPin               string `json:"provider_pin,omitempty"`
	ModelPin                  string `json:"model_pin,omitempty"`
	EffortFloor               string `json:"effort_floor,omitempty"`
	ModelFloorClass           string `json:"model_floor_class,omitempty"`
	ByteReplayReviewOffClaude *bool  `json:"byte_replay_review_off_claude,omitempty"`
	CostTier                  string `json:"cost_tier,omitempty"`
	ProviderAllow             []string `json:"provider_allow,omitempty"`
	Reason                    string `json:"reason,omitempty"`
}

// LaneDef defines one standing lane.
type LaneDef struct {
	ID           string `json:"id"`
	Packet       string `json:"packet,omitempty"`
	Role         string `json:"role,omitempty"`
	RouteShape   string `json:"route_shape"`
	RiskClass    string `json:"risk_class"`
	Prefer       string `json:"prefer,omitempty"`
	PreferModel  string `json:"prefer_model,omitempty"`
}

// AdvisoryModel documents model capability notes.
type AdvisoryModel struct {
	Family           string `json:"family"`
	PersistentAgents *bool  `json:"persistent_agents,omitempty"`
	ToolExecution    string `json:"tool_execution,omitempty"`
	Shape            string `json:"shape,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

// NetworkCapability documents per-provider network access in execution modes.
type NetworkCapability struct {
	ShapesRequiringNetwork []string            `json:"shapes_requiring_network,omitempty"`
	Providers              map[string]NetworkProviderCap `json:"providers,omitempty"`
}

// NetworkProviderCap documents one provider's network capability.
type NetworkProviderCap struct {
	Network              *bool `json:"network,omitempty"`
	SandboxedExecNetwork *bool `json:"sandboxed_exec_network,omitempty"`
}

// OutputValidation documents shot output validation rules.
type OutputValidation struct {
	ShapesRequiringReport []string `json:"shapes_requiring_report,omitempty"`
	MinReportChars        int      `json:"min_report_chars,omitempty"`
}

// ResolvedLane is the immutable result of resolving a lane.
type ResolvedLane struct {
	Lane           string   `json:"lane"`
	RouteShape     string   `json:"route_shape"`
	RiskClass      string   `json:"risk_class"`
	Provider       string   `json:"provider,omitempty"`
	Model          string   `json:"model,omitempty"`
	Effort         string   `json:"effort"`
	CostTier       string   `json:"cost_tier"`
	ByteReplayReview bool   `json:"byte_replay_review"`
	Constraints    []string `json:"constraints"`
	Resolvable     bool     `json:"resolvable"`
	Reason         string   `json:"reason,omitempty"`
}

// RouteScore is what a routing/scoring function returns for a candidate provider.
type RouteScore struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	Credits        string `json:"credits,omitempty"`
	QuotaPressure  string `json:"quota_pressure,omitempty"`
	QuotaPool      string `json:"quota_pool,omitempty"`
	LazerLastResort bool  `json:"lazer_last_resort,omitempty"`
}

// RouteScorer is the interface the resolver calls to rank candidates.
// The caller provides this — it wraps the herd-route equivalent logic
// (live quota, task-fit, provider health).
type RouteScorer interface {
	// Score returns the best route for the given shape, optionally constrained
	// to a specific provider. Returns nil when no healthy route exists.
	Score(shape string, preferProvider string) *RouteScore
}

// LaneResolver resolves lane definitions into concrete provider+model+effort.
type LaneResolver struct {
	registry *LaneRegistry
	scorer   RouteScorer
}

// New creates a LaneResolver from a parsed registry and a RouteScorer.
func New(registry *LaneRegistry, scorer RouteScorer) *LaneResolver {
	return &LaneResolver{registry: registry, scorer: scorer}
}

// ParseRegistry decodes the JSON lane registry.
func ParseRegistry(data []byte) (*LaneRegistry, error) {
	var reg LaneRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("resolve: invalid registry JSON: %w", err)
	}
	if len(reg.Lanes) == 0 {
		return nil, fmt.Errorf("resolve: registry has no lanes")
	}
	return &reg, nil
}

// Resolve resolves a single lane.
func (r *LaneResolver) Resolve(laneID string, dropPrefer bool) *ResolvedLane {
	result := &ResolvedLane{
		Lane: laneID,
		Effort: "medium",
		Resolvable: true,
	}

	lane := r.findLane(laneID)
	if lane == nil {
		result.Resolvable = false
		result.Reason = fmt.Sprintf("unknown lane %q (not in registry)", laneID)
		return result
	}

	result.RouteShape = lane.RouteShape
	result.RiskClass = lane.RiskClass

	rc := r.registry.RiskClasses[lane.RiskClass]

	// Gather constraints from risk class
	providerPin := rc.ProviderPin
	modelPin := rc.ModelPin
	effortFloor := rc.EffortFloor
	modelFloorClass := rc.ModelFloorClass
	costTier := rc.CostTier
	if costTier == "" {
		costTier = "market"
	}
	result.CostTier = costTier

	var byteOffClaude bool
	if rc.ByteReplayReviewOffClaude != nil {
		byteOffClaude = *rc.ByteReplayReviewOffClaude
	}

	// Posture interplay
	if providerPin == "claude" && isEnvSet("HERD_NO_CLAUDE") {
		providerPin = ""
		result.Constraints = append(result.Constraints, "provider_pin=claude(dropped:no-claude)")
	}

	prefer := lane.Prefer
	preferModel := lane.PreferModel

	if prefer != "" && isEnvSet("HERD_CLAUDE_ONLY") {
		result.Constraints = append(result.Constraints, fmt.Sprintf("prefer=%s(dropped:claude-only)", prefer))
		prefer = ""
	}

	// Hard pin outranks soft preference
	if providerPin != "" {
		prefer = ""
	}

	if dropPrefer {
		prefer = ""
	}

	// Score: try soft preference first, fall back to unconstrained
	var score *RouteScore
	if prefer != "" {
		score = r.scorer.Score(lane.RouteShape, prefer)
		if score != nil && score.Provider != prefer {
			score = nil
		}
		if score != nil {
			result.Constraints = append(result.Constraints, fmt.Sprintf("prefer=%s(honored)", prefer))
		} else {
			result.Constraints = append(result.Constraints, fmt.Sprintf("prefer=%s(fell-back)", prefer))
		}
	}

	if score == nil {
		if providerPin != "" {
			score = r.scorer.Score(lane.RouteShape, providerPin)
			result.Constraints = append(result.Constraints, fmt.Sprintf("provider_pin=%s", providerPin))
		} else {
			score = r.scorer.Score(lane.RouteShape, "")
		}
	}

	if score == nil {
		result.Resolvable = false
		result.Reason = fmt.Sprintf("no healthy provider for shape=%s", lane.RouteShape)
		if providerPin != "" {
			result.Reason += fmt.Sprintf(" (pinned %s)", providerPin)
		}
		return result
	}

	result.Provider = score.Provider
	result.Model = score.Model
	result.Effort = score.Effort

	// prefer_model: substitute lane's preferred model ONLY when soft provider
	// preference was honored
	if preferModel != "" && prefer != "" && result.Provider == prefer {
		if result.Model != preferModel {
			result.Constraints = append(result.Constraints, fmt.Sprintf("prefer_model=%s(applied,was:%s)", preferModel, result.Model))
			result.Model = preferModel
		} else {
			result.Constraints = append(result.Constraints, fmt.Sprintf("prefer_model=%s(already)", preferModel))
		}
	} else if preferModel != "" {
		result.Constraints = append(result.Constraints, fmt.Sprintf("prefer_model=%s(skipped:prefer-not-honored)", preferModel))
	}

	// --- model constraints ---

	// model_pin is claude-specific; only applies when lane landed on claude
	if modelPin != "" && result.Provider == "claude" {
		result.Model = modelPin
		result.Constraints = append(result.Constraints, fmt.Sprintf("model_pin=%s", modelPin))
	}

	// Haiku forbidden for standing/autonomous lanes
	if result.Provider == "claude" && strings.Contains(strings.ToLower(result.Model), "haiku") {
		hfloor := "claude-sonnet-5"
		if pc, ok := r.registry.ProviderConstraints["claude-haiku-4-5"]; ok && pc.FloorTo != "" {
			hfloor = pc.FloorTo
		}
		result.Model = hfloor
		result.Constraints = append(result.Constraints, fmt.Sprintf("haiku_forbidden->%s", hfloor))
	}

	// Model floor class (sonnet-class floor for fixtures-first)
	if modelFloorClass != "" {
		floorRank := floorRankForClass(modelFloorClass)
		curRank := modelClassRank(result.Model)
		if curRank < floorRank {
			bumped := providerSonnetModel(result.Provider)
			if bumped != "" {
				result.Model = bumped
				result.Constraints = append(result.Constraints, fmt.Sprintf("model_floor=%s(bumped)", modelFloorClass))
			} else {
				result.Constraints = append(result.Constraints, fmt.Sprintf("model_floor=%s(unmet;flag)", modelFloorClass))
				result.ByteReplayReview = true
			}
		}
	}

	// Byte-replay review off-Claude
	if byteOffClaude && result.Provider != "claude" {
		result.ByteReplayReview = true
		result.Constraints = append(result.Constraints, fmt.Sprintf("byte_replay_review(off-claude:%s)", result.Provider))
	}

	// --- effort floor ---
	if effortFloor != "" && effortRank(result.Effort) < effortRank(effortFloor) {
		result.Effort = effortFloor
		result.Constraints = append(result.Constraints, fmt.Sprintf("effort_floor=%s", effortFloor))
	}

	// --- cost-tier ceiling (CHA-894) ---
	if costTier != "" {
		ceilRank := costTierCeilingRank(costTier)
		pickRank := modelClassRank(result.Model)
		if pickRank > ceilRank {
			demoted := providerModelWithinRank(result.Provider, ceilRank)
			var unmet string
			if modelPin != "" {
				unmet = fmt.Sprintf("model_pin=%s is explicit and outranks a cost preference", modelPin)
			} else if demoted == "" {
				unmet = fmt.Sprintf("no in-tier model mapped for provider %s", result.Provider)
			} else if modelClassRank(demoted) < floorRankForClass(modelFloorClass) {
				unmet = fmt.Sprintf("model_floor=%s wins over the ceiling", modelFloorClass)
			}
			if unmet == "" {
				result.Constraints = append(result.Constraints, fmt.Sprintf("cost_ceiling=%s(demoted:%s->%s)", costTier, result.Model, demoted))
				result.Model = demoted
			} else {
				result.Constraints = append(result.Constraints, fmt.Sprintf("cost_ceiling=%s(EXCEEDED:%s;%s)", costTier, result.Model, unmet))
			}
		}
	}

	result.CostTier = costTierForModel(result.Model)

	if len(result.Constraints) == 0 {
		result.Constraints = []string{"none"}
	}

	return result
}

// ResolveAll resolves every lane in the registry in order.
func (r *LaneResolver) ResolveAll() []*ResolvedLane {
	var results []*ResolvedLane
	for _, lane := range r.registry.Lanes {
		results = append(results, r.Resolve(lane.ID, false))
	}
	return results
}

// LaneIDs returns all lane IDs in registry order.
func (r *LaneResolver) LaneIDs() []string {
	var ids []string
	for _, lane := range r.registry.Lanes {
		ids = append(ids, lane.ID)
	}
	return ids
}

// LaneField returns a specific field from a lane definition.
func (r *LaneResolver) LaneField(laneID, field string) (string, error) {
	lane := r.findLane(laneID)
	if lane == nil {
		return "", fmt.Errorf("resolve: lane %q not found", laneID)
	}

	switch field {
	case "id":
		return lane.ID, nil
	case "packet":
		return lane.Packet, nil
	case "role":
		return lane.Role, nil
	case "route_shape":
		return lane.RouteShape, nil
	case "risk_class":
		return lane.RiskClass, nil
	case "prefer":
		return lane.Prefer, nil
	case "prefer_model":
		return lane.PreferModel, nil
	default:
		return "", fmt.Errorf("resolve: unknown field %q", field)
	}
}

// --- internal helpers ---

func (r *LaneResolver) findLane(id string) *LaneDef {
	for i := range r.registry.Lanes {
		if r.registry.Lanes[i].ID == id {
			return &r.registry.Lanes[i]
		}
	}
	return nil
}

func isEnvSet(key string) bool {
	return os.Getenv(key) != "" && os.Getenv(key) != "0"
}

// effortRank maps effort labels to numeric ranks (low=1 .. max=5).
func effortRank(effort string) int {
	switch effort {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "xhigh":
		return 4
	case "max":
		return 5
	default:
		return 2
	}
}

// modelClassRank maps a model name to its capability class:
// 1=mechanical (haiku/spark), 2=sonnet-class, 3=opus-class, 4=premium.
func modelClassRank(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "haiku") || strings.Contains(m, "spark") || strings.Contains(m, "qwen3.5"):
		return 1
	case strings.Contains(m, "opus-5") || strings.Contains(m, "fable") || strings.Contains(m, "-sol") || strings.Contains(m, "sol"):
		return 4
	case strings.Contains(m, "opus-4") || strings.Contains(m, "terra") || strings.Contains(m, "grok") || strings.Contains(m, "-pro-") || strings.Contains(m, "deepseek") || strings.Contains(m, "glm") || strings.Contains(m, "kimi"):
		return 3
	default:
		return 2
	}
}

// providerSonnetModel returns the sonnet-class representative model for a provider.
func providerSonnetModel(provider string) string {
	switch provider {
	case "claude":
		return "claude-sonnet-5"
	case "codex":
		return "gpt-5.6-luna"
	case "agy":
		return "gemini-3.6-flash-high"
	default:
		return ""
	}
}

// floorRankForClass returns the numeric rank for a model floor class.
func floorRankForClass(class string) int {
	switch class {
	case "":
		return 0
	case "sonnet":
		return 2
	case "opus":
		return 3
	default:
		return 2
	}
}

// costTierForModel returns the cost tier string for a model.
func costTierForModel(model string) string {
	switch modelClassRank(model) {
	case 1:
		return "cheap"
	case 4:
		return "premium"
	default:
		return "market"
	}
}

// costTierCeilingRank returns the max model class rank a cost tier admits.
func costTierCeilingRank(tier string) int {
	switch tier {
	case "cheap":
		return 1
	case "premium":
		return 4
	default:
		return 3
	}
}

// providerModelWithinRank returns the best model for a provider at or below a rank ceiling.
func providerModelWithinRank(provider string, ceil int) string {
	var cand string
	if ceil >= 3 {
		cand = providerSonnetModel(provider)
	} else {
		switch provider {
		case "claude":
			cand = "claude-haiku-4-5"
		case "codex":
			cand = "gpt-5.3-codex-spark"
		default:
			cand = ""
		}
	}
	if cand != "" && modelClassRank(cand) <= ceil {
		return cand
	}
	return ""
}

// RationaleLine returns a one-line auditable rationale string.
func (r *ResolvedLane) RationaleLine() string {
	constraints := strings.Join(r.Constraints, ",")
	if r.Resolvable {
		return fmt.Sprintf("resolve %s shape=%s risk=%s -> %s/%s effort=%s | constraints:%s | byte_replay=%v | cost=%s",
			r.Lane, r.RouteShape, r.RiskClass, r.Provider, r.Model, r.Effort, constraints, r.ByteReplayReview, r.CostTier)
	}
	return fmt.Sprintf("resolve %s shape=%s risk=%s -> UNROUTABLE | reason:%s | constraints:%s",
		r.Lane, r.RouteShape, r.RiskClass, r.Reason, constraints)
}

// ResolveAllJSON resolves all lanes and returns JSON output.
func (r *LaneResolver) ResolveAllJSON() (string, error) {
	results := r.ResolveAll()
	type jsonRow struct {
		Lane            string   `json:"lane"`
		RouteShape      string   `json:"route_shape"`
		RiskClass       string   `json:"risk_class"`
		Provider        *string  `json:"provider"`
		Model           *string  `json:"model"`
		Effort          string   `json:"effort"`
		CostTier        string   `json:"cost_tier"`
		ByteReplayReview bool    `json:"byte_replay_review"`
		Constraints     []string `json:"constraints"`
		Resolvable      bool     `json:"resolvable"`
		Reason          *string  `json:"reason"`
	}

	rows := make([]jsonRow, 0, len(results))
	for _, res := range results {
		row := jsonRow{
			Lane:             res.Lane,
			RouteShape:       res.RouteShape,
			RiskClass:        res.RiskClass,
			Effort:           res.Effort,
			CostTier:         res.CostTier,
			ByteReplayReview: res.ByteReplayReview,
			Constraints:      res.Constraints,
			Resolvable:       res.Resolvable,
		}
		if res.Provider != "" {
			p := res.Provider
			row.Provider = &p
		}
		if res.Model != "" {
			m := res.Model
			row.Model = &m
		}
		if res.Reason != "" {
			r := res.Reason
			row.Reason = &r
		}
		rows = append(rows, row)
	}

	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rows); err != nil {
		return "", fmt.Errorf("resolve: JSON encode: %w", err)
	}
	return strings.TrimSpace(buf.String()), nil
}
