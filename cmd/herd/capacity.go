package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
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
// WHAT A REVIEWER ACTUALLY COSTS (FAC-686, from the kernel journal)
//
// The first version of this file sized a reviewer at 512MiB, taken from a live
// census showing Claude agents at 432-485MiB RSS. That number was measured
// correctly and was still wrong, because it measured the wrong thing: those were
// IDLE agents, and an agent's RSS is not an agent's cost. Each review spawns a
// Bun/Node toolchain whose heap dwarfs the agent that started it.
//
// The kernel journal from the degraded session settles it. At 12:19 a Bun worker
// hit a page-allocation failure with:
//
//	~41GB anonymous memory in use against a 48GB VM ceiling
//	5GB swap consumed, 68MB in active writeback -- the VM was thrashing
//	41MB free in the Normal zone, exactly at the minimum watermark
//	zero contiguous blocks >= 512KB (severe fragmentation)
//	1.4GB of page tables
//
// So the failure WAS memory, and this gate would have admitted every one of
// those launches while sizing them at an eighth of their real cost. A gate is
// only as good as the number it is built on.
//
// Hence 4096MiB per reviewer and a 6144MiB floor: the floor exists so sshd and
// herdr keep enough to accept a connection, which is precisely what stopped
// working (ssh could not complete a banner exchange). An eviction that takes
// out the control plane costs more than the review it was protecting.
//
// FRAGMENTATION IS NOT VISIBLE IN MemAvailable. The zone was at its watermark
// with no high-order blocks left while MemAvailable still looked survivable, so
// swap-in-use is recorded and gated on below: a host that has begun swapping is
// already degrading, and that is a far earlier signal than free bytes.
//
// Process, thread and fd-limit counts are recorded but deliberately NOT a
// refusal condition. 1.4GB of page tables says process count mattered too, but
// nobody has established the threshold where this host breaks, and inventing one
// would repeat the exact mistake the 512MiB figure already made -- guarding a
// number because it was easy to read rather than because it was the one that
// mattered. Record first; gate when there is evidence to gate on.
//
// UNKNOWN IS NOT A REFUSAL. If memory cannot be read (a platform with no
// /proc/meminfo, an unreadable file), this reports it unknown and lets the other
// gates decide. A capacity gate that refuses whenever it cannot measure is an
// outage generator, and this session has already paid for that lesson twice.
// Only a gate that is actually FALSE refuses.
func runCapacity(args []string) error {
	fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the structured capacity record")
	limit := fs.Int("review-limit", 0, "max concurrent live reviewers; 0 derives it from this host's memory budget")
	perReviewer := fs.Int64("reviewer-mib", envInt64("HERD_REVIEWER_RSS_MIB", 4096), "expected TOTAL cost of one reviewer including the toolchain it spawns, in MiB")
	floor := fs.Int64("floor-mib", envInt64("HERD_MEM_FLOOR_MIB", 6144), "memory that must remain free AFTER the new reviewer, for sshd/herdr and the host OS")
	claim := fs.Bool("claim", false, "hold an admission lease across the caller's launch, so concurrent callers cannot all pass the same check")
	holdFor := fs.Duration("claim-ttl", defaultAdmissionLeaseTTL, "how long a claimed admission stays held before it expires")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// FAC-686: the check alone is a TOCTOU gate. A review supervisor launched
	// FOUR concurrent reviews onto W4; each preflight ran against a census that
	// did not yet contain the other three, so every one of them saw room and all
	// four were admitted. The host then degraded until SSH stopped completing a
	// banner exchange. A gate that every racer passes is not a cap.
	//
	// --claim serializes admission on the host itself: the caller holds an
	// exclusive lease for the window between "capacity says yes" and the new
	// reviewer actually appearing in the census. Leases are host-local by
	// construction because this command runs ON the review host over ssh, which
	// is the only place the census is authoritative.
	if *claim {
		release, held, err := holdAdmissionLease(*holdFor)
		if err != nil {
			return err
		}
		if !held {
			c := Capacity{ReviewLimit: *limit, NeedMiB: *perReviewer + *floor}
			c.Reason = "another launch holds the admission lease on this host; serialize rather than racing it. " +
				"Retry after it resolves, or pass --claim-ttl if a legitimate launch needs longer."
			emitCapacity(c, *asJSON)
			os.Exit(3)
		}
		defer release()
	}

	obs := observeCapacity()
	// FAC-686: 4 was a guess. A slot ceiling has to be arithmetic on the host's
	// real memory, or it is the 512MiB mistake again in a different variable:
	// a number that looks like a policy and is actually a hope.
	//
	// slots x per-reviewer <= budget, where the budget is a FRACTION of total
	// RAM rather than all of it. The incident ran to ~41GB of 48GB (85%) before
	// the kernel could not find a page; a host is already unwell well before its
	// last byte. 50% leaves room for page cache, the control plane, and -- on
	// WSL2 -- the Windows host that shares the machine.
	effectiveLimit := *limit
	if effectiveLimit <= 0 {
		effectiveLimit = derivedReviewLimit(obs.MemTotalMiB, *perReviewer)
	}

	c := decideCapacity(obs, effectiveLimit, *perReviewer, *floor)
	c.LimitDerived = *limit <= 0

	emitCapacity(c, *asJSON)
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
	HerdrRunning bool   `json:"herdr_running"`
	HerdrDetail  string `json:"herdr_detail,omitempty"`
	MemTotalMiB  int64  `json:"mem_total_mib"`     // -1 unknown
	MemAvailMiB  int64  `json:"mem_available_mib"` // -1 unknown
	SwapUsedMiB  int64  `json:"swap_used_mib"`
	SwapTotalMiB int64  `json:"swap_total_mib"`
	// PressurePct is PSI "some avg10" for memory: the share of the last 10s
	// that work stalled waiting on memory. -1 where PSI is unavailable.
	PressurePct float64 `json:"memory_pressure_pct"` // -1 unknown
	// Processes/Threads/FDs, not RSS, are the plausible binding constraints
	// here -- see the note on hostProcessLoad. -1 means unmeasured.
	Processes int `json:"processes"`
	Threads   int `json:"threads"`
	FDLimit   int `json:"fd_limit"`
	// HarnessCapped states whether the harness this host launches is actually
	// wrapped in a memory-bounded scope. Reported, never assumed: "we wired
	// run-capped" is a claim, and a claim is not a cgroup.
	// MemorySource is freshness's own sentence about the memory reading, so an
	// unmeasured host reports WHY rather than rendering -1 as a number.
	MemorySource  string `json:"memory_source,omitempty"`
	HarnessCapped bool   `json:"harness_capped"`
	HarnessPath   string `json:"harness_path,omitempty"`
	AgentsListed  bool   `json:"agents_listed"`
	Agents        int    `json:"agents_total"`
	Reviewers     int    `json:"reviewers_live"`
	ReviewersIdle int    `json:"reviewers_idle"`
	// Blocked reviewers hold a slot and make no progress. They are neither
	// healthy work nor reapable garbage, and counting them only in the live
	// total hides the exact accumulation that preceded the W4 incident.
	ReviewersBlocked  int      `json:"reviewers_blocked"`
	BlockedReviewerID []string `json:"reviewers_blocked_names,omitempty"`
	IdleReviewerID    []string `json:"reviewers_idle_names,omitempty"`
}

