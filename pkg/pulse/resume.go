package pulse

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FAC-614: naming a paused goal is only half the fix. This is the half that
// acts on it, and the constraints are what make acting safe.
//
// Resuming forever is its own failure. A lane that pauses again immediately
// after every resume is not being helped -- it is stuck, and a coordinator that
// keeps sending the resume verb turns a visible stall into an invisible hot
// loop. That is strictly worse than the original defect, because a lane
// oscillating between paused and working looks busy in aggregate.
//
// So a resume is bounded twice: by cadence (not more often than X) and by
// consecutive attempts (after N with no progress, stop and escalate).

const (
	// ResumeCadence is the minimum gap between resume attempts for one lane.
	// A goal loop that stops again inside this window is stuck, not idle.
	ResumeCadence = 90 * time.Second

	// ResumeAttemptLimit is how many consecutive resumes may be sent to a lane
	// that never reports progress. Past this the lane is escalated instead:
	// something is wrong that a resume verb cannot fix, and continuing to send
	// it hides that.
	ResumeAttemptLimit = 3

	// ResumeStateEnv overrides where resume bookkeeping is kept (tests).
	ResumeStateEnv = "HERD_RESUME_STATE"
)

// ResumeRecord is what is remembered about one lane between beats.
type ResumeRecord struct {
	Lane        string    `json:"lane"`
	LastAttempt time.Time `json:"last_attempt"`
	Consecutive int       `json:"consecutive"`
	// LastProgressSeq is the state_change_seq observed when the lane last
	// looked healthy. Progress is measured against the harness's own counter
	// rather than inferred from elapsed time, because a lane can be legitimately
	// slow without being stuck.
	LastProgressSeq int64 `json:"last_progress_seq"`
}

// ResumeState is the durable map of lane bookkeeping.
type ResumeState struct {
	Lanes map[string]ResumeRecord `json:"lanes"`
}

// ResumeDecision is what a beat concluded about one paused lane.
type ResumeDecision struct {
	Lane      string
	Resume    bool
	Escalate  bool
	Reason    string
	Attempt   int
	NextAfter time.Duration
}

// DecideResume applies both bounds to one paused lane.
//
// It is pure so the policy is testable without a live pane, a clock, or a
// filesystem -- the three things that made the original defect hard to see.
func DecideResume(rec ResumeRecord, lane string, seq int64, now time.Time) ResumeDecision {
	d := ResumeDecision{Lane: lane}

	// Progress since the last attempt clears the ATTEMPT COUNTER -- a lane that
	// moved is not stuck, however many times it paused before.
	//
	// It does NOT license an immediate resume. The cadence bound below still
	// applies. Checking progress first would let a lane that advances and
	// pauses repeatedly be resumed on every single beat, which is the hot loop
	// this whole policy exists to prevent -- caught by
	// TestAJustResumedLaneIsThrottled rather than by review.
	advanced := seq > rec.LastProgressSeq && rec.Consecutive > 0
	if advanced {
		rec.Consecutive = 0
	}

	if rec.Consecutive >= ResumeAttemptLimit {
		d.Escalate = true
		d.Attempt = rec.Consecutive
		d.Reason = fmt.Sprintf("%d consecutive resumes with no progress (seq stuck at %d): a resume verb cannot fix this, escalating instead of looping",
			rec.Consecutive, rec.LastProgressSeq)
		return d
	}

	if !rec.LastAttempt.IsZero() {
		if since := now.Sub(rec.LastAttempt); since < ResumeCadence {
			d.Attempt = rec.Consecutive
			d.NextAfter = ResumeCadence - since
			d.Reason = fmt.Sprintf("resumed %s ago; next attempt in %s",
				since.Round(time.Second), d.NextAfter.Round(time.Second))
			return d
		}
	}

	d.Resume = true
	d.Attempt = rec.Consecutive + 1
	if advanced {
		d.Reason = fmt.Sprintf("lane advanced since the last resume; attempt counter reset, resume attempt %d of %d",
			d.Attempt, ResumeAttemptLimit)
		return d
	}
	d.Reason = fmt.Sprintf("paused goal, resume attempt %d of %d", d.Attempt, ResumeAttemptLimit)
	return d
}

// ResumeStatePath resolves where bookkeeping lives.
func ResumeStatePath(repoRoot string) string {
	if p := strings.TrimSpace(os.Getenv(ResumeStateEnv)); p != "" {
		return p
	}
	return filepath.Join(repoRoot, ".herd", "run", "resume-state.json")
}

// LoadResumeState reads bookkeeping. A missing file is an empty state, not an
// error: the first beat on a host has nothing to remember. A CORRUPT file is
// also empty rather than fatal -- losing throttle history costs one extra
// resume, while refusing to beat costs the whole fleet.
func LoadResumeState(path string) ResumeState {
	st := ResumeState{Lanes: map[string]ResumeRecord{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	var parsed ResumeState
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Lanes == nil {
		return st
	}
	return parsed
}

// SaveResumeState persists bookkeeping whole, via temp-file and rename, so a
// reader never sees a partial map.
func SaveResumeState(path string, st ResumeState) error {
	if st.Lanes == nil {
		st.Lanes = map[string]ResumeRecord{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RecordResume updates bookkeeping after an attempt.
func RecordResume(st ResumeState, lane string, seq int64, now time.Time) ResumeState {
	if st.Lanes == nil {
		st.Lanes = map[string]ResumeRecord{}
	}
	rec := st.Lanes[lane]
	if seq > rec.LastProgressSeq {
		rec.Consecutive = 0
	}
	rec.Lane = lane
	rec.LastAttempt = now
	rec.Consecutive++
	rec.LastProgressSeq = seq
	st.Lanes[lane] = rec
	return st
}

// ClearResume forgets a lane that is healthy again, so a lane which pauses
// once a day is never treated as stuck.
func ClearResume(st ResumeState, lane string) ResumeState {
	if st.Lanes != nil {
		delete(st.Lanes, lane)
	}
	return st
}

// EscalatedLanes lists lanes that exhausted their attempts, sorted for stable
// output. A count would not be actionable; only an identity is.
func EscalatedLanes(st ResumeState) []string {
	var out []string
	for lane, rec := range st.Lanes {
		if rec.Consecutive >= ResumeAttemptLimit {
			out = append(out, lane)
		}
	}
	sort.Strings(out)
	return out
}
