package resources

// FAC-829: resource observation on a native clock.
//
// The dedicated performance guard samples inside language-model turns, so its
// sample clock IS the turn clock. Measured against a 30s target its gaps ran
// 36-51s, and a hand-maintained report once carried an observation stamped
// 22:27:57Z beside a publish stamp of 22:20:52Z copied forward from an earlier
// iteration. Those are two distinct defects: cadence coupling, and a copied
// publish field. This observer addresses both mechanically -- ticks come from a
// timer rather than a turn, and the publish time is taken at write time and is
// refused if it would predate the sample it accompanies.
//
// What this does NOT claim:
//   - It is not a hard real-time guarantee. A sample that overruns its interval
//     causes the next tick to be DROPPED, not queued, and the drop is counted
//     and published rather than hidden.
//   - It cannot force a non-cooperative probe to stop. Cancellation reaches a
//     probe only insofar as the runner honours it (see boundedProbeOutput).
//   - It does not authorize work, clear a hold, or own the fleet. Its output is
//     an observation with an expiry; a consumer that ignores the expiry is
//     reading a dead observer's opinion.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

// ObserverSchemaVersion is bumped when the published shape changes
// incompatibly. A consumer that does not recognise the version must refuse the
// file rather than interpret unknown fields.
const ObserverSchemaVersion = 1

// Observer timing bounds. These are deliberately narrow: a flag that could set
// a zero or negative interval would turn the observer into a busy loop, which
// is the exact failure mode this package exists to prevent.
const (
	// DefaultObserverInterval matches the operational guard's own target, so a
	// native observer is directly comparable with it.
	DefaultObserverInterval = 30 * time.Second
	// MinObserverInterval keeps the four Darwin subprocess probes per sample
	// from becoming a load source themselves.
	MinObserverInterval = 5 * time.Second
	MaxObserverInterval = 15 * time.Minute

	DefaultObserverLifetime = 12 * time.Hour
	MinObserverLifetime     = time.Minute
	MaxObserverLifetime     = 7 * 24 * time.Hour

	// ObserverHistoryMax is the ring size. History lives INSIDE the one atomic
	// status object, so the artifact is bounded by construction rather than by
	// a truncation pass that could be interrupted.
	ObserverHistoryMax = 24

	// MinSampleTimeout is a floor, not a guarantee: see boundedProbeOutput.
	MinSampleTimeout = time.Second

	// ObserverExpiryTicks is how many intervals a published status stays
	// meaningful. Beyond it the observer is presumed dead and the report
	// authorizes nothing.
	ObserverExpiryTicks = 3
)

// ErrObserverBusy is returned when another observer already holds the canonical
// lock. It is a refusal, never a wait: two observers publishing into one status
// path would interleave writes and neither could be trusted.
var ErrObserverBusy = errors.New("resources: another observer holds the canonical observer lock")

// ObserverConfig is the validated shape of one observer run.
type ObserverConfig struct {
	Interval      time.Duration
	SampleTimeout time.Duration
	Lifetime      time.Duration
	// StatusPath is where the bounded snapshot is published. It is derived, not
	// caller-chosen, so the observer cannot be pointed at the operational
	// guard's report or at source. See ObserverStatusPath.
	StatusPath string
	// LockPath is the canonical observer lock. See ObserverLockPath for the
	// scope this actually enforces.
	LockPath string
	// LockWait bounds how long acquisition may retry before refusing. It is
	// short by design: a competing observer must fail promptly.
	LockWait time.Duration
}

// DefaultObserverConfig is the shipped configuration. Paths are resolved from
// the canonical per-user state root, never compiled in absolute.
func DefaultObserverConfig() ObserverConfig {
	return ObserverConfig{
		Interval:      DefaultObserverInterval,
		SampleTimeout: DefaultObserverInterval / 2,
		Lifetime:      DefaultObserverLifetime,
		StatusPath:    ObserverStatusPath(),
		LockPath:      ObserverLockPath(),
		LockWait:      2 * time.Second,
	}
}