// Capacity is the observation plus one decision with a named reason.
//
// FAC-584: schema_version and observed_at make this one stable JSON document a
// remote launcher can gate on without scraping prose. available_slots is the
// live remainder after the census, not the configured pool size.
type Capacity struct {
	SchemaVersion int    `json:"schema_version"`
	ObservedAt    string `json:"observed_at"`
	CapacityObservation
	ReviewLimit    int    `json:"review_limit"`
	LimitDerived   bool   `json:"review_limit_derived"`
	AvailableSlots int    `json:"available_slots"`
	NeedMiB        int64  `json:"need_mib"`
	Admit          bool   `json:"admit"`
	Reason         string `json:"reason"`
}

// capacitySchemaVersion is the JSON contract for `herd capacity --json` and the
// review --pool preflight gate. Bump when a consumer field is renamed or
// removed; additive fields do not require a bump.
const capacitySchemaVersion = 1

func (c Capacity) String() string {
	verdict := "ADMIT"
	if !c.Admit {
		verdict = "REFUSE"
	}
	mem := "unknown"
	if c.MemAvailMiB >= 0 {
		mem = fmt.Sprintf("%dMiB", c.MemAvailMiB)
	}
	procs := "unknown"
	if c.Processes >= 0 {
		procs = fmt.Sprintf("%d/%dthr", c.Processes, c.Threads)
	}
	return fmt.Sprintf("%s herdr=%v reviewers=%d/%d idle=%d blocked=%d mem_available=%s procs=%s need=%dMiB: %s",
		verdict, c.HerdrRunning, c.Reviewers, c.ReviewLimit, c.ReviewersIdle, c.ReviewersBlocked, mem, procs, c.NeedMiB, c.Reason)
}

