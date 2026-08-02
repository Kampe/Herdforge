package usage

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	Window5h      = 18000
	WindowWeekly  = 604800
	DefaultExhaustedPct = 95.0
)

type BurnClass string

const (
	BurnUnderspent BurnClass = "underspent"
	BurnUntracked  BurnClass = "untracked"
	BurnOnpace     BurnClass = "onpace"
	BurnOverpace   BurnClass = "overpace"
	BurnExhausted  BurnClass = "exhausted"
)

type BurnState struct {
	Resource         string    `json:"resource"`
	Window           string    `json:"window"`
	WindowSeconds    int       `json:"windowSeconds"`
	Used             float64   `json:"used"`
	Remaining        float64   `json:"remaining"`
	ResetsAt         string    `json:"resetsAt,omitempty"`
	ResetsIn         string    `json:"resetsIn"`
	Class            BurnClass `json:"class"`
	Pace             int       `json:"pace"`
	Pressure         float64   `json:"pressure"`
	RunwayMinutes    *int      `json:"runwayMinutes,omitempty"`
	ExhaustsBeforeReset *bool `json:"exhaustsBeforeReset,omitempty"`
	SoonestResetWindow string  `json:"soonestResetWindow,omitempty"`
	SoonestResetIn   string    `json:"soonestResetIn,omitempty"`
	PressureResource string    `json:"pressureResource,omitempty"`
	RunwayResource   string    `json:"runwayResource,omitempty"`
	Available        bool      `json:"available"`
	Reason           string    `json:"reason"`
	Stale            bool      `json:"stale"`
	Plan             string    `json:"plan,omitempty"`
	Windows          []BurnState `json:"windows,omitempty"`
	Pools            map[string]BurnState `json:"pools,omitempty"`
}

type QuotaEngine struct {
	ExhaustedPct  float64
	AliasProvider func(string) string
}

func NewQuotaEngine() *QuotaEngine {
	return &QuotaEngine{
		ExhaustedPct:  DefaultExhaustedPct,
		AliasProvider: func(name string) string {
			if name == "agy" {
				return "antigravity"
			}
			return name
		},
	}
}

var realWindows = map[int]string{
	Window5h:     "5h",
	WindowWeekly: "weekly",
}

func resetsIn(iso string, now time.Time) *time.Duration {
	if iso == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05Z", iso)
		if err != nil {
			return nil
		}
	}
	d := t.Sub(now)
	return &d
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		return "now"
	}
	s := int(d.Seconds())
	if s < 3600 {
		return fmt.Sprintf("%dm", s/60)
	}
	if s < 86400 {
		h := s / 3600
		m := (s % 3600) / 60
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dd", s/86400)
}

func classPace(used float64, windowSeconds int, resetsAt string, exhaustedPct float64, now time.Time) (BurnClass, int, float64) {
	rin := resetsIn(resetsAt, now)
	if rin == nil || windowSeconds == 0 {
		return BurnUntracked, 100, math.Max(used, 50.0)
	}
	elapsedSeconds := float64(windowSeconds) - math.Max(rin.Seconds(), 0)
	elapsedPct := elapsedSeconds / float64(windowSeconds) * 100.0
	if elapsedPct < 4.0 {
		elapsedPct = 4.0
	}
	pace := int(math.Round(used * 100.0 / elapsedPct))
	pressure := math.Max(used, math.Min(float64(pace), 250.0)/2.0)

	if used >= exhaustedPct {
		return BurnExhausted, pace, math.Max(pressure, 125.0)
	}
	if pace < 85 {
		return BurnUnderspent, pace, pressure
	}
	if pace <= 150 {
		return BurnOnpace, pace, pressure
	}
	return BurnOverpace, pace, pressure
}

func (e *QuotaEngine) ClassPace(used float64, windowSeconds int, resetsAt string) (BurnClass, int, float64) {
	return classPace(used, windowSeconds, resetsAt, e.ExhaustedPct, time.Now())
}

