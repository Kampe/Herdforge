package beat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Wake is a lane's next executable action, recorded at the moment the event
// that produced it happened.
//
// FAC-707: a builder reported PASS and stopped. A reviewer ingested a verdict
// and stopped. A merge landed and nothing consumed it. Every one of those is a
// completed handoff with no next edge, and the lane then sat resident and idle
// until an operator noticed by hand -- eleven lanes at once, most recently.
//
// The fix is not a scheduler. It is that an event which ENDS a lane's current
// action must, in the same step, record what that lane does next. A handoff
// nobody enqueued is a handoff nobody consumes.
type Wake struct {
	Lane string `json:"lane"`
	// Event is what ended the previous action.
	Event Transition `json:"event"`
	// Action is the exact next thing this lane should do. Never a description
	// of a category of work: a lane cannot execute "review something".
	Action string `json:"action"`
	// Cause names the thing that triggered this, so a stale wake can be told
	// apart from a current one.
	Cause      string `json:"cause,omitempty"`
	RecordedAt string `json:"recorded_at"`
}

// WakeQueueEnv overrides where wakes are recorded.
const WakeQueueEnv = "HERD_WAKE_QUEUE"

// Enqueue atomically records a lane's next action.
//
// A single O_APPEND write of one line is atomic for the sizes involved, so a
// concurrent reader sees either the whole wake or none of it -- never half.
//
// An empty Action is REFUSED. That is the entire point: an event that ends a
// lane's work and names no successor is exactly the dead handoff this exists to
// prevent, and it must be impossible to record rather than merely discouraged.
func Enqueue(repoRoot string, w Wake) error {
	if strings.TrimSpace(w.Lane) == "" {
		return fmt.Errorf("wake requires a lane")
	}
	if strings.TrimSpace(w.Action) == "" {
		return fmt.Errorf("wake for lane %q names no next action: an event that ends a lane's work "+
			"MUST record what it does next, or the lane goes idle holding a completed handoff", w.Lane)
	}
	if w.Event == "" {
		return fmt.Errorf("wake for lane %q names no triggering event", w.Lane)
	}
	if strings.TrimSpace(w.RecordedAt) == "" {
		w.RecordedAt = time.Now().UTC().Format(time.RFC3339)
	}

	path := WakeQueuePath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(w)
	if err != nil {
		return err
	}
	f, err := openAppend(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	// Durability matters more than speed here: a wake lost to a crash is a lane
	// that never restarts, which is the failure being fixed.
	return f.Sync()
}

// WakeQueuePath resolves the append-only wake log.
func WakeQueuePath(repoRoot string) string {
	if p := strings.TrimSpace(os.Getenv(WakeQueueEnv)); p != "" {
		return p
	}
	return filepath.Join(repoRoot, ".herd", "wake-queue.jsonl")
}

// PendingWakes returns the most recent wake per lane.
//
// Last-write-wins per lane, deliberately: the queue is append-only history, and
// a lane's CURRENT next action is whatever the latest event said. Replaying
// older wakes would resurrect superseded work, which is the review-ingest
// polarity defect in a different costume.
//
// A malformed line is reported and skipped rather than aborting the read: one
// bad record must not hide every other lane's next action.
func PendingWakes(repoRoot string) (map[string]Wake, []error) {
	path := WakeQueuePath(repoRoot)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Wake{}, nil
		}
		return nil, []error{err}
	}
	out := map[string]Wake{}
	var problems []error
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var w Wake
		if err := json.Unmarshal([]byte(line), &w); err != nil {
			problems = append(problems, fmt.Errorf("wake-queue line %d unparseable: %w", i+1, err))
			continue
		}
		if strings.TrimSpace(w.Lane) == "" {
			problems = append(problems, fmt.Errorf("wake-queue line %d names no lane", i+1))
			continue
		}
		out[w.Lane] = w
	}
	return out, problems
}

// openAppend is the single place this package opens the queue for writing, so
// the append mode and permissions cannot drift between callers.
func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}