// Validate repairs nothing and accepts nothing doubtful.
//
// Unlike Limits.sane(), which repairs a bad threshold because refusing to
// decide at all would be worse, an out-of-range observer flag is an explicit
// operator mistake with no safe interpretation: silently "repairing" a 0s
// interval into 30s would hide that the operator asked for a busy loop.
func (c ObserverConfig) Validate() (ObserverConfig, error) {
	if c.Interval < MinObserverInterval || c.Interval > MaxObserverInterval {
		return c, fmt.Errorf("observer interval %s is outside [%s, %s]",
			c.Interval, MinObserverInterval, MaxObserverInterval)
	}
	if c.Lifetime < MinObserverLifetime || c.Lifetime > MaxObserverLifetime {
		return c, fmt.Errorf("observer lifetime %s is outside [%s, %s]",
			c.Lifetime, MinObserverLifetime, MaxObserverLifetime)
	}
	// The first tick falls one interval after the start, so a lifetime of
	// exactly one interval exits having sampled nothing. Requiring room for at
	// least one completed sample makes that impossible to configure, rather
	// than leaving a run that validates and then does nothing.
	if c.Lifetime < 2*c.Interval {
		return c, fmt.Errorf("observer lifetime %s leaves no room for a sample at a %s interval; at least %s is required",
			c.Lifetime, c.Interval, 2*c.Interval)
	}
	// A non-positive timeout is an invalid VALUE, not an absent one. The
	// caller distinguishes "flag not given" (fill the default before calling)
	// from "flag given as 0 or negative", which is refused here.
	if c.SampleTimeout <= 0 {
		return c, fmt.Errorf("per-sample timeout %s is not positive; an explicit zero or negative value is refused, not repaired",
			c.SampleTimeout)
	}
	if c.SampleTimeout < MinSampleTimeout {
		return c, fmt.Errorf("per-sample timeout %s is below the %s floor", c.SampleTimeout, MinSampleTimeout)
	}
	// A per-sample timeout at or beyond the interval guarantees a dropped tick
	// on every slow sample. Half the interval leaves room to publish.
	if c.SampleTimeout > c.Interval/2 {
		return c, fmt.Errorf("per-sample timeout %s exceeds half the %s interval; every slow sample would drop the next tick",
			c.SampleTimeout, c.Interval)
	}
	if c.StatusPath == "" {
		return c, errors.New("observer status path is empty")
	}
	if c.LockPath == "" {
		return c, errors.New("observer lock path is empty")
	}
	if c.StatusPath == c.LockPath {
		return c, errors.New("observer status path and lock path are the same file")
	}
	if c.LockWait <= 0 || c.LockWait > time.Minute {
		return c, fmt.Errorf("observer lock wait %s is outside (0, 1m]; a competing observer must fail promptly", c.LockWait)
	}
	return c, nil
}