func computeBinding(prov ProviderUsage, resourceNames map[string]bool, exhaustedPct float64, now time.Time) *BurnState {
	var windows []BurnState
	for rk, r := range prov.Resources {
		if resourceNames != nil && !resourceNames[rk] {
			continue
		}
		ws := r.WindowSeconds
		wn, ok := realWindows[ws]
		if !ok {
			continue
		}
		if r.Used == 0 || r.Unit != "percent" {
			continue
		}
		rin := resetsIn(r.ResetsAt, now)
		cls, pace, pressure := classPace(r.Used, ws, r.ResetsAt, exhaustedPct, now)

		var elapsedSeconds float64
		if rin != nil {
			elapsedSeconds = float64(ws) - math.Max(rin.Seconds(), 0)
		} else {
			elapsedSeconds = float64(ws) * 0.04
		}
		if elapsedSeconds < float64(ws)*0.04 {
			elapsedSeconds = float64(ws) * 0.04
		}

		ri := "?"
		if rin != nil {
			ri = humanDuration(*rin)
		}

		var runwayMinutes *int
		var exhaustsBeforeReset *bool
		if r.Used > 0 {
			runway := math.Max(exhaustedPct-r.Used, 0.0) * elapsedSeconds / r.Used
			rm := int(math.Round(runway / 60.0))
			runwayMinutes = &rm
			if rin != nil && rin.Seconds() > 0 {
				ebr := runway < rin.Seconds()
				exhaustsBeforeReset = &ebr
			}
		}

		windows = append(windows, BurnState{
			Resource:      rk,
			Window:        wn,
			WindowSeconds: ws,
			Used:          r.Used,
			Remaining:     math.Max(100-r.Used, 0),
			ResetsAt:      r.ResetsAt,
			ResetsIn:      ri,
			Class:         cls,
			Pace:          pace,
			Pressure:      pressure,
			RunwayMinutes: runwayMinutes,
			ExhaustsBeforeReset: exhaustsBeforeReset,
		})
	}
	if len(windows) == 0 {
		return nil
	}
	sort.Slice(windows, func(i, j int) bool {
		return windows[i].Used > windows[j].Used
	})

	b := windows[0]
	classOrder := map[BurnClass]int{BurnUnderspent: 0, BurnUntracked: 1, BurnOnpace: 2, BurnOverpace: 3, BurnExhausted: 4}
	burn := windows[0]
	for _, w := range windows {
		ci := classOrder[w.Class]
		bi := classOrder[burn.Class]
		if ci > bi || (ci == bi && w.Pressure > burn.Pressure) || (ci == bi && w.Pressure == burn.Pressure && w.Used > burn.Used) {
			burn = w
		}
	}

	var soonestReset *BurnState
	for _, w := range windows {
		if w.ResetsAt != "" && resetsIn(w.ResetsAt, now) != nil {
			if soonestReset == nil || resetsIn(w.ResetsAt, now).Seconds() < resetsIn(soonestReset.ResetsAt, now).Seconds() {
				soonestReset = &w
			}
		}
	}
	if soonestReset == nil {
		soonestReset = &b
	}

	var runwayState *BurnState
	for _, w := range windows {
		if w.ExhaustsBeforeReset != nil && *w.ExhaustsBeforeReset && w.RunwayMinutes != nil {
			if runwayState == nil || *w.RunwayMinutes < *runwayState.RunwayMinutes {
				runwayState = &w
			}
		}
	}

	result := BurnState{
		Used:            b.Used,
		Remaining:       b.Remaining,
		Window:          b.Window,
		WindowSeconds:   b.WindowSeconds,
		Resource:        b.Resource,
		ResetsAt:        b.ResetsAt,
		ResetsIn:        b.ResetsIn,
		SoonestResetWindow: soonestReset.Window,
		SoonestResetIn:  soonestReset.ResetsIn,
		Class:           burn.Class,
		Pace:            burn.Pace,
		Pressure:        burn.Pressure,
		PressureResource: burn.Resource,
		Windows:         windows,
	}
	if runwayState != nil {
		result.ExhaustsBeforeReset = boolPtr(true)
		result.RunwayMinutes = runwayState.RunwayMinutes
		result.RunwayResource = runwayState.Resource
	}
	return &result
}

func poolResources(name string, prov ProviderUsage) map[string]map[string]bool {
	names := make(map[string]bool)
	for n := range prov.Resources {
		names[n] = true
	}
	pools := map[string]map[string]bool{"all": names}

	switch name {
	case "claude":
		generic := make(map[string]bool)
		for n := range names {
			if n != "fable" {
				generic[n] = true
			}
		}
		pools["default"] = generic
		fable := make(map[string]bool)
		for n := range names {
			fable[n] = true
		}
		pools["fable"] = fable
	case "antigravity":
		gemini := make(map[string]bool)
		nonGemini := make(map[string]bool)
		for n := range names {
			lower := strings.ToLower(n)
			if strings.HasPrefix(lower, "gemini") {
				gemini[n] = true
			} else if strings.HasPrefix(lower, "nongemini") {
				nonGemini[n] = true
			}
		}
		if len(gemini) > 0 {
			pools["gemini"] = gemini
		}
		if len(nonGemini) > 0 {
			pools["nonGemini"] = nonGemini
		}
	case "codex":
		def := make(map[string]bool)
		spark := make(map[string]bool)
		for n := range names {
			if strings.Contains(strings.ToLower(n), "spark") {
				spark[n] = true
			} else {
				def[n] = true
			}
		}
		pools["default"] = def
		if len(spark) > 0 {
			pools["spark"] = spark
		}
	}
	return pools
}

