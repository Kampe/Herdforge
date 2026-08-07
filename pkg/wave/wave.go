// Package wave is the operator entry point for a controlled work wave (FAC-105).
//
// Default mode is a read-only pre-wave report: readiness gates, quota/capacity,
// attention, review pressure, and deterministic claimable work. Under explicit
// --standing/--up it raises only configured standing roles after every
// readiness gate passes. It never claims task work; eligible refs are handed
// to the normal lifecycle/dispatch path via next-actions only.
//
// Fail-closed: any gate status other than "ok" that sets BlocksRaise prevents
// all raise actions. Unknown state is never treated as ready.
package wave

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Status is the ternary outcome of one readiness gate.
type Status string

const (
	// StatusOK means the gate admits raise/report progression.
	StatusOK Status = "ok"
	// StatusBlocked means a known active block (wind-down on, freeze on, ALERT).
	StatusBlocked Status = "blocked"
	// StatusFailed means a check ran and failed hard.
	StatusFailed Status = "failed"
	// StatusUnknown means state could not be determined — never admit raise.
	StatusUnknown Status = "unknown"
)

// Gate is one readiness check in the pre-wave report.
type Gate struct {
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Detail      string `json:"detail,omitempty"`
	BlocksRaise bool   `json:"blocks_raise"`
}

// NextAction is a human-actionable next step; wave never executes claims.
type NextAction struct {
	Kind    string `json:"kind"`
	Command string `json:"command,omitempty"`
	Summary string `json:"summary"`
}

// StandingAction is the planned outcome for one standing agent name.
type StandingAction string

const (
	// StandingRaise means the agent is missing and should be raised.
	StandingRaise StandingAction = "raise"
	// StandingAlreadyLive means the agent is already present — idempotent skip.
	StandingAlreadyLive StandingAction = "already_live"
	// StandingSkipHeld means a durable hold parks this lane.
	StandingSkipHeld StandingAction = "skip_held"
)

// StandingPlan is the planned raise decision for one standing agent.
type StandingPlan struct {
	Name   string         `json:"name"`
	Lane   string         `json:"lane"`
	Action StandingAction `json:"action"`
	Detail string         `json:"detail,omitempty"`
}

// ClaimableRef is one board card that may enter the normal claim path.
type ClaimableRef struct {
	Ref      string `json:"ref"`
	Title    string `json:"title,omitempty"`
	Priority string `json:"priority,omitempty"`
	Role     string `json:"role,omitempty"`
}

// RaiseResult records what a mutating raise attempt did for one agent.
type RaiseResult struct {
	Name   string `json:"name"`
	Action string `json:"action"` // raised | already_live | skip_held | failed | blocked
	Detail string `json:"detail,omitempty"`
}

// ReviewPressure is in-review count vs the operator cap.
type ReviewPressure struct {
	InReview int  `json:"in_review"`
	Cap      int  `json:"cap"`
	Pressure bool `json:"pressure"`
}

// AttentionItem is a compact attention row for the wave report.
type AttentionItem struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Level  string `json:"level"`
	Reason string `json:"reason,omitempty"`
}

// Report is the stable JSON shape of a wave evaluation (and optional raise).
type Report struct {
	Mode         string          `json:"mode"` // report | standing
	Ready        bool            `json:"ready"`
	Gates        []Gate          `json:"gates"`
	Resources    string          `json:"resources,omitempty"`
	Attention    []AttentionItem `json:"attention,omitempty"`
	Claimable    []ClaimableRef  `json:"claimable"`
	Review       ReviewPressure  `json:"review"`
	StandingPlan []StandingPlan  `json:"standing_plan"`
	NextActions  []NextAction    `json:"next_actions"`
	RaiseResults []RaiseResult   `json:"raise_results,omitempty"`
	// Mutation is true only when readiness passed and the standing raise
	// path executed (including already_live / skip_held outcomes). Report
	// mode and refused raises leave this false — no board/session/lease/
	// worktree side effects occurred.
	Mutation bool `json:"mutation"`
}

// Options select report-only vs raise-after-gates.
type Options struct {
	// Standing requests raise of every configured standing role after gates pass.
	Standing bool
	// Up is an alias of Standing (ticket: --standing or --up).
	Up bool
}

