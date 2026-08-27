package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FAC-609: memory is not the only thing that can stop a reviewer launching.
//
// Right after FAC-587 taught this host to measure its own RAM, capacity
// reported:
//
//	review_limit = 6   reviewers_live = 0   "host can host another reviewer"
//
// while every native provider surface was at concurrency cap: claude live=2
// cap=2, grok live=4 cap=4, codex over cap, antigravity exhausted. Two review
// launches failed and were retried per the documented twice-before-concluding
// rule. Both failed for the same real reason: there was no launchable surface.
//
// So the host offered six slots the fleet could not fill one of. The
// memory-derived ceiling and the provider concurrency ceiling are two
// independent authorities and only one was consulted. Reporting headroom that
// cannot be used sends a coordinator into a retry loop against a wall -- the
// same shape as the pre-#618 standing-capacity mirage, one layer over.
//
// This is NOT a regression from FAC-587. Before it, the limit was 1 and the
// disagreement was masked because the smaller number happened to be right for
// the wrong reason.

// launchable reports how many reviewer launches the provider layer can
// currently accept.
//
// Known=false means the router could not be consulted. That stays UNKNOWN and
// must NOT reduce the ceiling: refusing because a secondary authority is
// unreadable is exactly the outage generator this file warns about elsewhere.
// Only a router that positively answers "zero" may bind the limit.
type launchable struct {
	Slots   int
	Known   bool
	Detail  string
	Binding bool
}

// routeDoctorTimeout bounds an EXPLICIT refresh. Measured on this host, the
// doctor takes 20-47s because it queries live quota per provider.
const routeDoctorTimeout = 90 * time.Second

// launchableMaxAge is how long a cached reading stays usable. Concurrency moves
// when panes start and stop, so an old reading is worse than none: it would
// bind the ceiling on state that has already changed.
const launchableMaxAge = 3 * time.Minute

// launchableCacheEnv overrides where the reading is cached (tests).
const launchableCacheEnv = "HERD_LAUNCHABLE_CACHE"

type launchableCache struct {
	ObservedAt time.Time `json:"observed_at"`
	Slots      int       `json:"slots"`
	Detail     string    `json:"detail"`
}

// launchableReviewSlots consults the router for live provider concurrency.
//
// Parses `herdr-route --doctor` lines of the shape:
//
//	claude  READY  69  claude-sonnet-5  available quota=... live=1 cap=2
//	grok    SKIP   19  grok-4.5         at concurrency cap live=4 cap=4
//
// Slots are summed per SURFACE (first field), not per model: three claude model
// rows share one claude concurrency pool, and adding them would re-invent the
// overstatement this fixes.
// launchableReviewSlots reads the CACHED provider-concurrency reading.
//
// It never probes inline. Measured on this host the router doctor takes 20-47s
// because it queries live quota per provider; calling that from an admission
// check would make `herd capacity` itself hang past an operator's bounded
// window -- reintroducing the exact rc=124 silence FAC-607 just removed, from a
// different direction. The first version of this file did precisely that and
// was caught by timing it rather than by assuming it was cheap.
//
// A missing or stale cache is UNKNOWN and never reduces the ceiling.
func launchableReviewSlots() launchable {
	path := launchableCachePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return launchable{Known: false, Detail: "no cached provider-concurrency reading (run `herd capacity --refresh-launchable`)"}
	}
	var c launchableCache
	if err := json.Unmarshal(raw, &c); err != nil {
		return launchable{Known: false, Detail: "cached provider-concurrency reading is unreadable: " + err.Error()}
	}
	if age := time.Since(c.ObservedAt); age > launchableMaxAge {
		return launchable{Known: false,
			Detail: "cached provider-concurrency reading is " + age.Round(time.Second).String() +
				" old (max " + launchableMaxAge.String() + "); concurrency has probably moved since"}
	}
	return launchable{Slots: c.Slots, Known: true, Detail: c.Detail + " (cached)"}
}

