package quotasup

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/credits"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// Surface is one independently metered execution surface: the ledger provider
// plus the pool inside it that actually bills the launch.
//
// Caps are keyed here and nowhere coarser. Keying on the provider alone is the
// failure this type exists to prevent: an exhausted claude/fable pool would
// take claude/default down with it, and half the fleet stops for a pool that
// was never touched.
type Surface struct {
	Provider string `json:"provider"`
	Pool     string `json:"pool"`
}

func (s Surface) String() string { return s.Provider + "/" + s.Pool }

// Posture is what routing may do with a surface right now.
type Posture string

const (
	// PostureOpen: admit new work, up to Cap concurrent.
	PostureOpen Posture = "open"
	// PostureDrain: let in-flight work finish, admit no more than Cap. Set
	// while a surface is projected to exhaust but has not yet.
	PostureDrain Posture = "drain"
	// PostureBlocked: Cap is 0. No new work, for any reason.
	PostureBlocked Posture = "blocked"
)

const (
	// MaxCap is the ceiling any surface can reach. Matches the top of
	// credits.ClassConcurrency, which owns the pace->concurrency table.
	MaxCap = 3
	// RecoveryStreak is how many consecutive healthy observations a surface
	// must post before its cap rises. One healthy read is a sample; the
	// second is what makes recovery *verified*, and a single optimistic
	// sample is exactly how a recovering pool gets re-flooded and re-exhausted.
	RecoveryStreak = 2
	// MaxRisePerObservation bounds how fast a cap climbs. Drops are immediate;
	// only the climb is rate-limited, so the supervisor can never step from
	// blocked to full concurrency in one tick.
	MaxRisePerObservation = 1
	// DefaultMaxObservationAge is how old a quota reading may be and still
	// authorize work. Past it the surface reports Unknown: quota nobody
	// refreshed is not evidence that headroom still exists.
	DefaultMaxObservationAge = 5 * time.Minute
)

// Evidence is every input behind one surface's cap, kept verbatim so a
// persisted decision can be audited without re-running the supervisor.
type Evidence struct {
	Capacity      Capacity          `json:"capacity"`
	BurnClass     usage.BurnClass   `json:"burn_class,omitempty"`
	LedgerReason  string            `json:"ledger_reason,omitempty"`
	UsedPct       float64           `json:"used_pct"`
	Window        string            `json:"window,omitempty"`
	ResetAt       string            `json:"reset_at,omitempty"`
	WindowSeconds int               `json:"window_seconds,omitempty"`
	Windows       []usage.BurnState `json:"windows,omitempty"`
	RunwayMinutes *int              `json:"runway_minutes,omitempty"`
	// SourceAt is when the quota provider generated the reading, NOT when the
	// supervisor ran. Recording the supervisor's own clock here would make
	// every observation look fresh no matter how stale the ledger is.
	SourceAt   time.Time `json:"source_at"`
	AgeSeconds int       `json:"observation_age_seconds"`
	Stale      bool      `json:"stale"`
	// Cooldown is the routing store's own reason string, verbatim. The
	// expiry is deliberately not mirrored here: the router owns when a cool
	// lifts, and a second copy of that deadline is a second thing to drift.
	Cooldown       string   `json:"cooldown,omitempty"`
	Active         int      `json:"active_agents"`
	Models         []string `json:"models,omitempty"`
	ProviderErrors []string `json:"provider_errors,omitempty"`
}

// Observation is the raw evidence gathered for one surface, before grading.
type Observation struct {
	Surface Surface
	// Burn is the ledger row for this exact pool. Nil means no row at all.
	Burn           *usage.BurnState
	SourceAt       time.Time
	Cooldown       string
	Active         int
	Models         []string
	ProviderErrors []string
}

// Grade classifies a raw observation at the supplied instant.
//
// maxAge <= 0 means DefaultMaxObservationAge; a caller cannot accidentally
// disable the freshness gate by leaving the field zero.
func (o Observation) Grade(now time.Time, warnRunwayMinutes int, maxAge time.Duration) Evidence {
	if maxAge <= 0 {
		maxAge = DefaultMaxObservationAge
	}
	e := Evidence{
		Capacity:       Classify(o.Burn, warnRunwayMinutes),
		SourceAt:       o.SourceAt,
		Cooldown:       strings.TrimSpace(o.Cooldown),
		Active:         o.Active,
		Models:         append([]string(nil), o.Models...),
		ProviderErrors: append([]string(nil), o.ProviderErrors...),
	}
	sort.Strings(e.Models)
	sort.Strings(e.ProviderErrors)
	if o.Burn != nil {
		e.BurnClass = o.Burn.Class
		e.LedgerReason = o.Burn.Reason
		e.UsedPct = o.Burn.Used
		e.Window = o.Burn.Window
		e.ResetAt = o.Burn.ResetsAt
		e.WindowSeconds = o.Burn.WindowSeconds
		e.Windows = append([]usage.BurnState(nil), o.Burn.Windows...)
		e.RunwayMinutes = o.Burn.RunwayMinutes
		e.Stale = o.Burn.Stale
	}
	if len(e.ProviderErrors) > 0 {
		e.Capacity = Exhausted
		e.LedgerReason = "live provider error: " + strings.Join(e.ProviderErrors, "; ")
	}

	// An unset source timestamp is not "just now" — it is a reading whose
	// provenance we cannot establish, which is the same fail-closed case as
	// one we know to be old.
	if o.SourceAt.IsZero() {
		e.AgeSeconds = -1
		e.Capacity = Unknown
		e.Stale = true
		return e
	}
	age := now.Sub(o.SourceAt)
	if age < 0 {
		age = 0 // a clock-skewed future reading is not extra-fresh
	}
	e.AgeSeconds = int(age / time.Second)
	if age > maxAge {
		e.Capacity = Unknown
		e.Stale = true
	}
	return e
}