// WantsRaise reports whether the operator asked to raise standing roles.
func (o Options) WantsRaise() bool { return o.Standing || o.Up }

// Mode returns the report mode label.
func (o Options) Mode() string {
	if o.WantsRaise() {
		return "standing"
	}
	return "report"
}

// Agent is the live-fleet subset wave reasons about.
type Agent struct {
	Name   string
	Status string
	PaneID string
	TabID  string
}

// Lane is one configured standing lane.
type Lane struct {
	// Name is the config lane id (e.g. "worker").
	Name string
	// AgentName is the live herdr name (e.g. "forge-worker").
	AgentName string
}

// Sources are pure-read inputs for Evaluate. Every field is optional; a nil
// source becomes StatusUnknown with BlocksRaise=true (fail-closed), except
// optional report enrichments (claimable, review, attention) which degrade to
// empty with an explicit unknown gate only when the source errors.
type Sources struct {
	// Winddown returns nil when fleet admission is allowed.
	Winddown func(ctx context.Context) error
	// BoardFreeze returns whether the board mutation gate is active.
	BoardFreeze func() (frozen bool, detail string, err error)
	// Resources returns a resources verdict (OK/TIGHT/ALERT) and detail.
	Resources func() (verdict, detail string)
	// Quota returns whether capacity is usable. err => unknown.
	Quota func() (ok bool, detail string, err error)
	// HerdrOK reports whether the herdr CLI/control plane is reachable.
	HerdrOK func() (ok bool, detail string)
	// StandingLanes returns configured standing lanes only.
	StandingLanes func() []Lane
	// LiveAgents lists current fleet agents (read-only).
	LiveAgents func() ([]Agent, error)
	// Held reports whether a standing agent name is under durable hold.
	Held func(agentName string) (held bool, reason string)
	// Claimable lists deterministic claimable work (read-only; never claims).
	Claimable func(ctx context.Context) ([]ClaimableRef, error)
	// InReview returns the current in-review count (read-only).
	InReview func(ctx context.Context) (int, error)
	// ReviewCap is the pressure threshold; default 3 when zero.
	ReviewCap int
	// Attention classifies live agents against the standing roster.
	// When nil, Evaluate builds a minimal missing/live view.
	Attention func(agents []Agent, standing []Lane) []AttentionItem
}

// Raiser performs standing-agent raise. Production wiring must make Raise
// idempotent: calling Raise on an already-live agent is a no-op success.
type Raiser interface {
	// Raise brings up one standing agent. Must not duplicate an existing live agent.
	Raise(ctx context.Context, lane Lane) error
}

// countingRaiser is used only in tests via package helpers; production uses
// the injected Raiser directly.

