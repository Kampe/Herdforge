package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// runCapacity reports whether THIS host can accept another review launch, as one
// structured answer.
//
// WHY THIS EXISTS
// ---------------
// W4 stopped responding, a remote review failed because herdr was not running,
// and the host came back only after a reboot. At 11:35:15 herdr status was `not
// running`; by 11:37:03 it was running again. In between, `herd-review-remote`
// happily fetched a sha, created a worktree, and launched into a host that had
// no control socket -- because it asked the host NOTHING before mutating it.
//
// The census after the reboot: eight idle Claude reviewers resident at 432-485
// MiB RSS each, one working. Explicit idle reaping released 2 GiB across six
// closures. Configured slots said there was room. The live host disagreed, and
// the live host is the one that runs out of memory.
//
// So the cap is derived from the CENSUS, not from configured slots: what is
// actually resident right now, plus what one more reviewer will actually cost.
//
// UNKNOWN IS NOT A REFUSAL. If memory cannot be read (a platform with no
// /proc/meminfo, an unreadable file), this reports it unknown and lets the other
// gates decide. A capacity gate that refuses whenever it cannot measure is an
// outage generator, and this session has already paid for that lesson twice.
// Only a gate that is actually FALSE refuses.
func runCapacity(args []string) error {
	fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the structured capacity record")
	limit := fs.Int("review-limit", envInt("HERD_REVIEW_CONCURRENCY", 4), "max concurrent live reviewers on this host")
	perReviewer := fs.Int64("reviewer-mib", envInt64("HERD_REVIEWER_RSS_MIB", 512), "expected RSS of one reviewer, in MiB (measured 432-485 on W4)")
	floor := fs.Int64("floor-mib", envInt64("HERD_MEM_FLOOR_MIB", 2048), "memory that must remain free AFTER the new reviewer")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c := decideCapacity(observeCapacity(), *limit, *perReviewer, *floor)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(c); err != nil {
			return err
		}
	} else {
		fmt.Println(c.String())
	}
	if !c.Admit {
		// Fail-closed exit status so a shell caller cannot ignore a refusal by
		// forgetting to parse the JSON it just printed.
		os.Exit(3)
	}
	return nil
}

// CapacityObservation is what was actually measured. Every field that can be
// unmeasurable says so, rather than defaulting to a number that reads as fact.
type CapacityObservation struct {
	HerdrRunning   bool     `json:"herdr_running"`
	HerdrDetail    string   `json:"herdr_detail,omitempty"`
	MemAvailMiB    int64    `json:"mem_available_mib"` // -1 unknown
	SwapUsedMiB    int64    `json:"swap_used_mib"`     // -1 unknown
	AgentsListed   bool     `json:"agents_listed"`
	Agents         int      `json:"agents_total"`
	Reviewers      int      `json:"reviewers_live"`
	ReviewersIdle  int      `json:"reviewers_idle"`
	IdleReviewerID []string `json:"reviewers_idle_names,omitempty"`
}

// Capacity is the observation plus one decision with a named reason.
type Capacity struct {
	CapacityObservation
	ReviewLimit int    `json:"review_limit"`
	NeedMiB     int64  `json:"need_mib"`
	Admit       bool   `json:"admit"`
	Reason      string `json:"reason"`
}

func (c Capacity) String() string {
	verdict := "ADMIT"
	if !c.Admit {
		verdict = "REFUSE"
	}
	mem := "unknown"
	if c.MemAvailMiB >= 0 {
		mem = fmt.Sprintf("%dMiB", c.MemAvailMiB)
	}
	return fmt.Sprintf("%s herdr=%v reviewers=%d/%d idle=%d mem_available=%s need=%dMiB: %s",
		verdict, c.HerdrRunning, c.Reviewers, c.ReviewLimit, c.ReviewersIdle, mem, c.NeedMiB, c.Reason)
}