// State is what the supervisor carries across runs for one surface. It is the
// whole of the supervisor's memory: same State plus same Evidence must always
// yield the same Decision, which is what makes a restart safe.
type State struct {
	Cap    int `json:"cap"`
	Streak int `json:"healthy_streak"`
}

// Decision is the cap chosen for one surface, with the evidence that chose it.
type Decision struct {
	Surface  Surface  `json:"surface"`
	Cap      int      `json:"cap"`
	PriorCap int      `json:"prior_cap"`
	Target   int      `json:"target_cap"`
	Streak   int      `json:"healthy_streak"`
	Posture  Posture  `json:"posture"`
	Reason   string   `json:"reason"`
	Evidence Evidence `json:"evidence"`
}

// classCeiling is the concurrency the pace class alone would justify.
//
// The four real pace classes defer to credits.ClassConcurrency, which already
// owns that table. An unrecognised class under an otherwise-healthy ledger is
// NOT sent to that function's permissive default: a surface we cannot pace is
// worth a trickle, not the middle of the range.
func classCeiling(c usage.BurnClass) int {
	switch c {
	case usage.BurnUnderspent, usage.BurnOnpace, usage.BurnOverpace, usage.BurnExhausted:
		return credits.ClassConcurrency(credits.PaceClass(c))
	}
	return 1
}

// TargetCap is the cap this evidence alone justifies, before any hysteresis,
// together with the evidence that justified it.
func TargetCap(e Evidence) (int, string) {
	if e.Cooldown != "" {
		return 0, "cooldown: " + e.Cooldown
	}
	switch e.Capacity {
	case Exhausted:
		if len(e.ProviderErrors) > 0 {
			return 0, e.LedgerReason
		}
		return 0, fmt.Sprintf("exhausted (%.0f%% used, window %s)", e.UsedPct, orDash(e.Window))
	case Unknown:
		return 0, fmt.Sprintf("UNKNOWN quota (%s, observation age %s)",
			orDash(e.LedgerReason), ageText(e.AgeSeconds))
	case Untracked:
		return 0, "UNKNOWN quota (no ledger row for this pool)"
	case AtRisk:
		return 1, fmt.Sprintf("at risk (%.0f%% used, %s runway before window %s resets)",
			e.UsedPct, runwayText(e.RunwayMinutes), orDash(e.Window))
	}
	return clampCap(classCeiling(e.BurnClass)),
		fmt.Sprintf("healthy (%.0f%% used, pace %s, window %s)",
			e.UsedPct, orDash(string(e.BurnClass)), orDash(e.Window))
}

// Decide applies hysteresis to a graded surface.
//
// Capacity falls the instant the evidence says so and climbs by at most
// MaxRisePerObservation, and only once RecoveryStreak consecutive healthy
// observations have verified the recovery. The asymmetry is the point: being
// slow to trust a recovered pool costs throughput, while being quick to trust
// one costs the launch and the context of every lane sent at it.
func Decide(s Surface, e Evidence, prior State) Decision {
	target, why := TargetCap(e)
	target = clampCap(target)

	streak := 0
	if e.Capacity == Healthy && e.Cooldown == "" {
		streak = prior.Streak + 1
	}

	cap := clampCap(prior.Cap)
	switch {
	case target < cap:
		cap = target
		why += fmt.Sprintf("; cut %d->%d", prior.Cap, cap)
	case target > cap:
		if streak >= RecoveryStreak {
			if rise := cap + MaxRisePerObservation; rise < target {
				cap = rise
			} else {
				cap = target
			}
			why += fmt.Sprintf("; recovery verified over %d observations, raised %d->%d (target %d)",
				streak, prior.Cap, cap, target)
		} else {
			why += fmt.Sprintf("; holding %d: recovery unverified (%d/%d consecutive healthy observations)",
				cap, streak, RecoveryStreak)
		}
	}

	posture := PostureOpen
	switch {
	case cap == 0:
		posture = PostureBlocked
	case e.Capacity == AtRisk || e.Cooldown != "":
		posture = PostureDrain
	}

	return Decision{
		Surface:  s,
		Cap:      cap,
		PriorCap: prior.Cap,
		Target:   target,
		Streak:   streak,
		Posture:  posture,
		Reason: fmt.Sprintf("%s %s cap=%d: %s; %d active; observed %s (age %s)",
			s, posture, cap, why, e.Active, sourceText(e.SourceAt), ageText(e.AgeSeconds)),
		Evidence: e,
	}
}

// Admits reports whether a surface has room for one more launch. Routing asks
// this; it never reads Cap and Active separately, so the ">= not >" boundary
// lives in one place.
func (d Decision) Admits() bool { return d.Cap > 0 && d.Evidence.Active < d.Cap }

// State is what this decision hands to the next run.
func (d Decision) State() State { return State{Cap: d.Cap, Streak: d.Streak} }

func clampCap(c int) int {
	if c < 0 {
		return 0
	}
	if c > MaxCap {
		return MaxCap
	}
	return c
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func runwayText(m *int) string {
	if m == nil {
		return "unknown"
	}
	return fmt.Sprintf("%dm", *m)
}

func ageText(seconds int) string {
	if seconds < 0 {
		return "unknown"
	}
	return fmt.Sprintf("%ds", seconds)
}

func sourceText(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