// Evaluate builds a read-only pre-wave report. It never calls a Raiser and
// never mutates board, session, lease, or worktree state through Sources
// (callers must inject read-only sources).
func Evaluate(ctx context.Context, src Sources, opts Options) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rep := &Report{
		Mode:      opts.Mode(),
		Claimable: []ClaimableRef{},
		Mutation:  false,
	}
	if src.ReviewCap <= 0 {
		src.ReviewCap = 3
	}
	rep.Review.Cap = src.ReviewCap

	rep.Gates = collectGates(ctx, src)
	rep.Ready = Ready(rep.Gates)

	for _, g := range rep.Gates {
		if g.Name == "resources" {
			rep.Resources = g.Detail
		}
	}

	lanes := []Lane{}
	if src.StandingLanes != nil {
		lanes = append(lanes, src.StandingLanes()...)
	}
	sort.SliceStable(lanes, func(i, j int) bool {
		return lanes[i].AgentName < lanes[j].AgentName
	})

	var agents []Agent
	if src.LiveAgents != nil {
		list, err := src.LiveAgents()
		if err != nil {
			// Already reflected as herdr/live gate; keep agents empty.
			agents = nil
		} else {
			agents = list
		}
	}
	live := liveIndex(agents)

	if src.Attention != nil {
		rep.Attention = src.Attention(agents, lanes)
	} else {
		rep.Attention = defaultAttention(agents, lanes)
	}

	if src.Claimable != nil {
		claimable, err := src.Claimable(ctx)
		if err != nil {
			rep.Gates = append(rep.Gates, Gate{
				Name: "claimable", Status: StatusUnknown,
				Detail: "claimable list unreadable: " + err.Error(), BlocksRaise: false,
			})
			// Unknown claimable does not block raise (raise is fleet posture,
			// not claim), but Ready is recomputed only over BlocksRaise gates.
		} else {
			rep.Claimable = claimable
			if rep.Claimable == nil {
				rep.Claimable = []ClaimableRef{}
			}
		}
	}

	if src.InReview != nil {
		n, err := src.InReview(ctx)
		if err != nil {
			rep.Gates = append(rep.Gates, Gate{
				Name: "review", Status: StatusUnknown,
				Detail: "in-review count unreadable: " + err.Error(), BlocksRaise: false,
			})
		} else {
			rep.Review.InReview = n
			rep.Review.Pressure = n >= rep.Review.Cap
		}
	}

	heldFn := src.Held
	if heldFn == nil {
		heldFn = func(string) (bool, string) { return false, "" }
	}
	rep.StandingPlan = planStanding(lanes, live, heldFn)
	rep.NextActions = nextActions(rep, opts)
	// Recompute Ready after optional non-blocking gates were appended.
	rep.Ready = Ready(rep.Gates)
	return rep, nil
}

// Ready reports whether every BlocksRaise gate is StatusOK.
func Ready(gates []Gate) bool {
	for _, g := range gates {
		if g.BlocksRaise && g.Status != StatusOK {
			return false
		}
	}
	return true
}