// decideCapacity is the whole policy, separated from measurement so it is
// testable without a live host.
//
// Gate order matters: report the cause that actually stopped the launch, not
// the first one that happens to be checkable. herdr being down is the cause of
// the incident this exists for, so it is named first.
// FAC-693: this gate used to refuse whenever swap USE exceeded 512MiB. That
// permanently fenced a healthy host.
//
// Swap use is HYSTERETIC. Linux does not fault pages back in when memory frees
// up -- they sit in swap until something touches them, or until swapoff. So
// after the 2026-08-26 incident W4 carried 1.7GB of stale swap while completely
// idle, and the gate refused every launch. Measured at that moment:
//
//	pswpin/pswpout   UNCHANGED over 5s -- zero paging activity
//	PSI some avg10   0.26%             -- no pressure at all
//	MemAvailable     23.9GB
//	reviewers        0
//
// The host was fine. The gate was reading a scar as a wound, and nothing the
// fleet could do would clear it -- only a manual swapoff. A gate nobody can
// satisfy is an outage, not a safety property.
//
// The phenomenon is a RATE, so measure the rate. PSI reports the share of time
// work is stalled waiting on memory, which is exactly "is memory hurting right
// now". 20% matches the operator's own alerting guidance for this host.
const memoryPressurePct = 20

// swapExhaustedPct keeps a LEVEL check only as a far backstop: swap genuinely
// near full is real exhaustion rather than residue, and there is nowhere left
// to page into.
const swapExhaustedPct = 75

func decideCapacity(o CapacityObservation, limit int, perReviewerMiB, floorMiB int64) Capacity {
	c := Capacity{
		SchemaVersion:       capacitySchemaVersion,
		ObservedAt:          time.Now().UTC().Format(time.RFC3339Nano),
		CapacityObservation: o,
		ReviewLimit:         limit,
		NeedMiB:             perReviewerMiB + floorMiB,
	}
	if limit > o.Reviewers {
		c.AvailableSlots = limit - o.Reviewers
	}

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
		if o.ReviewersBlocked > 0 {
			c.Reason += fmt.Sprintf("; %d are BLOCKED and holding slots without progressing: %s",
				o.ReviewersBlocked, strings.Join(o.BlockedReviewerID, " "))
		}
		if o.ReviewersIdle > 0 {
			c.Reason += fmt.Sprintf("; %d of them are idle and reapable: %s",
				o.ReviewersIdle, strings.Join(o.IdleReviewerID, " "))
		}
	case o.PressurePct >= memoryPressurePct:
		// Memory PRESSURE, not swap residue. PSI reports the share of time work
		// is stalled waiting on memory, which is the thing that actually hurts.
		c.Reason = fmt.Sprintf("host is under memory pressure (PSI some avg10=%.2f%%, threshold %.0f%%): work is stalling on memory now",
			o.PressurePct, float64(memoryPressurePct))
		if o.ReviewersIdle > 0 {
			c.Reason += fmt.Sprintf("; reap %d idle reviewer(s) first", o.ReviewersIdle)
		}
	case o.SwapTotalMiB > 0 && o.SwapUsedMiB*100/o.SwapTotalMiB >= swapExhaustedPct:
		// Level, kept ONLY as a far backstop: swap genuinely near full is real
		// exhaustion, not residue.
		c.Reason = fmt.Sprintf("swap is %d%% consumed (%dMiB of %dMiB): the host is out of room to page into",
			o.SwapUsedMiB*100/o.SwapTotalMiB, o.SwapUsedMiB, o.SwapTotalMiB)
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
		if o.HarnessPath != "" && !o.HarnessCapped {
			// Deliberately a WARNING, not a refusal. Refusing would fence every
			// host that has not adopted the wrapper yet, which trades a bounded
			// risk for a certain outage -- and the memory, swap and derived-slot
			// gates already bound the aggregate. But it must be VISIBLE, because
			// the whole failure mode was a per-agent heap nobody was limiting.
			c.Reason += "; WARNING harness " + o.HarnessPath + " is NOT memory-capped, so a single runaway review is bounded only by this host's total RAM"
		}
	}
	return c
}