// decideCapacity is the whole policy, separated from measurement so it is
// testable without a live host.
//
// Gate order matters: report the cause that actually stopped the launch, not
// the first one that happens to be checkable. herdr being down is the cause of
// the incident this exists for, so it is named first.
func decideCapacity(o CapacityObservation, limit int, perReviewerMiB, floorMiB int64) Capacity {
	c := Capacity{CapacityObservation: o, ReviewLimit: limit, NeedMiB: perReviewerMiB + floorMiB}

	switch {
	case !o.HerdrRunning:
		c.Reason = "herdr server is not running on this host: a review launched now creates a worktree and then dies with no pane. " +
			"Start herdr and re-check; do not prepare a candidate first."
		if o.HerdrDetail != "" {
			c.Reason += " (" + o.HerdrDetail + ")"
		}
	case !o.AgentsListed:
		// herdr answered "running" but the census failed. That is a live
		// contradiction, not an empty fleet -- and an empty fleet is exactly
		// what an unparsed census looks like.
		c.Reason = "herdr reports running but the agent census could not be read; refusing rather than treating an unreadable census as an idle host"
	case o.Reviewers >= limit:
		c.Reason = fmt.Sprintf("%d reviewers already live against a cap of %d (live census, not configured slots)", o.Reviewers, limit)
		if o.ReviewersIdle > 0 {
			c.Reason += fmt.Sprintf("; %d of them are idle and reapable: %s",
				o.ReviewersIdle, strings.Join(o.IdleReviewerID, " "))
		}
	case o.MemAvailMiB >= 0 && o.MemAvailMiB < c.NeedMiB:
		c.Reason = fmt.Sprintf("%dMiB available, one reviewer needs %dMiB plus a %dMiB floor", o.MemAvailMiB, perReviewerMiB, floorMiB)
		if o.ReviewersIdle > 0 {
			c.Reason += fmt.Sprintf("; reap %d idle reviewer(s) to recover roughly %dMiB",
				o.ReviewersIdle, int64(o.ReviewersIdle)*perReviewerMiB)
		}
	default:
		c.Admit = true
		c.Reason = "host can host another reviewer"
		if o.MemAvailMiB < 0 {
			c.Reason += " (memory unmeasurable here; admitted on herdr and census alone)"
		}
	}
	return c
}

func observeCapacity() CapacityObservation {
	o := CapacityObservation{MemAvailMiB: -1, SwapUsedMiB: -1}
	o.HerdrRunning, o.HerdrDetail = herdrServerRunning()
	o.MemAvailMiB, o.SwapUsedMiB = hostMemoryMiB()

	agents, err := herdr.AgentList()
	if err != nil {
		o.HerdrDetail = strings.TrimSpace(o.HerdrDetail + " " + err.Error())
		return o
	}
	o.AgentsListed = true
	o.Agents = len(agents)
	for _, a := range agents {
		if !isReviewerAgent(a.Name) {
			continue
		}
		o.Reviewers++
		// "idle" here means resident and doing nothing -- the class the
		// default done-only reaper misses, and the class that held 2 GiB on W4.
		if strings.EqualFold(a.Status, "idle") || strings.EqualFold(a.Status, "done") {
			o.ReviewersIdle++
			o.IdleReviewerID = append(o.IdleReviewerID, a.Name)
		}
	}
	return o
}

// isReviewerAgent matches the launch convention (review-<ref>) on a hyphen
// boundary, so a lane merely named "reviewer-tooling" is not counted as a live
// reviewer and does not silently consume the cap.
func isReviewerAgent(name string) bool {
	return name == "review" || strings.HasPrefix(name, "review-")
}

func herdrServerRunning() (bool, string) {
	out, err := exec.Command("herdr", "status", "server").CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil && text == "" {
		return false, "herdr status server: " + err.Error()
	}
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "status" {
			return strings.TrimSpace(v) == "running", ""
		}
	}
	return false, "herdr status server printed no status line"
}

// hostMemoryMiB reads /proc/meminfo. Returns (-1, -1) where that does not
// exist, which the decision treats as unmeasured rather than as zero.
func hostMemoryMiB() (avail, swapUsed int64) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1, -1
	}
	kb := map[string]int64{}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		kb[k] = n
	}
	avail, swapUsed = -1, -1
	if v, ok := kb["MemAvailable"]; ok {
		avail = v / 1024
	}
	if total, ok := kb["SwapTotal"]; ok {
		swapUsed = (total - kb["SwapFree"]) / 1024
	}
	return avail, swapUsed
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64); err == nil && v > 0 {
		return v
	}
	return def
}
