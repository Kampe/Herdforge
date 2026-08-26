package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	holdFor := fs.Duration("claim-ttl", 180*time.Second, "how long a claimed admission stays held before it expires")
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
	SwapUsedMiB  int64  `json:"swap_used_mib"`     // -1 unknown
	// Processes/Threads/FDs, not RSS, are the plausible binding constraints
	// here -- see the note on hostProcessLoad. -1 means unmeasured.
	Processes      int      `json:"processes"`
	Threads        int      `json:"threads"`
	FDLimit        int      `json:"fd_limit"`
	AgentsListed   bool     `json:"agents_listed"`
	Agents         int      `json:"agents_total"`
	Reviewers      int      `json:"reviewers_live"`
	ReviewersIdle  int      `json:"reviewers_idle"`
	IdleReviewerID []string `json:"reviewers_idle_names,omitempty"`
}

// Capacity is the observation plus one decision with a named reason.
type Capacity struct {
	CapacityObservation
	ReviewLimit  int    `json:"review_limit"`
	LimitDerived bool   `json:"review_limit_derived"`
	NeedMiB      int64  `json:"need_mib"`
	Admit        bool   `json:"admit"`
	Reason       string `json:"reason"`
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
	procs := "unknown"
	if c.Processes >= 0 {
		procs = fmt.Sprintf("%d/%dthr", c.Processes, c.Threads)
	}
	return fmt.Sprintf("%s herdr=%v reviewers=%d/%d idle=%d mem_available=%s procs=%s need=%dMiB: %s",
		verdict, c.HerdrRunning, c.Reviewers, c.ReviewLimit, c.ReviewersIdle, mem, procs, c.NeedMiB, c.Reason)
}

// decideCapacity is the whole policy, separated from measurement so it is
// testable without a live host.
//
// Gate order matters: report the cause that actually stopped the launch, not
// the first one that happens to be checkable. herdr being down is the cause of
// the incident this exists for, so it is named first.
// swapDegradedMiB is the point at which swap use stops being incidental. Zero
// would refuse on a single reclaimed page; this is "the host has started paying
// for memory it does not have".
const swapDegradedMiB = 512

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
	case o.SwapUsedMiB > swapDegradedMiB:
		// Swapping means the host is already losing, and it degrades long before
		// MemAvailable looks alarming. W4 held 5GB of swap with active writeback
		// while free memory sat at the kernel's minimum watermark.
		c.Reason = fmt.Sprintf("host is already swapping (%dMiB in use): it is degrading now, and another reviewer accelerates it", o.SwapUsedMiB)
		if o.ReviewersIdle > 0 {
			c.Reason += fmt.Sprintf("; reap %d idle reviewer(s) first", o.ReviewersIdle)
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
	o := CapacityObservation{MemTotalMiB: -1, MemAvailMiB: -1, SwapUsedMiB: -1, Processes: -1, Threads: -1, FDLimit: -1}
	o.HerdrRunning, o.HerdrDetail = herdrServerRunning()
	o.MemTotalMiB, o.MemAvailMiB, o.SwapUsedMiB = hostMemoryMiB()
	o.Processes, o.Threads, o.FDLimit = hostProcessLoad()

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
func hostMemoryMiB() (total, avail, swapUsed int64) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
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
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(c)
		return
	}
	fmt.Println(c.String())
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
func holdAdmissionLease(ttl time.Duration) (release func(), held bool, err error) {
	path := admissionLeasePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	try := func() (bool, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "pid=%d taken=%s ttl=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339), ttl)
			return true, f.Close()
		}
		if !os.IsExist(err) {
			return false, err
		}
		return false, nil
	}
	ok, err := try()
	if err != nil {
		return nil, false, err
	}
	if !ok {
		// Expired lease: reclaim it. Age is read from the file, not remembered,
		// so a lease survives the process that took it.
		if fi, statErr := os.Stat(path); statErr == nil && time.Since(fi.ModTime()) > ttl {
			_ = os.Remove(path)
			if ok, err = try(); err != nil {
				return nil, false, err
			}
		}
	}
	if !ok {
		return nil, false, nil
	}
	return func() { _ = os.Remove(path) }, true, nil
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