// RaiseStanding applies the standing plan via raiser only when Ready.
// When not ready it records blocked results and returns a non-nil error.
// Already-live and held lanes are skipped without calling Raise (idempotent).
func RaiseStanding(ctx context.Context, rep *Report, raiser Raiser) error {
	if rep == nil {
		return fmt.Errorf("wave: nil report")
	}
	if raiser == nil {
		return fmt.Errorf("wave: raiser is required for standing raise")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !rep.Ready {
		// No side effects when gates fail — Mutation stays false.
		for _, p := range rep.StandingPlan {
			rep.RaiseResults = append(rep.RaiseResults, RaiseResult{
				Name: p.Name, Action: "blocked",
				Detail: "readiness gate failed or unknown; raise refused",
			})
		}
		return fmt.Errorf("wave: raise refused — readiness not ready")
	}

	// Gates passed: any raise path outcome (raised / already_live / skip_held)
	// is a deliberate standing activation attempt.
	rep.Mutation = true
	var firstErr error
	for _, p := range rep.StandingPlan {
		switch p.Action {
		case StandingAlreadyLive:
			rep.RaiseResults = append(rep.RaiseResults, RaiseResult{
				Name: p.Name, Action: "already_live", Detail: p.Detail,
			})
			continue
		case StandingSkipHeld:
			rep.RaiseResults = append(rep.RaiseResults, RaiseResult{
				Name: p.Name, Action: "skip_held", Detail: p.Detail,
			})
			continue
		case StandingRaise:
			if err := raiser.Raise(ctx, Lane{Name: p.Lane, AgentName: p.Name}); err != nil {
				rep.RaiseResults = append(rep.RaiseResults, RaiseResult{
					Name: p.Name, Action: "failed", Detail: err.Error(),
				})
				if firstErr == nil {
					firstErr = fmt.Errorf("raise %s: %w", p.Name, err)
				}
				continue
			}
			rep.RaiseResults = append(rep.RaiseResults, RaiseResult{
				Name: p.Name, Action: "raised",
			})
		default:
			rep.RaiseResults = append(rep.RaiseResults, RaiseResult{
				Name: p.Name, Action: "failed", Detail: "unknown plan action " + string(p.Action),
			})
			if firstErr == nil {
				firstErr = fmt.Errorf("wave: unknown standing plan action %q for %s", p.Action, p.Name)
			}
		}
	}
	return firstErr
}

// Run is Evaluate followed by optional RaiseStanding when opts.WantsRaise.
// Report-only mode never touches raiser (nil is fine).
func Run(ctx context.Context, src Sources, opts Options, raiser Raiser) (*Report, error) {
	rep, err := Evaluate(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	if !opts.WantsRaise() {
		return rep, nil
	}
	raiseErr := RaiseStanding(ctx, rep, raiser)
	return rep, raiseErr
}

func collectGates(ctx context.Context, src Sources) []Gate {
	var gates []Gate

	// winddown
	if src.Winddown == nil {
		gates = append(gates, Gate{Name: "winddown", Status: StatusUnknown, Detail: "winddown source not configured", BlocksRaise: true})
	} else if err := src.Winddown(ctx); err != nil {
		st := StatusBlocked
		if isUnknownErr(err) {
			st = StatusUnknown
		}
		gates = append(gates, Gate{Name: "winddown", Status: st, Detail: err.Error(), BlocksRaise: true})
	} else {
		gates = append(gates, Gate{Name: "winddown", Status: StatusOK, Detail: "fleet admission allowed", BlocksRaise: true})
	}

	// board freeze
	if src.BoardFreeze == nil {
		gates = append(gates, Gate{Name: "board_freeze", Status: StatusUnknown, Detail: "board freeze source not configured", BlocksRaise: true})
	} else {
		frozen, detail, err := src.BoardFreeze()
		switch {
		case err != nil:
			gates = append(gates, Gate{Name: "board_freeze", Status: StatusUnknown, Detail: err.Error(), BlocksRaise: true})
		case frozen:
			if detail == "" {
				detail = "board is frozen"
			}
			gates = append(gates, Gate{Name: "board_freeze", Status: StatusBlocked, Detail: detail, BlocksRaise: true})
		default:
			gates = append(gates, Gate{Name: "board_freeze", Status: StatusOK, Detail: "board not frozen", BlocksRaise: true})
		}
	}

	// resources / capacity
	if src.Resources == nil {
		gates = append(gates, Gate{Name: "resources", Status: StatusUnknown, Detail: "resources source not configured", BlocksRaise: true})
	} else {
		verdict, detail := src.Resources()
		v := strings.ToUpper(strings.TrimSpace(verdict))
		d := detail
		if d == "" {
			d = "verdict=" + v
		}
		switch v {
		case "OK", "TIGHT":
			gates = append(gates, Gate{Name: "resources", Status: StatusOK, Detail: d, BlocksRaise: true})
		case "ALERT":
			gates = append(gates, Gate{Name: "resources", Status: StatusBlocked, Detail: d, BlocksRaise: true})
		default:
			gates = append(gates, Gate{Name: "resources", Status: StatusUnknown, Detail: d, BlocksRaise: true})
		}
	}

	// quota
	if src.Quota == nil {
		gates = append(gates, Gate{Name: "quota", Status: StatusUnknown, Detail: "quota source not configured", BlocksRaise: true})
	} else {
		ok, detail, err := src.Quota()
		switch {
		case err != nil:
			gates = append(gates, Gate{Name: "quota", Status: StatusUnknown, Detail: err.Error(), BlocksRaise: true})
		case !ok:
			if detail == "" {
				detail = "quota capacity not usable"
			}
			gates = append(gates, Gate{Name: "quota", Status: StatusBlocked, Detail: detail, BlocksRaise: true})
		default:
			if detail == "" {
				detail = "quota usable"
			}
			gates = append(gates, Gate{Name: "quota", Status: StatusOK, Detail: detail, BlocksRaise: true})
		}
	}

	// herdr
	if src.HerdrOK == nil {
		gates = append(gates, Gate{Name: "herdr", Status: StatusUnknown, Detail: "herdr source not configured", BlocksRaise: true})
	} else {
		ok, detail := src.HerdrOK()
		if !ok {
			if detail == "" {
				detail = "herdr unavailable"
			}
			gates = append(gates, Gate{Name: "herdr", Status: StatusUnknown, Detail: detail, BlocksRaise: true})
		} else {
			if detail == "" {
				detail = "herdr available"
			}
			gates = append(gates, Gate{Name: "herdr", Status: StatusOK, Detail: detail, BlocksRaise: true})
		}
	}

	// standing roster configured
	if src.StandingLanes == nil {
		gates = append(gates, Gate{Name: "standing_roster", Status: StatusUnknown, Detail: "standing roster source not configured", BlocksRaise: true})
	} else {
		lanes := src.StandingLanes()
		if len(lanes) == 0 {
			gates = append(gates, Gate{Name: "standing_roster", Status: StatusFailed, Detail: "no standing lanes configured", BlocksRaise: true})
		} else {
			gates = append(gates, Gate{
				Name: "standing_roster", Status: StatusOK,
				Detail: fmt.Sprintf("%d standing lane(s)", len(lanes)),
				BlocksRaise: true,
			})
		}
	}

	return gates
}

func isUnknownErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "missing") ||
		strings.Contains(msg, "corrupt") ||
		strings.Contains(msg, "unreadable") ||
		strings.Contains(msg, "unknown")
}