// refreshLaunchable runs the slow router probe deliberately and caches it.
func refreshLaunchable() (launchable, error) {
	path, err := exec.LookPath("herdr-route")
	if err != nil {
		return launchable{Known: false, Detail: "herdr-route not on PATH"}, err
	}
	out, err := runWithTimeout(path, routeDoctorTimeout, "--doctor")
	if err != nil {
		return launchable{Known: false, Detail: "herdr-route --doctor did not answer: " + err.Error()}, err
	}
	l := parseDoctorSurfaces(out)
	if !l.Known {
		return l, nil
	}
	body, err := json.MarshalIndent(launchableCache{ObservedAt: time.Now(), Slots: l.Slots, Detail: l.Detail}, "", "  ")
	if err != nil {
		return l, err
	}
	dst := launchableCachePath()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return l, err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return l, err
	}
	return l, os.Rename(tmp, dst)
}

func launchableCachePath() string {
	if p := strings.TrimSpace(os.Getenv(launchableCacheEnv)); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".herd", "run", "launchable.json")
	}
	return filepath.Join(home, ".herd", "run", "launchable.json")
}

// parseDoctorSurfaces turns router doctor output into a launchable reading.
//
// Split from the exec deliberately: this is where the judgement lives, so it
// must be testable without a router on PATH. A test that could only run where
// herdr-route happens to be installed would prove nothing on CI and would be
// skipped into uselessness.
func parseDoctorSurfaces(out string) launchable {
	best := map[string]int{}
	seen := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		surface := fields[0]
		state := fields[1]
		if state != "READY" && state != "SKIP" {
			continue
		}
		seen = true
		free := 0
		if state == "READY" {
			live, capacity, ok := parseLiveCap(line)
			if !ok {
				// READY without a parsable cap still means launchable; assume
				// one rather than zero. Under-reporting a healthy surface would
				// manufacture the refusal this exists to prevent.
				free = 1
			} else if capacity > live {
				free = capacity - live
			}
		}
		if free > best[surface] {
			best[surface] = free
		}
	}
	if !seen {
		// Output we cannot parse is UNKNOWN, never a confident zero. A router
		// whose format changed must not read as "nothing can launch".
		return launchable{Known: false, Detail: "herdr-route --doctor produced no recognisable surface rows"}
	}

	total := 0
	for _, n := range best {
		total += n
	}
	return launchable{Slots: total, Known: true,
		Detail: "provider concurrency reports " + strconv.Itoa(total) + " launchable slot(s)"}
}

func parseLiveCap(line string) (live, capacity int, ok bool) {
	var gotLive, gotCap bool
	for _, f := range strings.Fields(line) {
		switch {
		case strings.HasPrefix(f, "live="):
			if n, err := strconv.Atoi(strings.TrimPrefix(f, "live=")); err == nil {
				live, gotLive = n, true
			}
		case strings.HasPrefix(f, "cap="):
			if n, err := strconv.Atoi(strings.TrimPrefix(f, "cap=")); err == nil {
				capacity, gotCap = n, true
			}
		}
	}
	return live, capacity, gotLive && gotCap
}

// applyLaunchable binds the reported slots to what a provider can actually
// accept, and says which authority is binding.
//
// An operator staring at capacity=0 needs to know whether to free memory or
// free a pane; those are different actions and the old output could not tell
// them apart.
func applyLaunchable(c *Capacity, l launchable) {
	if !l.Known {
		if l.Detail != "" {
			c.LaunchableDetail = l.Detail + " (unknown, not treated as a refusal)"
		}
		return
	}
	c.LaunchableSlots = l.Slots
	c.LaunchableKnown = true
	c.LaunchableDetail = l.Detail
	if l.Slots < c.AvailableSlots {
		c.AvailableSlots = l.Slots
		c.LaunchableBinding = true
	}
	if c.AvailableSlots == 0 && c.Admit {
		c.Admit = false
		c.Reason = "no provider surface can accept a reviewer launch right now: " + l.Detail +
			"; memory is not the constraint, provider concurrency is -- free a pane or wait, do not free memory"
	}
}

// runWithTimeout runs a command with a bound and returns combined output.
//
// The router is a SECONDARY authority. If it hangs, capacity must still answer
// -- a probe that can block the primary answer would be the same silence
// FAC-607 exists to prevent, arriving from a different direction.
func runWithTimeout(path string, budget time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	return string(out), err
}
