// Package quotasup ports bin/herd-quota-supervisor: proactive fleet quota
// supervision.
//
// It reads live pool-specific usage BEFORE panes start failing, maps every live
// agent to the pool it is actually routed on, classifies that pool's capacity,
// and reports transitions. Reacting to a dead pane is too late — by then the
// lane has already burned a launch and lost its context.
package quotasup

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// Capacity is a pool's headroom as the supervisor sees it.
type Capacity string

const (
	// Healthy: room to launch.
	Healthy Capacity = "healthy"
	// AtRisk: projected to exhaust before its window resets, within the
	// warning runway.
	AtRisk Capacity = "at_risk"
	// Exhausted: no headroom now.
	Exhausted Capacity = "exhausted"
	// Unknown: the ledger is stale or errored. NOT healthy — an unreadable
	// pool must never be treated as available.
	Unknown Capacity = "unknown"
	// Untracked: no ledger row at all for this pool.
	Untracked Capacity = "untracked"
)

// DefaultWarnRunway is how close to exhaustion counts as at-risk.
const DefaultWarnRunwayMinutes = 120

// QuotaProvider maps a launch provider to its quota-ledger name.
func QuotaProvider(provider string) string {
	if strings.EqualFold(provider, "agy") {
		return "antigravity"
	}
	return strings.ToLower(strings.TrimSpace(provider))
}

// QuotaPool maps (provider, model) to the independently metered pool that
// actually bills the launch. Getting this wrong is worse than not checking:
// a lane on codex/spark charged against codex/default reads as exhausted while
// its own pool is idle, and the supervisor reroutes work that never needed to
// move.
//
// The table itself is router.QuotaPoolFor's, deliberately: if the supervisor
// and the router disagreed about which pool a model bills, every cap the
// supervisor set would apply to a surface the router never routes to. This
// wrapper only normalises live, untrusted input — herdr reports the harness
// kind and argv verbatim, in whatever case the operator typed — before handing
// it to a table keyed on canonical lowercase names.
func QuotaPool(provider, model string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "antigravity" {
		p = "agy" // the pool table is keyed by harness name, not ledger name
	}
	return router.QuotaPoolFor(p, strings.ToLower(strings.TrimSpace(model)))
}

// ModelFromArgv recovers the ROUTED model a running agent was launched with.
//
// `herdr agent list` reports no model (verified against the 0.7.5 API schema:
// AgentInfo carries agent/display_agent, never a model), so the running
// process's own argv is the only live evidence of which pool a lane bills.
// Returns "" when the lane took its surface default, which is not an error:
// QuotaPool maps an empty model to the surface's default pool.
//
// A lane launched through the Pi harness carries a vendor-qualified model
// ("anthropic/claude-fable-5"), because that is what Pi's --model wants — the
// harness is not the provider. The pool table is keyed on routed model names,
// so the qualification is undone here. Left in place it silently mis-bills:
// "anthropic/claude-fable-5" misses the exact-match fable rule and
// "google/gemini-3.1-pro-high" misses the gemini prefix rule, so a Fable lane
// bills claude/default and a Gemini lane bills agy/nonGemini — each capping a
// pool that was never touched while the real one runs uncapped.
func ModelFromArgv(argv []string) string {
	model := ""
	for i, a := range argv {
		if a == "--model" || a == "-m" {
			if i+1 < len(argv) {
				model = strings.TrimSpace(argv[i+1])
			}
			break
		}
		if v, ok := strings.CutPrefix(a, "--model="); ok {
			model = strings.TrimSpace(v)
			break
		}
	}
	if model == "" {
		return ""
	}
	if len(argv) > 0 && filepath.Base(argv[0]) == router.PiHarness {
		return router.PiBareModel(model)
	}
	return model
}

// Classify grades one pool's capacity. A nil state is Untracked, and a stale
// or errored ledger is Unknown rather than Healthy — failing open here would
// send work at a pool nobody can vouch for.
func Classify(st *usage.BurnState, warnRunwayMinutes int) Capacity {
	if st == nil {
		return Untracked
	}
	if st.Stale {
		return Unknown
	}
	switch strings.ToLower(st.Reason) {
	case "stale", "provider-error", "no-quota-data":
		return Unknown
	case "exhausted":
		return Exhausted
	}
	if st.Class == usage.BurnExhausted {
		return Exhausted
	}
	if st.ExhaustsBeforeReset != nil && *st.ExhaustsBeforeReset &&
		st.RunwayMinutes != nil && *st.RunwayMinutes <= warnRunwayMinutes {
		return AtRisk
	}
	return Healthy
}