func observeCapacity() CapacityObservation {
	o := CapacityObservation{MemTotalMiB: -1, MemAvailMiB: -1, SwapUsedMiB: -1, SwapTotalMiB: -1, PressurePct: -1, Processes: -1, Threads: -1, FDLimit: -1}
	o.HerdrRunning, o.HerdrDetail = herdrServerRunning()
	// FAC-690: the memory reading now goes through pkg/freshness rather than
	// three hand-rolled -1 sentinels. That package exists so an UNKNOWN cannot
	// be read as a value by forgetting to check the second return -- which is
	// exactly the mistake -1 invites, because -1 IS a value and every consumer
	// has to remember it means "nothing was measured".
	mem := readHostMemory()
	if v, ok := mem.Value(); ok {
		o.MemTotalMiB, o.MemAvailMiB, o.SwapUsedMiB = v.TotalMiB, v.AvailMiB, v.SwapUsedMiB
	} else {
		o.MemTotalMiB, o.MemAvailMiB, o.SwapUsedMiB = -1, -1, -1
	}
	o.SwapTotalMiB = hostSwapTotalMiB()
	o.PressurePct = hostMemoryPressurePct()
	o.MemorySource = mem.MustExplain(time.Now())
	o.Processes, o.Threads, o.FDLimit = hostProcessLoad()
	o.HarnessCapped, o.HarnessPath = harnessIsMemoryCapped("claude")

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
		switch {
		case strings.EqualFold(a.Status, "idle"), strings.EqualFold(a.Status, "done"):
			o.ReviewersIdle++
			o.IdleReviewerID = append(o.IdleReviewerID, a.Name)
		case strings.EqualFold(a.Status, "blocked"):
			// NOT reaped here. Blocked usually means waiting on a permission
			// prompt, which is recoverable -- closing it would throw away a
			// review that only needed an answer. It is reported so a slot held
			// by stalled work is visible instead of looking like throughput.
			o.ReviewersBlocked++
			o.BlockedReviewerID = append(o.BlockedReviewerID, a.Name)
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

// hostMemoryMiB reads /proc/meminfo, falling back to the Darwin interfaces.
// Returns (-1, -1) where neither exists, which the decision treats as
// unmeasured rather than as zero.
func hostMemoryMiB() (total, avail, swapUsed int64) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		if runtime.GOOS == "darwin" {
			return darwinMemoryMiB()
		}
		return -1, -1, -1
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
	total, avail, swapUsed = -1, -1, -1
	if v, ok := kb["MemTotal"]; ok {
		total = v / 1024
	}
	if v, ok := kb["MemAvailable"]; ok {
		avail = v / 1024
	}
	if total, ok := kb["SwapTotal"]; ok {
		swapUsed = (total - kb["SwapFree"]) / 1024
	}
	return total, avail, swapUsed
}

// darwinMemoryMiB measures total and available memory on macOS.
//
// FAC-718: a Mac reports no /proc/meminfo, so hostMemoryMiB returned -1 and
// derivedReviewLimit collapsed the ceiling to ONE reviewer for the whole host.
// That is not the documented "unknown is not a refusal" behaviour -- it refuses
// every reviewer after the first, which is how a single settled reviewer of
// mine blocked another project's admission entirely. The host has 48GiB; the
// cap was 1 because nobody had taught it to look.
//
// Swap is deliberately returned UNMEASURED rather than read from
// vm.swapusage. macOS pages into a dynamically grown, compressed swap file as
// normal operation, so its swap LEVEL is not the same phenomenon as the Linux
// exhaustion this host's 75% backstop was calibrated against -- this machine
// sits at 87% while completely healthy. Reporting it would fire that gate and
// refuse every review, turning a too-small cap into a total outage. Declining
// to map a differently-defined metric onto a threshold calibrated for another
// one is the honest reading, not a gap being hidden: MemAvailable still gates,
// so a genuinely exhausted Mac is still refused on the measurement that means
// the same thing on both platforms.
func darwinMemoryMiB() (total, avail, swapUsed int64) {
	total, avail, swapUsed = -1, -1, -1

	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		if b, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && b > 0 {
			total = b / 1024 / 1024
		}
	}

	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return total, avail, swapUsed
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) == 0 {
		return total, avail, swapUsed
	}
	// "Mach Virtual Memory Statistics: (page size of 16384 bytes)"
	_, sizePart, ok := strings.Cut(lines[0], "page size of ")
	if !ok {
		return total, avail, swapUsed
	}
	pageBytes, err := strconv.ParseInt(strings.Fields(sizePart)[0], 10, 64)
	if err != nil || pageBytes <= 0 {
		return total, avail, swapUsed
	}

	// Free plus the pages the kernel can reclaim without paging anything out.
	// This is the closest analogue to Linux MemAvailable; counting only "Pages
	// free" would understate reclaimable memory by an order of magnitude on a
	// warm machine and refuse reviewers a healthy host can seat.
	var pages int64
	for _, key := range []string{"Pages free", "Pages inactive", "Pages speculative", "Pages purgeable"} {
		for _, line := range lines {
			k, v, ok := strings.Cut(line, ":")
			if !ok || strings.TrimSpace(k) != key {
				continue
			}
			n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(v), "."), 10, 64)
			if err == nil {
				pages += n
			}
			break
		}
	}
	if pages > 0 {
		avail = pages * pageBytes / 1024 / 1024
	}
	return total, avail, swapUsed
}