func liveIndex(agents []Agent) map[string]Agent {
	idx := make(map[string]Agent, len(agents))
	for _, a := range agents {
		if a.Name != "" {
			idx[a.Name] = a
		}
	}
	return idx
}

func planStanding(lanes []Lane, live map[string]Agent, held func(string) (bool, string)) []StandingPlan {
	out := make([]StandingPlan, 0, len(lanes))
	for _, lane := range lanes {
		name := lane.AgentName
		if name == "" {
			name = "forge-" + lane.Name
		}
		if _, ok := live[name]; ok {
			out = append(out, StandingPlan{
				Name: name, Lane: lane.Name, Action: StandingAlreadyLive,
				Detail: "agent already live",
			})
			continue
		}
		if h, reason := held(name); h {
			if reason == "" {
				reason = "held by coordinator"
			}
			out = append(out, StandingPlan{
				Name: name, Lane: lane.Name, Action: StandingSkipHeld,
				Detail: reason,
			})
			continue
		}
		out = append(out, StandingPlan{
			Name: name, Lane: lane.Name, Action: StandingRaise,
			Detail: "not live — will raise when ready",
		})
	}
	return out
}

func defaultAttention(agents []Agent, lanes []Lane) []AttentionItem {
	live := liveIndex(agents)
	var items []AttentionItem
	for _, lane := range lanes {
		name := lane.AgentName
		if name == "" {
			name = "forge-" + lane.Name
		}
		if a, ok := live[name]; ok {
			status := a.Status
			if status == "" {
				status = "unknown"
			}
			level := "none"
			reason := "live"
			switch strings.ToLower(status) {
			case "working", "starting":
				level = "none"
				reason = "healthy"
			case "done":
				level = "high"
				reason = "done — awaiting review/harvest"
			case "blocked":
				level = "critical"
				reason = "blocked"
			case "idle":
				level = "medium"
				reason = "idle — may need kick"
			default:
				level = "medium"
				reason = "status=" + status
			}
			if level != "none" {
				items = append(items, AttentionItem{Name: name, Status: status, Level: level, Reason: reason})
			}
			continue
		}
		items = append(items, AttentionItem{
			Name: name, Status: "missing", Level: "missing",
			Reason: "not live — needs raising",
		})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items
}

func nextActions(rep *Report, opts Options) []NextAction {
	var out []NextAction
	for _, g := range rep.Gates {
		if !g.BlocksRaise || g.Status == StatusOK {
			continue
		}
		switch g.Name {
		case "winddown":
			out = append(out, NextAction{
				Kind: "clear-winddown", Command: "herd wind-down off --reason <why> --generation <n>",
				Summary: "clear wind-down posture before raising the fleet: " + g.Detail,
			})
		case "board_freeze":
			out = append(out, NextAction{
				Kind: "clear-board-freeze", Command: "herd board-freeze off --actor <who>",
				Summary: "thaw the board freeze before a controlled wave: " + g.Detail,
			})
		case "resources":
			out = append(out, NextAction{
				Kind: "relieve-capacity", Command: "herd resources",
				Summary: "capacity gate blocked raise: " + g.Detail,
			})
		case "quota":
			out = append(out, NextAction{
				Kind: "relieve-quota", Command: "herd quota-supervisor --read-only",
				Summary: "quota gate blocked raise: " + g.Detail,
			})
		case "herdr":
			out = append(out, NextAction{
				Kind: "restore-herdr", Command: "herdr --version",
				Summary: "herdr unavailable: " + g.Detail,
			})
		default:
			out = append(out, NextAction{
				Kind:    "fix-gate",
				Summary: fmt.Sprintf("gate %s is %s: %s", g.Name, g.Status, g.Detail),
			})
		}
	}

	if rep.Ready {
		if opts.WantsRaise() {
			// raise path already selected; still surface claim handoff
		} else {
			needRaise := false
			for _, p := range rep.StandingPlan {
				if p.Action == StandingRaise {
					needRaise = true
					break
				}
			}
			if needRaise {
				out = append(out, NextAction{
					Kind: "raise-standing", Command: "herd wave --standing",
					Summary: "readiness ok; raise configured standing roles",
				})
			}
		}
	}

	if rep.Review.Pressure {
		out = append(out, NextAction{
			Kind: "review-pressure", Command: "herd review",
			Summary: fmt.Sprintf("review pressure: %d in-review (cap %d)", rep.Review.InReview, rep.Review.Cap),
		})
	}

	for _, a := range rep.Attention {
		if a.Level == "critical" || a.Level == "high" {
			out = append(out, NextAction{
				Kind: "attention", Command: "herd attention",
				Summary: fmt.Sprintf("%s needs eyes (%s): %s", a.Name, a.Level, a.Reason),
			})
		}
	}

	// Deterministic claimable handoff — never claim here.
	if len(rep.Claimable) > 0 {
		top := rep.Claimable[0]
		cmd := "herd dispatch " + top.Ref
		if top.Role != "" {
			cmd = "herd pulse --role " + top.Role + " --spawn"
		}
		out = append(out, NextAction{
			Kind: "dispatch-claimable", Command: cmd,
			Summary: fmt.Sprintf("claimable work ready for normal lifecycle: %s", top.Ref),
		})
	} else if rep.Ready {
		out = append(out, NextAction{
			Kind: "none-claimable", Command: "herd next",
			Summary: "no claimable cards in this snapshot; consult herd next",
		})
	}

	return out
}

// FormatHuman renders a concise operator report to a multi-line string.
func FormatHuman(rep *Report) string {
	if rep == nil {
		return "herd wave: nil report"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "herd wave: mode=%s ready=%v\n", rep.Mode, rep.Ready)
	for _, g := range rep.Gates {
		fmt.Fprintf(&b, "  gate %-16s %-8s %s\n", g.Name, g.Status, g.Detail)
	}
	fmt.Fprintf(&b, "  review in_review=%d cap=%d pressure=%v\n", rep.Review.InReview, rep.Review.Cap, rep.Review.Pressure)
	if len(rep.Claimable) == 0 {
		fmt.Fprintf(&b, "  claimable: (none)\n")
	} else {
		fmt.Fprintf(&b, "  claimable:\n")
		for _, c := range rep.Claimable {
			fmt.Fprintf(&b, "    - %s %s\n", c.Ref, c.Title)
		}
	}
	if len(rep.StandingPlan) > 0 {
		fmt.Fprintf(&b, "  standing plan:\n")
		for _, p := range rep.StandingPlan {
			fmt.Fprintf(&b, "    - %s action=%s %s\n", p.Name, p.Action, p.Detail)
		}
	}
	if len(rep.RaiseResults) > 0 {
		fmt.Fprintf(&b, "  raise results:\n")
		for _, r := range rep.RaiseResults {
			fmt.Fprintf(&b, "    - %s action=%s %s\n", r.Name, r.Action, r.Detail)
		}
	}
	if len(rep.NextActions) > 0 {
		fmt.Fprintf(&b, "  next actions:\n")
		for _, a := range rep.NextActions {
			if a.Command != "" {
				fmt.Fprintf(&b, "    - [%s] %s\n      $ %s\n", a.Kind, a.Summary, a.Command)
			} else {
				fmt.Fprintf(&b, "    - [%s] %s\n", a.Kind, a.Summary)
			}
		}
	}
	return b.String()
}