// Assignment is one live agent bound to the pool it actually runs on.
//
// Provider is the harness kind herdr reports ("agy"); QuotaProvider is the
// ledger that meters it ("antigravity"). They are kept apart because the
// cooldown store and the router are keyed on the first while the quota map is
// keyed on the second.
type Assignment struct {
	Name          string   `json:"name"`
	PaneID        string   `json:"pane_id,omitempty"`
	TabID         string   `json:"tab_id,omitempty"`
	AgentStatus   string   `json:"agent_status"`
	Provider      string   `json:"provider"`
	QuotaProvider string   `json:"quota_provider"`
	Model         string   `json:"model,omitempty"`
	Family        string   `json:"family,omitempty"`
	Pool          string   `json:"pool"`
	Capacity      Capacity `json:"capacity_state"`
	// ModelResolved records whether the lane's argv was actually read. False
	// means Pool is the provider's default by fallback, not by evidence —
	// this lane's contribution to a pool's active count is a guess, and the
	// snapshot says so rather than letting it read as an observation.
	ModelResolved bool   `json:"model_resolved"`
	ProviderError string `json:"provider_error,omitempty"`
}

// Surface is the metered surface this agent bills against.
func (a Assignment) Surface() Surface {
	return Surface{Provider: a.QuotaProvider, Pool: a.Pool}
}

// Snapshot is one observation of the fleet's capacity, and the whole of what
// the supervisor carries across runs. Decisions is the durable part: it is
// what makes the next run's caps continuous with this one's instead of
// restarting the ratchet from cold every time the process bounces.
type Snapshot struct {
	ObservedAt        string         `json:"observed_at"`
	SourceAt          string         `json:"source_at,omitempty"`
	Workspace         string         `json:"workspace"`
	WarnRunwayMinutes int            `json:"warn_runway_minutes"`
	Agents            []Assignment   `json:"agents"`
	Decisions         []Decision     `json:"decisions,omitempty"`
	Wake              *WakePlan      `json:"wake,omitempty"`
	Recovery          []RecoveryPlan `json:"recovery,omitempty"`
}

// WakePlan is a durable, deduplicable self-wake hint for a coordinator. It is
// a plan rather than a scheduler side effect; callers persist it and arm the
// platform's cron/launchd adapter using the stable Key.
type WakePlan struct {
	Key      string    `json:"key"`
	Provider string    `json:"provider"`
	Window   string    `json:"window"`
	ResetAt  time.Time `json:"reset_at"`
	Reason   string    `json:"reason"`
}

// RecoveryPlan distinguishes a short five-hour exhaustion from a weekly cap.
// The coordinator keeps its identity while routing around the exhausted pool;
// only a five-hour reset gets a precise self-wake.
type RecoveryPlan struct {
	Provider string    `json:"provider"`
	Pool     string    `json:"pool"`
	Window   string    `json:"window"`
	ResetAt  time.Time `json:"reset_at"`
	Action   string    `json:"action"`
	Reason   string    `json:"reason"`
}

