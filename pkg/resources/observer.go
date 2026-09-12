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
	"os"
	"path/filepath"
	"time"
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
	if c.Lifetime < c.Interval {
		return c, fmt.Errorf("observer lifetime %s is shorter than one interval %s: it would publish nothing",
			c.Lifetime, c.Interval)
	}
	if c.SampleTimeout <= 0 {
		c.SampleTimeout = c.Interval / 2
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
	PID           int    `json:"pid"`
	StartedAt     string `json:"started_at"`
	LockPath      string `json:"lock_path"`
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
		seq++

		sampleCtx, cancel := context.WithTimeout(ctx, cfg.SampleTimeout)
		report, sampleErr := deps.sample(sampleCtx, started)
		cancel()

		completed := deps.now()
		skipped := uint64(0)
		if lastIndex > 0 {
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

		// Advance to the first scheduled tick strictly in the future. No
		// catch-up burst: elapsed ticks are skipped, not replayed.
		now := deps.now()
		next := tickIndex + 1
		for !base.Add(time.Duration(next) * cfg.Interval).After(now) {
			next++
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
	status.ExpiresAt = stampUTC(at.Add(time.Duration(ObserverExpiryTicks) * interval))
	return deps.publish(*status)
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
	body, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("observer: encode status: %w", err)
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
// The cap is deliberate: a status file larger than a full ring cannot have been
// written by this observer, so it is refused rather than parsed. An unbounded
// read of an attacker- or bug-grown file is exactly the memory cost this
// artifact was shaped to avoid.
func ReadObserverStatus(path string) (ObserverStatus, error) {
	var status ObserverStatus
	info, err := os.Stat(path)
	if err != nil {
		return status, fmt.Errorf("observer: stat status: %w", err)
	}
	if info.Size() > MaxObserverStatusBytes {
		return status, fmt.Errorf("observer: status file is %d bytes, beyond the %d-byte bound; refusing to read it",
			info.Size(), MaxObserverStatusBytes)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return status, fmt.Errorf("observer: read status: %w", err)
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
	return true, ""
}