// ObserverSample is one completed tick.
//
// Every time on it is REAL and belongs to that tick. History entries are copied
// verbatim into later publications and are never restamped, because rewriting a
// historical observation to a newer tick is precisely the defect that made the
// hand-maintained guard report untrustworthy.
type ObserverSample struct {
	Sequence uint64 `json:"sequence"`
	// ScheduledAt is when this tick was DUE; StartedAt when it actually began.
	// The difference is scheduling lateness, which is information, not noise.
	ScheduledAt string `json:"scheduled_at"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
	LateByMS    int64  `json:"late_by_ms"`
	DurationMS  int64  `json:"duration_ms"`
	// SkippedBefore is how many scheduled ticks were dropped between the
	// previous sample and this one. Dropped, never queued: there is no
	// catch-up burst.
	SkippedBefore uint64 `json:"skipped_before"`
	// Report carries each metric's OWN observation time and state.
	Report AdmissionReport `json:"admission"`
	// SampleError is set when the sample itself could not be taken. It never
	// becomes a healthy reading.
	SampleError string `json:"sample_error,omitempty"`
}

// ObserverIdentity says who published, so a stale file names its author.
type ObserverIdentity struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
	// LockID is a LOGICAL identifier, never the runtime path. A published
	// absolute path leaks the host's directory layout into an artifact that
	// may be copied elsewhere, and tells a reader nothing the scope does not
	// already say.
	LockID        string `json:"lock_id"`
	LockScope     string `json:"lock_scope"`
	IntervalMS    int64  `json:"interval_ms"`
	LifetimeMS    int64  `json:"lifetime_ms"`
	SampleTimeout int64  `json:"sample_timeout_ms"`
}

// ObserverStatus is the single atomic artifact.
type ObserverStatus struct {
	SchemaVersion int              `json:"schema_version"`
	Observer      ObserverIdentity `json:"observer"`
	// PublishedAt is stamped at WRITE time, from the clock, on every write. It
	// is never carried over from a previous status.
	PublishedAt string `json:"published_at"`
	// ExpiresAt is when this status stops meaning anything. A consumer past it
	// is reading a dead observer and must treat the contents as UNKNOWN.
	ExpiresAt    string           `json:"expires_at"`
	Latest       ObserverSample   `json:"latest"`
	History      []ObserverSample `json:"history"`
	TotalTicks   uint64           `json:"total_ticks"`
	SkippedTicks uint64           `json:"skipped_ticks"`
	// Terminated is set on the final write when the loop exits for a known
	// reason, so an operator can distinguish a clean stop from a crash.
	Terminated string `json:"terminated,omitempty"`
	// Authority is a standing disclaimer carried IN the artifact, because the
	// file outlives the conversation that produced it.
	Authority string `json:"authority"`
}

// ObserverAuthorityNote travels with every publication.
const ObserverAuthorityNote = "observation only: this report authorizes no execution, clears no hold, " +
	"owns no process, and does not replace the operational performance guard"

// observerDeps is the seam that makes the loop testable without a clock, a
// host, or a goroutine. Production wires real implementations in RunObserver.
type observerDeps struct {
	now func() time.Time
	// wait blocks until d has elapsed or ctx is done. A fake implementation
	// advances a fake clock instantly, so timing tests need no real time.
	wait func(ctx context.Context, d time.Duration) error
	// sample takes one observation as of at. It must respect its context.
	sample func(ctx context.Context, at time.Time) (AdmissionReport, error)
	// publish writes the status atomically.
	publish func(ObserverStatus) error
}

// runObserverLoop is the whole cadence policy, isolated from I/O.
//
// Serial by construction: exactly one sample is in flight at any moment, and a
// sample that overruns never gets a second runner. There is no goroutine here
// at all.
func runObserverLoop(ctx context.Context, cfg ObserverConfig, id ObserverIdentity, deps observerDeps) (ObserverStatus, error) {
	base := deps.now()
	deadline := base.Add(cfg.Lifetime)

	status := ObserverStatus{
		SchemaVersion: ObserverSchemaVersion,
		Observer:      id,
		History:       make([]ObserverSample, 0, ObserverHistoryMax),
		Authority:     ObserverAuthorityNote,
	}

	var (
		seq       uint64
		tickIndex int64 = 1
		lastIndex int64
	)

	for {
		scheduled := base.Add(time.Duration(tickIndex) * cfg.Interval)
		if !scheduled.Before(deadline) {
			status.Terminated = "lifetime reached"
			return status, publishFinal(deps, &status)
		}

		if err := deps.wait(ctx, scheduled.Sub(deps.now())); err != nil {
			status.Terminated = "cancelled: " + err.Error()
			// A cancellation still publishes, so the last thing on disk says
			// the observer stopped rather than silently going stale.
			if perr := publishFinal(deps, &status); perr != nil {
				return status, errors.Join(err, perr)
			}
			return status, err
		}

		started := deps.now()
		// The wake could have been delayed past the lifetime. Sampling then
		// would spend a probe the operator's deadline had already ended, so
		// the clock is re-read here rather than trusted from before the wait.
		if !started.Before(deadline) {
			status.Terminated = "lifetime reached"
			return status, publishFinal(deps, &status)
		}
		// Backward or impossible chronology is a fault, not a timing detail. A
		// clock that moved backwards makes every age and every staleness
		// comparison meaningless, so the run stops and says so instead of
		// clamping the numbers into something that looks healthy.
		if started.Before(scheduled.Add(-cfg.Interval)) {
			status.Terminated = "impossible chronology: sample started before its own schedule"
			return status, publishFinal(deps, &status)
		}

		seq++

		sampleCtx, cancel := context.WithTimeout(ctx, cfg.SampleTimeout)
		report, sampleErr := deps.sample(sampleCtx, started)
		cancel()

		completed := deps.now()
		if completed.Before(started) {
			status.Terminated = "impossible chronology: sample completed before it started"
			return status, publishFinal(deps, &status)
		}
		skipped := uint64(0)
		if lastIndex > 0 && tickIndex > lastIndex+1 {
			// Ticks between the previous sample and this one were DROPPED.
			skipped = uint64(tickIndex - lastIndex - 1)
		}
		lastIndex = tickIndex

		sample := ObserverSample{
			Sequence:      seq,
			ScheduledAt:   stampUTC(scheduled),
			StartedAt:     stampUTC(started),
			CompletedAt:   stampUTC(completed),
			LateByMS:      millis(started.Sub(scheduled)),
			DurationMS:    millis(completed.Sub(started)),
			SkippedBefore: skipped,
			Report:        report,
		}
		if sampleErr != nil {
			sample.SampleError = sampleErr.Error()
			// A sample that failed cannot keep an admitting decision. Whatever
			// the probe path produced before the failure is not a measurement
			// of the host now, and publishing it as ADMIT would be the
			// fail-open shape this package exists to remove.
			sample.Report.Admits = false
			sample.Report.Decision = string(DecisionRefuse)
			sample.Report.Reasons = append(sample.Report.Reasons,
				"sample failed: "+sampleErr.Error())
		}

		status.TotalTicks++
		status.SkippedTicks += skipped
		status.Latest = sample
		status.History = appendBounded(status.History, sample)

		if err := publishAt(deps, &status, deps.now(), cfg.Interval); err != nil {
			// A failed publish is reported and the loop stops: continuing to
			// sample into a file nobody can read is not observation.
			status.Terminated = "publish failed"
			return status, err
		}

		// Advance to the first scheduled tick strictly in the future, by
		// ARITHMETIC rather than by stepping once per missed tick: a long
		// overrun must cost one division, not one loop iteration per elapsed
		// interval. No catch-up burst either way -- elapsed ticks are skipped,
		// never replayed.
		now := deps.now()
		elapsed := now.Sub(base)
		if elapsed < 0 {
			status.Terminated = "impossible chronology: clock moved behind the observer's start"
			return status, publishFinal(deps, &status)
		}
		next := int64(elapsed/cfg.Interval) + 1
		if next <= tickIndex {
			next = tickIndex + 1
		}
		tickIndex = next
	}
}

// publishAt stamps the publish time from the clock and refuses to publish a
// status whose publish time would predate the observation it carries.
//
// That refusal is the direct mechanical guard against the copied-stale-field
// defect: a publish stamp can only ever be taken here, at write time.
func publishAt(deps observerDeps, status *ObserverStatus, at time.Time, interval time.Duration) error {
	if status.Latest.CompletedAt != "" {
		completed, err := time.Parse(time.RFC3339Nano, status.Latest.CompletedAt)
		if err != nil {
			return fmt.Errorf("observer: latest sample carries an unparseable completion time %q: %w",
				status.Latest.CompletedAt, err)
		}
		if at.Before(completed) {
			return fmt.Errorf("observer: refusing to publish at %s, before the observation it carries (%s)",
				stampUTC(at), status.Latest.CompletedAt)
		}
	}
	status.PublishedAt = stampUTC(at)
	status.ExpiresAt = stampUTC(observerExpiry(at, interval, status.Latest))
	return deps.publish(*status)
}

// observerExpiry is the earlier of two deadlines, never the later.
//
// A cadence-only expiry (publish + N intervals) would let a slow publication
// renew observations that had already aged out: with a 30s staleness limit and
// a 90s artifact expiry, a stale ADMIT stays readable for a minute after the
// decision behind it stopped being true. The artifact therefore expires no
// later than the underlying metric window.
//
// When no metric is usable there is no window to bound, so the status expires
// at the instant it is written: an observation of nothing authorizes nothing.
func observerExpiry(at time.Time, interval time.Duration, latest ObserverSample) time.Time {
	cadence := at.Add(time.Duration(ObserverExpiryTicks) * interval)
	window, ok := metricWindowEnd(latest.Report)
	if !ok {
		return at
	}
	if window.Before(cadence) {
		return window
	}
	return cadence
}

// metricWindowEnd is when the OLDEST metric behind a decision stops being
// current, using the same staleness limit the decision itself applied.
func metricWindowEnd(report AdmissionReport) (time.Time, bool) {
	stale := report.Limits.StaleAfter
	if stale <= 0 {
		stale = defaultStaleAfter
	}
	var oldest time.Time
	found := false
	for _, reading := range []ReadingReport{report.CPU.ReadingReport, report.Memory.ReadingReport} {
		if !reading.Known || reading.ObservedAt == "" {
			continue
		}
		observed, err := time.Parse(time.RFC3339Nano, reading.ObservedAt)
		if err != nil {
			// An unparseable observation time is not a usable window.
			return time.Time{}, false
		}
		if !found || observed.Before(oldest) {
			oldest = observed
			found = true
		}
	}
	if !found {
		return time.Time{}, false
	}
	return oldest.Add(stale), true
}

// publishFinal writes the terminal status. It keeps the existing expiry rather
// than extending it: an exited observer must not look fresher for stopping.
func publishFinal(deps observerDeps, status *ObserverStatus) error {
	status.PublishedAt = stampUTC(deps.now())
	return deps.publish(*status)
}

// appendBounded keeps at most ObserverHistoryMax entries, newest last, by
// copying rather than resizing in place. Nothing already in the ring is
// modified, so historical observation times survive verbatim.
func appendBounded(history []ObserverSample, sample ObserverSample) []ObserverSample {
	if len(history) < ObserverHistoryMax {
		return append(history, sample)
	}
	out := make([]ObserverSample, 0, ObserverHistoryMax)
	out = append(out, history[len(history)-ObserverHistoryMax+1:]...)
	return append(out, sample)
}

// String bounds. Free-form text reaches the artifact from probe errors and
// decision reasons, neither of which this package controls the length of.
const (
	maxObserverStringBytes = 512
	maxObserverReasons     = 8
)

// capString truncates to a byte bound and SAYS it truncated, so a reader is
// never shown a shortened message that looks complete.
func capString(s string) string {
	if len(s) <= maxObserverStringBytes {
		return s
	}
	return s[:maxObserverStringBytes] + "...[truncated]"
}

func capReasons(reasons []string) []string {
	if len(reasons) == 0 {
		return reasons
	}
	limit := len(reasons)
	truncated := false
	if limit > maxObserverReasons {
		limit = maxObserverReasons
		truncated = true
	}
	out := make([]string, 0, limit+1)
	for _, r := range reasons[:limit] {
		out = append(out, capString(r))
	}
	if truncated {
		out = append(out, "...[further reasons truncated]")
	}
	return out
}

func capObserverSample(s ObserverSample) ObserverSample {
	s.SampleError = capString(s.SampleError)
	s.Report.Explanation = capString(s.Report.Explanation)
	s.Report.Reasons = capReasons(s.Report.Reasons)
	s.Report.CPU.Error = capString(s.Report.CPU.Error)
	s.Report.CPU.Recovery = capString(s.Report.CPU.Recovery)
	s.Report.Memory.Error = capString(s.Report.Memory.Error)
	s.Report.Memory.Recovery = capString(s.Report.Memory.Recovery)
	return s
}

func capObserverStrings(status ObserverStatus) ObserverStatus {
	status.Terminated = capString(status.Terminated)
	status.Latest = capObserverSample(status.Latest)
	if len(status.History) > ObserverHistoryMax {
		status.History = status.History[len(status.History)-ObserverHistoryMax:]
	}
	capped := make([]ObserverSample, 0, len(status.History))
	for _, s := range status.History {
		capped = append(capped, capObserverSample(s))
	}
	status.History = capped
	return status
}

func stampUTC(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func millis(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return int64(d / time.Millisecond)
}

// writeObserverStatus publishes the snapshot atomically: a reader either sees
// the previous complete status or the new one, never a partial object.
//
// Same shape as the durable writes elsewhere in the tree (temp in the target
// directory, restrictive mode, write, sync, close, rename).
func writeObserverStatus(path string, status ObserverStatus) error {
	// Cap every free-form string BEFORE marshalling. A 24-entry ring bounds the
	// COUNT of samples, not the length of the strings inside them: one probe
	// error echoing a long command line would make the artifact unbounded
	// without adding a single entry.
	status = capObserverStrings(status)
	body, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("observer: encode status: %w", err)
	}
	if len(body) > MaxObserverStatusBytes {
		return fmt.Errorf("observer: encoded status is %d bytes, beyond the %d-byte bound; refusing to write it",
			len(body), MaxObserverStatusBytes)
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("observer: create status directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".resource-observer-*")
	if err != nil {
		return fmt.Errorf("observer: create temporary status: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("observer: protect status: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("observer: write status: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("observer: sync status: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("observer: close status: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("observer: commit status: %w", err)
	}
	return nil
}

// ReadObserverStatus loads a published status with a bounded read.
//
// A size check taken on the PATH before reading is not a bound: the file can be
// replaced or can keep growing between the check and the read, and a FIFO has
// no size at all while blocking the open forever. So the sequence here is the
// other way round -- open without blocking, verify the mode on the DESCRIPTOR
// actually held, and read at most one byte past the limit so an oversized file
// is detected by the read itself rather than by a stale prediction about it.
func ReadObserverStatus(path string) (ObserverStatus, error) {
	var status ObserverStatus

	file, err := openRegularNonBlocking(path)
	if err != nil {
		return status, fmt.Errorf("observer: open status: %w", err)
	}
	defer file.Close()

	// Mode is checked on the descriptor, not the path: whatever this fd refers
	// to is what will be read, whatever the path points at now.
	info, err := file.Stat()
	if err != nil {
		return status, fmt.Errorf("observer: stat status descriptor: %w", err)
	}
	if !info.Mode().IsRegular() {
		return status, fmt.Errorf("observer: status path is not a regular file (mode %s); refusing to read it", info.Mode())
	}

	// Max+1 so "exactly at the limit" and "beyond the limit" are
	// distinguishable, and neither can allocate more than the bound.
	body, err := io.ReadAll(io.LimitReader(file, MaxObserverStatusBytes+1))
	if err != nil {
		return status, fmt.Errorf("observer: read status: %w", err)
	}
	if len(body) > MaxObserverStatusBytes {
		return status, fmt.Errorf("observer: status exceeds the %d-byte bound; refusing to parse it",
			MaxObserverStatusBytes)
	}

	if err := json.Unmarshal(body, &status); err != nil {
		return status, fmt.Errorf("observer: decode status: %w", err)
	}
	if status.SchemaVersion != ObserverSchemaVersion {
		return status, fmt.Errorf("observer: status schema version %d is not the supported %d; refusing to interpret it",
			status.SchemaVersion, ObserverSchemaVersion)
	}
	return status, nil
}

// MaxObserverStatusBytes bounds a status read. One sample with a full report is
// well under 4KiB; 24 of them plus identity cannot approach this.
const MaxObserverStatusBytes = 1 << 20

// ObserverUsable reports whether a published status may inform a decision AT
// at, and why not when it may not.
//
// Fail-closed by construction: an expired status, an unparseable stamp, a
// sample that errored, or a decision that was not an admit all return false.
// Nothing here grants execution -- a true result means only that the
// observation is current enough to be worth reading.
func ObserverUsable(status ObserverStatus, at time.Time) (bool, string) {
	if status.SchemaVersion != ObserverSchemaVersion {
		return false, "unrecognised schema version"
	}
	if status.Terminated != "" {
		return false, "observer terminated: " + status.Terminated
	}
	if status.ExpiresAt == "" {
		return false, "status carries no expiry"
	}
	expires, err := time.Parse(time.RFC3339Nano, status.ExpiresAt)
	if err != nil {
		return false, "status expiry is unparseable"
	}
	if !at.Before(expires) {
		return false, "status expired at " + status.ExpiresAt
	}
	if status.Latest.SampleError != "" {
		return false, "latest sample failed: " + status.Latest.SampleError
	}
	if !status.Latest.Report.Admits {
		return false, "latest decision was " + status.Latest.Report.Decision
	}
	// A consumer must not take Admits on trust. The published booleans and the
	// published observations are two separate claims, and a report whose
	// decision contradicts its own metrics is evidence of a bug, not of
	// headroom -- so it refuses here rather than being believed.
	if ok, why := reportSelfConsistent(status.Latest.Report, at); !ok {
		return false, why
	}
	return true, ""
}

// reportSelfConsistent revalidates a published decision against the metrics
// published beside it, at consumer time.
//
// It applies the SAME staleness and skew limits the decision carried, so this
// is not a second policy: it is the same policy, checked again by the reader,
// because the reader's clock is not the writer's clock.
func reportSelfConsistent(report AdmissionReport, at time.Time) (bool, string) {
	if report.Admits && report.Decision != string(DecisionAdmit) {
		return false, "report admits but its decision is " + report.Decision
	}
	limits := report.Limits
	stale := limits.StaleAfter
	if stale <= 0 {
		stale = defaultStaleAfter
	}
	skew := limits.ClockSkew
	if skew <= 0 {
		skew = defaultClockSkew
	}

	checked := 0
	for _, named := range []struct {
		what    string
		reading ReadingReport
	}{
		{"cpu", report.CPU.ReadingReport},
		{"memory", report.Memory.ReadingReport},
	} {
		r := named.reading
		if !r.Known {
			return false, named.what + " observation is not known; an admit cannot rest on it"
		}
		if r.State != string(freshness.StateFresh) {
			return false, named.what + " observation state is " + r.State
		}
		if r.ObservedAt == "" {
			return false, named.what + " observation carries no time"
		}
		observed, err := time.Parse(time.RFC3339Nano, r.ObservedAt)
		if err != nil {
			return false, named.what + " observation time is unparseable"
		}
		if observed.After(at.Add(skew)) {
			return false, named.what + " observation is in the future; chronology is impossible"
		}
		if at.Sub(observed) > stale {
			return false, named.what + " observation is older than the " + stale.String() + " staleness limit"
		}
		checked++
	}
	if checked == 0 {
		return false, "no observation was checked"
	}
	// An admitting CPU report must actually carry the number it decided on.
	if report.CPU.Normalized == nil {
		return false, "cpu report carries no normalized load behind its admit"
	}
	if !finite(*report.CPU.Normalized) || *report.CPU.Normalized < 0 {
		return false, "cpu normalized load is not a usable number"
	}
	if !report.Memory.PressureKnown {
		return false, "memory pressure is not known behind its admit"
	}
	return true, ""
}