// PlanRecovery preserves nested quota-window evidence and returns the
// deterministic failover action for an exhausted surface.
func PlanRecovery(now time.Time, d Decision) *RecoveryPlan {
	windows := append([]usage.BurnState(nil), d.Evidence.Windows...)
	if len(windows) == 0 && d.Evidence.Window != "" {
		windows = append(windows, usage.BurnState{Window: d.Evidence.Window, WindowSeconds: d.Evidence.WindowSeconds, ResetsAt: d.Evidence.ResetAt, Class: usage.BurnExhausted})
	}
	var best *RecoveryPlan
	for _, w := range windows {
		if w.Class != usage.BurnExhausted && d.Evidence.Capacity != Exhausted {
			continue
		}
		reset, err := time.Parse(time.RFC3339, strings.TrimSpace(w.ResetsAt))
		if err != nil || !reset.After(now) {
			continue
		}
		window := strings.ToLower(strings.TrimSpace(w.Window))
		switch {
		case w.WindowSeconds == usage.Window5h || window == "5h" || window == "five_hour":
			window = "5h"
		case w.WindowSeconds == usage.WindowWeekly || window == "weekly" || window == "7d":
			window = "weekly"
		default:
			continue
		}
		action := "reroute-until-reset"
		if window == "5h" {
			action = "reroute-and-self-wake"
		}
		candidate := &RecoveryPlan{Provider: d.Surface.Provider, Pool: d.Surface.Pool, Window: window, ResetAt: reset.UTC(), Action: action,
			Reason: fmt.Sprintf("%s quota exhausted; keep coordinator identity and fail over to a healthy surface", window)}
		if best == nil || candidate.ResetAt.Before(best.ResetAt) {
			best = candidate
		}
	}
	return best
}

// PlanClaudeFiveHourWake returns the earliest future Claude five-hour reset
// among observed surfaces. A reset without a parseable timestamp is not a
// wake plan: coordinators must never arm a timer from an invented deadline.
func PlanClaudeFiveHourWake(now time.Time, decisions []Decision) *WakePlan {
	var best *WakePlan
	for _, d := range decisions {
		provider := strings.ToLower(strings.TrimSpace(d.Surface.Provider))
		if provider != "anthropic" && provider != "claude" {
			continue
		}
		e := d.Evidence
		if e.WindowSeconds != usage.Window5h && !strings.EqualFold(e.Window, "5h") && !strings.EqualFold(e.Window, "five_hour") {
			continue
		}
		reset, err := time.Parse(time.RFC3339, strings.TrimSpace(e.ResetAt))
		if err != nil || !reset.After(now) {
			continue
		}
		candidate := &WakePlan{
			Key:      fmt.Sprintf("%s/5h/%s", provider, reset.UTC().Format(time.RFC3339)),
			Provider: provider, Window: "5h", ResetAt: reset.UTC(),
			Reason: "Claude five-hour quota window reset; re-read live quota before dispatch",
		}
		if best == nil || candidate.ResetAt.Before(best.ResetAt) {
			best = candidate
		}
	}
	return best
}

// UnresolvedModels names the lanes whose pool is a fallback rather than an
// observation, so a caller can report the gap instead of publishing a count
// that reads as fully evidenced.
func (s *Snapshot) UnresolvedModels() []string {
	var out []string
	for _, a := range s.Agents {
		if !a.ModelResolved {
			out = append(out, a.Name)
		}
	}
	return out
}

// Blocked lists the surfaces admitting no new work, in decision order.
func (s *Snapshot) Blocked() []Decision {
	var out []Decision
	for _, d := range s.Decisions {
		if d.Cap == 0 {
			out = append(out, d)
		}
	}
	return out
}

// Counts summarises a snapshot.
type Counts struct{ Agents, Exhausted, AtRisk, Unknown int }

func (s *Snapshot) Counts() Counts {
	c := Counts{Agents: len(s.Agents)}
	for _, a := range s.Agents {
		switch a.Capacity {
		case Exhausted:
			c.Exhausted++
		case AtRisk:
			c.AtRisk++
		case Unknown, Untracked:
			c.Unknown++
		}
	}
	return c
}

// FirstObservation is the sentinel prior state for a lane never seen before.
const FirstObservation Capacity = "new"

// Prior returns a lane's previously observed capacity, or FirstObservation.
func Prior(prev *Snapshot, lane string) Capacity {
	if prev == nil {
		return FirstObservation
	}
	for _, a := range prev.Agents {
		if a.Name == lane {
			return a.Capacity
		}
	}
	return FirstObservation
}

// IsTransition reports whether a capacity change is worth telling the
// coordinator about.
//
// A lane first observed as healthy is a BASELINE, not an incident — reporting
// it would make every fresh supervisor run page the coordinator about a fleet
// that is fine. Recovery from a previously risky state IS a transition and is
// always reported.
func IsTransition(old, current Capacity) bool {
	if old == current {
		return false
	}
	if old == FirstObservation && current == Healthy {
		return false
	}
	return true
}
