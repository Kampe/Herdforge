// Package drainreceipt is the FAC-605 durable bounded drain receipt.
//
// A drain that vanishes is indistinguishable from a drain that never ran.
// The receipt is written at start and updated to an explicit terminal status
// (completed or timeout). Missing is not a valid terminal state after Begin.
//
// Partial evidence follows the same discipline as pkg/provider maxCommentPages:
// exceeding the bound is an explicit timeout refusal, never a silently complete
// readback. Freshness posture: a timeout receipt is degraded evidence that
// MustExplain how to resume.
package drainreceipt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

const (
	// RelPath is the repository-scoped durable receipt location.
	RelPath = ".herd/drain/receipt.json"
	Schema  = 1
)

// Status is the explicit terminal (or in-flight) posture of a drain.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusTimeout   Status = "timeout"
)

// Receipt is the durable bounded drain attestation.
type Receipt struct {
	SchemaVersion int    `json:"schema_version"`
	Status        Status `json:"status"`
	// Bound is the wall-clock bound that applied to this run (e.g. "2m0s").
	Bound string `json:"bound"`
	Phase string `json:"phase,omitempty"`
	// ResumeCursor is the last tip SHA fully processed before stop. Empty means
	// start from the beginning. Next drain continues AFTER this SHA.
	ResumeCursor string `json:"resume_cursor,omitempty"`
	StartedAt    string `json:"started_at"`
	UpdatedAt    string `json:"updated_at"`
	FinishedAt   string `json:"finished_at,omitempty"`
	HarvestTips  int    `json:"harvest_tips,omitempty"`
	ScannedTips  int    `json:"scanned_tips,omitempty"`
	TotalTips    int    `json:"total_tips,omitempty"`
	Note         string `json:"note,omitempty"`
}

// Path joins repoRoot with RelPath.
func Path(repoRoot string) string {
	return filepath.Join(strings.TrimSpace(repoRoot), RelPath)
}

// Begin writes a running receipt. Call before any scan work.
func Begin(repoRoot, bound, phase string) (*Receipt, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r := &Receipt{
		SchemaVersion: Schema,
		Status:        StatusRunning,
		Bound:         strings.TrimSpace(bound),
		Phase:         strings.TrimSpace(phase),
		StartedAt:     now,
		UpdatedAt:     now,
		Note:          "drain in progress; absence of a later terminal status means the process vanished",
	}
	if err := write(repoRoot, r); err != nil {
		return nil, err
	}
	return r, nil
}

// Load reads the durable receipt. os.ErrNotExist means no drain has attested.
func Load(repoRoot string) (*Receipt, error) {
	body, err := os.ReadFile(Path(repoRoot))
	if err != nil {
		return nil, err
	}
	var r Receipt
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("drainreceipt: corrupt receipt: %w", err)
	}
	return &r, nil
}

// DefaultAbandonedAge is how old a still-running receipt must be before it is
// treated as an abandoned process (FAC-605). Presence is not liveness: a
// receipt frozen at status=running after SIGKILL/OOM must not be read as a
// live drain, and must not collapse into "never ran".
const DefaultAbandonedAge = 3 * time.Minute

// PriorResumeCursor returns a usable resume cursor from a prior drain.
// Accepts an explicit timeout receipt, or a running receipt whose UpdatedAt is
// older than the bound (or DefaultAbandonedAge) — that is an abandoned run,
// not a live one. ok is false when there is no receipt, the prior run
// completed, the cursor is empty, or a running receipt is still fresh.
// Call BEFORE Begin: Begin overwrites the prior receipt.
func PriorResumeCursor(repoRoot string) (cursor string, ok bool) {
	r, err := Load(repoRoot)
	if err != nil || r == nil {
		return "", false
	}
	c := strings.TrimSpace(r.ResumeCursor)
	if c == "" {
		return "", false
	}
	switch r.Status {
	case StatusTimeout:
		return c, true
	case StatusRunning:
		if Abandoned(r, time.Now().UTC()) {
			return c, true
		}
		return "", false
	default:
		return "", false
	}
}

// Abandoned reports whether a running receipt is too old to be a live drain.
// An unfinished record and a missing record are different facts.
func Abandoned(r *Receipt, now time.Time) bool {
	if r == nil || r.Status != StatusRunning {
		return false
	}
	updated, err := time.Parse(time.RFC3339Nano, r.UpdatedAt)
	if err != nil {
		updated, err = time.Parse(time.RFC3339, r.UpdatedAt)
		if err != nil {
			return true // unparseable mtime: fail closed as abandoned
		}
	}
	limit := DefaultAbandonedAge
	if b := strings.TrimSpace(r.Bound); b != "" {
		if d, err := time.ParseDuration(b); err == nil && d > 0 {
			// Bound plus a small grace: a live drain should refresh UpdatedAt
			// inside the hot loop well before the wall bound expires.
			limit = d + time.Minute
		}
	}
	return now.Sub(updated) > limit
}

// MarkTimeout records an explicit timeout with resume cursor. Never leave
// StatusRunning as the last word after a bound expires.
func MarkTimeout(repoRoot string, phase, resumeCursor string, scanned, total int) error {
	r, err := Load(repoRoot)
	if err != nil {
		// Process may have died before Begin flushed; still attest timeout.
		r = &Receipt{SchemaVersion: Schema, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r.Status = StatusTimeout
	r.Phase = strings.TrimSpace(phase)
	r.ResumeCursor = strings.TrimSpace(resumeCursor)
	r.ScannedTips = scanned
	r.TotalTips = total
	r.UpdatedAt = now
	r.FinishedAt = now
	prev := freshness.Fresh("herd-drain", time.Now().UTC(), "running")
	r.Note = freshness.Degrade(prev, "herd-drain", fmt.Errorf("bounded scan exceeded"),
		"re-run herd drain; it continues after resume_cursor").MustExplain(time.Now().UTC())
	return write(repoRoot, r)
}

// MarkCompleted records a finished drain. Distinguishable from never-ran
// (no receipt) and from timeout.
func MarkCompleted(repoRoot string, scanned, total int) error {
	r, err := Load(repoRoot)
	if err != nil {
		r = &Receipt{SchemaVersion: Schema, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r.Status = StatusCompleted
	r.Phase = "done"
	r.ResumeCursor = ""
	r.ScannedTips = scanned
	r.TotalTips = total
	r.UpdatedAt = now
	r.FinishedAt = now
	r.Note = "drain completed inside bound"
	return write(repoRoot, r)
}

// Progress updates an in-flight receipt without terminating it.
func Progress(repoRoot, phase, resumeCursor string, scanned, total, harvestTips int) error {
	r, err := Load(repoRoot)
	if err != nil {
		return err
	}
	if r.Status != StatusRunning {
		return nil
	}
	r.Phase = strings.TrimSpace(phase)
	if c := strings.TrimSpace(resumeCursor); c != "" {
		r.ResumeCursor = c
	}
	r.ScannedTips = scanned
	r.TotalTips = total
	if harvestTips > 0 {
		r.HarvestTips = harvestTips
	}
	r.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return write(repoRoot, r)
}

func write(repoRoot string, r *Receipt) error {
	path := Path(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("drainreceipt: mkdir: %w", err)
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("drainreceipt: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("drainreceipt: rename: %w", err)
	}
	return nil
}