func decorate(bs *BurnState, stale bool, plan string, providerError bool, exhaustedPct float64) BurnState {
	if bs == nil {
		reason := "no-quota-data"
		if stale {
			reason = "stale"
		} else if providerError {
			reason = "provider-error"
		}
		return BurnState{
			Available: false,
			Reason:    reason,
			Stale:     stale,
			Plan:      plan,
		}
	}
	reason := "ok"
	if stale {
		reason = "stale"
	} else if providerError {
		reason = "provider-error"
	} else if bs.Used >= exhaustedPct {
		reason = "exhausted"
	}
	bs.Available = !stale && !providerError && bs.Used < exhaustedPct
	bs.Reason = reason
	bs.Stale = stale
	bs.Plan = plan
	return *bs
}

func boolPtr(b bool) *bool { return &b }

func (e *QuotaEngine) ComputeAll(snap *UsageSnapshot) map[string]BurnState {
	if snap == nil {
		return nil
	}
	now := time.Now()
	computed := make(map[string]BurnState)
	for name, prov := range snap.Providers {
		stale := prov.Stale
		plan := prov.Plan
		pools := make(map[string]BurnState)
		providerError := false

		for pool, resources := range poolResources(name, prov) {
			bs := computeBinding(prov, resources, e.ExhaustedPct, now)
			d := decorate(bs, stale, plan, providerError, e.ExhaustedPct)
			if pool != "all" {
				pools[pool] = d
			}
			if pool == "all" {
				top := d
				top.Pools = pools
				computed[name] = top
			}
		}
	}
	return computed
}

func (e *QuotaEngine) PickProvider(computed map[string]BurnState, among []string) (string, BurnState, error) {
	if len(among) == 0 {
		among = []string{"codex", "claude"}
	}
	resolved := make([]string, len(among))
	for i, name := range among {
		resolved[i] = e.AliasProvider(name)
	}

	type ranked struct {
		unknown   int
		runway    int
		pressure  float64
		remaining float64
		reset     time.Duration
		name      string
		state     BurnState
	}

	now := time.Now()
	var candidates []ranked
	for _, name := range resolved {
		p, ok := computed[name]
		if !ok || !p.Available {
			continue
		}
		rin := resetsIn(p.ResetsAt, now)
		var resetDuration time.Duration
		if rin != nil {
			resetDuration = *rin
		} else {
			resetDuration = time.Duration(math.MaxInt64)
		}
		risky := p.ExhaustsBeforeReset != nil && *p.ExhaustsBeforeReset
		unknown := p.ExhaustsBeforeReset == nil
		rwy := 1_000_000_000
		if risky && p.RunwayMinutes != nil {
			rwy = *p.RunwayMinutes
		}
		candidates = append(candidates, ranked{
			unknown:   boolToInt(unknown)*2 + boolToInt(risky)*1,
			runway:    -rwy,
			pressure:  p.Pressure,
			remaining: -p.Remaining,
			reset:     resetDuration,
			name:      name,
			state:     p,
		})
	}

	if len(candidates) == 0 {
		var details []string
		for _, n := range resolved {
			if p, ok := computed[n]; ok {
				details = append(details, fmt.Sprintf("%s=%s", n, p.Reason))
			} else {
				details = append(details, fmt.Sprintf("%s=no-data", n))
			}
		}
		return "", BurnState{}, fmt.Errorf("no available provider among [%s] (%s)",
			strings.Join(resolved, " "), strings.Join(details, "; "))
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].unknown != candidates[j].unknown {
			return candidates[i].unknown < candidates[j].unknown
		}
		if candidates[i].runway != candidates[j].runway {
			return candidates[i].runway < candidates[j].runway
		}
		if candidates[i].pressure != candidates[j].pressure {
			return candidates[i].pressure < candidates[j].pressure
		}
		if candidates[i].remaining != candidates[j].remaining {
			return candidates[i].remaining < candidates[j].remaining
		}
		return candidates[i].reset < candidates[j].reset
	})

	return candidates[0].name, candidates[0].state, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (e *QuotaEngine) ProviderOK(computed map[string]BurnState, provider string) (BurnState, bool) {
	name := e.AliasProvider(provider)
	p, ok := computed[name]
	if !ok {
		return BurnState{}, false
	}
	if p.Reason == "no-quota-data" {
		return p, false
	}
	return p, p.Available
}