// derivedReviewLimit sizes the slot ceiling from the host's own memory.
//
// Returns 1 when total memory is unmeasurable: unknown must not read as
// unlimited. One reviewer still makes progress, and the memory/swap gates
// remain in front of it.
func derivedReviewLimit(memTotalMiB, perReviewerMiB int64) int {
	if memTotalMiB <= 0 || perReviewerMiB <= 0 {
		return 1
	}
	pct := int64(envInt("HERD_REVIEW_BUDGET_PCT", 50))
	if pct <= 0 || pct > 100 {
		pct = 50
	}
	slots := (memTotalMiB * pct / 100) / perReviewerMiB
	if slots < 1 {
		return 1
	}
	return int(slots)
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

func emitCapacity(c Capacity, asJSON bool) {
	if c.SchemaVersion == 0 {
		c.SchemaVersion = capacitySchemaVersion
	}
	if c.ObservedAt == "" {
		c.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(c)
		return
	}
	fmt.Println(c.String())
}

// poolCapacityObserve is the live census behind the review --pool gate.
// Tests replace it so admission policy can be proven without a live host.
var poolCapacityObserve = observeCapacity

// acquirePoolCapacityOrRefuse is the FAC-584 launch gate: refuse BEFORE any
// candidate resolution or worktree preparation when this host cannot safely
// admit another reviewer. It holds the host-local admission lease across the
// caller's preparation+launch window so concurrent pool launches cannot each
// pass the same census (FAC-686).
//
// Remote launchers (herd-review-remote and siblings) must run the same check on
// the review host before creating a remote worktree:
//
//	ssh <review-host> herd capacity --json --claim
//
// A non-zero exit or admit=false means do not prepare the candidate.
func acquirePoolCapacityOrRefuse() (release func(), err error) {
	perReviewer := envInt64("HERD_REVIEWER_RSS_MIB", 4096)
	floor := envInt64("HERD_MEM_FLOOR_MIB", 6144)
	ttl := defaultAdmissionLeaseTTL
	if v := strings.TrimSpace(os.Getenv("HERD_ADMISSION_LEASE_TTL")); v != "" {
		if d, parseErr := time.ParseDuration(v); parseErr == nil && d > 0 {
			ttl = d
		}
	}

	rel, held, leaseErr := holdAdmissionLease(ttl)
	if leaseErr != nil {
		return nil, fmt.Errorf("review --pool capacity gate: admission lease: %w", leaseErr)
	}
	if !held {
		return nil, fmt.Errorf("review --pool REFUSING before candidate preparation: another launch holds the admission lease on this host; serialize rather than racing it")
	}

	obs := poolCapacityObserve()
	limit := derivedReviewLimit(obs.MemTotalMiB, perReviewer)
	c := decideCapacity(obs, limit, perReviewer, floor)
	c.LimitDerived = true
	if !c.Admit {
		rel()
		return nil, fmt.Errorf("review --pool REFUSING before candidate preparation: %s", c.Reason)
	}
	fmt.Printf("review --pool capacity: %s\n", c.String())
	return rel, nil
}

// admissionLeasePath is host-local on purpose: the census this lease protects is
// only authoritative on the machine that owns it.
func admissionLeasePath() string {
	if p := strings.TrimSpace(os.Getenv("HERD_ADMISSION_LEASE_PATH")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "herd-admission.lease")
	}
	return filepath.Join(home, ".herd", "state", "admission.lease")
}

// holdAdmissionLease takes an exclusive, self-expiring admission lease.
//
// O_EXCL create is the whole mechanism: it is atomic on every filesystem that
// matters here, so two simultaneous launches cannot both believe they hold it.
//
// The lease EXPIRES rather than requiring a clean release. A launch that is
// killed mid-flight would otherwise fence the host permanently, which converts
// a crash into an outage -- the same trade this codebase keeps getting wrong in
// the other direction.
// defaultAdmissionLeaseTTL must exceed the work it guards, or the guard
// expires mid-launch and a second launcher is admitted while the first is still
// preparing.
//
// FAC-713: this was 180s. Route resolution alone was MEASURED at 29-272s before
// FAC-679 cached the quota read, so the lease could expire during the very
// operation it exists to serialize -- and it did, which is how the reviewer
// reproduced a third concurrent launcher. A TTL shorter than its critical
// section is not a short lease, it is no lease.
//
// 600s is deliberately generous. The cost of a too-long TTL is a delayed
// retry after a crash; the cost of a too-short one is concurrent launches on a
// host that just fell over from concurrent launches.
const defaultAdmissionLeaseTTL = 600 * time.Second

func holdAdmissionLease(ttl time.Duration) (release func(), held bool, err error) {
	path := admissionLeasePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}

	// FAC-713: an OWNERSHIP TOKEN, not just a file.
	//
	// Review finding on 1d99c2ef6c4f, reproduced: release() removed the lease
	// file unconditionally. If holder A's lease expired and B reclaimed it, A's
	// deferred release deleted B's lease and a third launcher C could take it --
	// three concurrent launchers from a gate whose entire purpose is to admit
	// one. That is worse than no lease, because the gate reports success while
	// serializing nothing.
	//
	// The token is what makes release safe: a holder may only delete the lease
	// it still owns.
	token := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

	write := func() (bool, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := fmt.Fprintf(f, "token=%s pid=%d taken=%s ttl=%s\n",
				token, os.Getpid(), time.Now().UTC().Format(time.RFC3339), ttl)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			return werr == nil, werr
		}
		if !os.IsExist(err) {
			return false, err
		}
		return false, nil
	}

	ok, err := write()
	if err != nil {
		return nil, false, err
	}
	if !ok {
		// Expired lease: reclaim. Age is read from the file, not remembered, so
		// a lease outlives the process that took it.
		if fi, statErr := os.Stat(path); statErr == nil && time.Since(fi.ModTime()) > ttl {
			_ = os.Remove(path)
			if ok, err = write(); err != nil {
				return nil, false, err
			}
		}
	}
	if !ok {
		return nil, false, nil
	}

	return func() { releaseAdmissionLease(path, token) }, true, nil
}

// releaseAdmissionLease removes the lease ONLY if this holder still owns it.
//
// A holder whose lease already expired and was reclaimed by someone else must
// not delete the new owner's lease on its way out. Silence is correct here:
// losing the lease is a normal outcome of running past TTL, and the launch it
// guarded has already happened either way.
func releaseAdmissionLease(path, token string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if !strings.Contains(string(raw), "token="+token+" ") {
		// Someone else owns it now. Deleting it would admit a third launcher.
		return
	}
	_ = os.Remove(path)
}

// hostProcessLoad counts processes and threads, and reads the fd ceiling.
//
// Recorded rather than gated: see the CORRECTION note at the top of this file.
// These are the numbers a banner-exchange failure would actually be explained
// by, and none of them were captured during the incident that needed them.
//
// (-1, -1, -1) where /proc is unavailable, which is unmeasured, not zero.
func hostProcessLoad() (processes, threads, fdLimit int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1, -1, -1
	}
	processes, threads = 0, 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		processes++
		if tasks, err := os.ReadDir(filepath.Join("/proc", e.Name(), "task")); err == nil {
			threads += len(tasks)
		}
	}
	fdLimit = -1
	if raw, err := os.ReadFile("/proc/sys/fs/file-max"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			fdLimit = n
		}
	}
	return processes, threads, fdLimit
}

// harnessIsMemoryCapped reports whether the harness this host would launch is
// wrapped in a memory-bounded scope (systemd-run/run-capped).
//
// herdr resolves the harness executable itself from --kind and offers no
// command override, so the only place a cap can be applied is the binary PATH
// resolves to. That makes "is it capped" a question about a file, and one worth
// answering out loud: run-capped existing on a host proves nothing about
// whether anything uses it. On W4 it was installed and the harness was not
// wrapped, so every launch was still unbounded.
//
// Returns ("", false) when the harness is absent, which is unmeasured rather
// than uncapped, and reads as no warning.
func harnessIsMemoryCapped(kind string) (capped bool, path string) {
	resolved, err := exec.LookPath(kind)
	if err != nil {
		return false, ""
	}
	body, err := os.ReadFile(resolved)
	if err != nil {
		// A real ELF we cannot classify. Do not claim it is uncapped.
		return false, ""
	}
	text := string(body)
	if len(text) > 8192 {
		text = text[:8192]
	}
	for _, marker := range []string{"run-capped", "systemd-run", "MemoryMax"} {
		if strings.Contains(text, marker) {
			return true, resolved
		}
	}
	return false, resolved
}

// hostMemory is one coherent memory observation. The three numbers come from a
// single /proc/meminfo read, so they must succeed or fail together -- reporting
// a total without an available is a half-truth the caller cannot use.
type hostMemory struct {
	TotalMiB    int64
	AvailMiB    int64
	SwapUsedMiB int64
}

// readHostMemory wraps the /proc/meminfo read in a freshness.Reading.
//
// An unreadable /proc/meminfo is UNKNOWN, not zero and not -1. Value() then
// returns ok=false, so a consumer cannot accidentally treat "we could not
// measure this host" as "this host has no memory available" -- which would
// refuse every launch on a platform that simply does not expose /proc.
func readHostMemory() freshness.Reading[hostMemory] {
	total, avail, swapUsed := hostMemoryMiB()
	now := time.Now()
	// FAC-718: name the interface the number actually came from. Reporting a
	// Darwin sysctl reading as "/proc/meminfo: fresh" is the same class of
	// defect as every other mislabelled source this control plane has been
	// bitten by -- an operator checking why a cap moved would go read a file
	// that does not exist on the host that produced the number.
	source := "/proc/meminfo"
	if runtime.GOOS == "darwin" {
		source = "sysctl hw.memsize + vm_stat"
	}
	if total < 0 && avail < 0 {
		return freshness.Degrade[hostMemory](
			freshness.Reading[hostMemory]{}, source,
			fmt.Errorf("no %s on this host", source),
			"run the capacity check on the review host itself; memory gating is skipped where it cannot be measured")
	}
	return freshness.Fresh(now.Format(time.RFC3339)+" "+source, now,
		hostMemory{TotalMiB: total, AvailMiB: avail, SwapUsedMiB: swapUsed})
}

// hostMemoryPressurePct reads PSI "some avg10" for memory: the share of the
// last ten seconds that some task stalled waiting on memory.
//
// Returns -1 where PSI is unavailable, which the decision treats as unmeasured
// and therefore NOT a refusal. A kernel without PSI must not be fenced for
// lacking an instrument.
func hostMemoryPressurePct() float64 {
	raw, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			k, v, ok := strings.Cut(f, "=")
			if !ok || k != "avg10" {
				continue
			}
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return -1
			}
			return n
		}
	}
	return -1
}

// hostSwapTotalMiB reports configured swap. -1 where unreadable, so the
// backstop below cannot divide by an invented total.
func hostSwapTotalMiB() int64 {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || k != "SwapTotal" {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			return -1
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return -1
		}
		return n / 1024
	}
	return -1
}
