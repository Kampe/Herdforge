// Package goalguard owns the durable continuation contract for a standing
// lane. A guard decision is deliberately boring: it either consumes one
// bounded continuation or returns a durable stop reason. Missing, malformed,
// expired, or mismatched authority evidence is an error, never permission to
// continue.
package goalguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/lock"
	"github.com/Kampe/Herdforge/pkg/posture"
)

const SchemaVersion = 1

var (
	ErrMissing = errors.New("goalguard: goal is missing")
	ErrCorrupt = errors.New("goalguard: durable goal is corrupt")
	ErrStale   = errors.New("goalguard: evidence is stale or mismatched")
)

// StopConditions are operator or coordinator facts that make stopping safe.
// They are persisted with the contract so a restart cannot forget the lane's
// stop policy.
type StopConditions struct {
	Completed bool `json:"completed"`
	LeaseLost bool `json:"lease_lost"`
	Held      bool `json:"held"`
	WindDown  bool `json:"wind_down"`
}

// AuthorityEnvelope is the coordinator's verifiable grant for a standing
// lane. It is persisted beside the goal so the Stop hook enforces a grant
// that already exists; the hook never creates continuation authority.
type AuthorityEnvelope struct {
	Grantor          string   `json:"grantor"`
	PacketPath       string   `json:"packet_path"`
	BoundedAutonomy  string   `json:"bounded_autonomy"`
	MutationLimits   string   `json:"mutation_limits"`
	ForbiddenActions []string `json:"forbidden_actions"`
	StopConditions   []string `json:"stop_conditions"`
}

func (a AuthorityEnvelope) Validate() error {
	if strings.TrimSpace(a.Grantor) == "" || strings.TrimSpace(a.PacketPath) == "" ||
		strings.TrimSpace(a.BoundedAutonomy) == "" || strings.TrimSpace(a.MutationLimits) == "" ||
		len(nonBlank(a.ForbiddenActions)) == 0 || len(nonBlank(a.StopConditions)) == 0 {
		return errors.New("goalguard: authority envelope is incomplete")
	}
	return nil
}

func nonBlank(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

// Goal is the durable standing-lane contract. Continuations is incremented
// before a continue decision is returned, so a crash after the decision cannot
// reset the budget and create an unbounded loop.
type Goal struct {
	SchemaVersion    int                `json:"schema_version"`
	Lane             string             `json:"lane"`
	Task             string             `json:"task"`
	Owner            string             `json:"owner"`
	Generation       int64              `json:"generation"`
	MaxContinuations int                `json:"max_continuations"`
	Continuations    int                `json:"continuations"`
	Stop             StopConditions     `json:"stop"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
	ExpiresAt        *time.Time         `json:"expires_at,omitempty"`
	Authority        *AuthorityEnvelope `json:"authority,omitempty"`
}

// Evidence is the live authority snapshot supplied for one guard decision.
type Evidence struct {
	Lane       string    `json:"lane"`
	Task       string    `json:"task"`
	Owner      string    `json:"owner"`
	Generation int64     `json:"generation"`
	Completed  bool      `json:"completed"`
	LeaseHeld  bool      `json:"lease_held"`
	Held       bool      `json:"held"`
	WindDown   bool      `json:"wind_down"`
	Now        time.Time `json:"now"`
}

type Decision struct {
	Continue      bool   `json:"continue"`
	Reason        string `json:"reason"`
	Continuations int    `json:"continuations"`
}

// Store is a small atomic JSON authority. Rename gives readers either the
// previous complete contract or the next complete contract, never a partial
// write. The path is intentionally injectable for hermetic tests.
type Store struct{ path string }

// DefaultPath is cwd-relative, not the shared posture.StateDir(): a standing
// lane's Stop hook always runs with its own worktree as cwd, so this alone
// gives per-lane isolation without any lane-name plumbing through the hook
// config. A single shared home-based path would make every lane's launch
// and every lane's Stop hook read/write the same file, corrupting whichever
// lane set its goal last. Falls back to posture.StateDir() only when cwd is
// unavailable (e.g. a deleted worktree).
func DefaultPath() string {
	if cwd, err := os.Getwd(); err == nil {
		return filepath.Join(cwd, ".herd", "goal-guard.json")
	}
	return filepath.Join(posture.StateDir(), "goal-guard.json")
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("goalguard: state path is required")
	}
	return &Store{path: path}, nil
}

func (s *Store) Load() (Goal, error) {
	if s == nil || s.path == "" {
		return Goal{}, fmt.Errorf("%w: store is nil", ErrCorrupt)
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Goal{}, ErrMissing
		}
		return Goal{}, fmt.Errorf("%w: read: %v", ErrCorrupt, err)
	}
	var g Goal
	if err := json.Unmarshal(b, &g); err != nil {
		return Goal{}, fmt.Errorf("%w: decode: %v", ErrCorrupt, err)
	}
	if err := validate(g); err != nil {
		return Goal{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return g, nil
}

func (s *Store) Set(g Goal) error {
	if s == nil || s.path == "" {
		return fmt.Errorf("%w: store is nil", ErrCorrupt)
	}
	now := g.UpdatedAt
	if now.IsZero() {
		now = g.CreatedAt
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if g.SchemaVersion == 0 {
		g.SchemaVersion = SchemaVersion
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	g.UpdatedAt = now
	if err := validate(g); err != nil {
		return err
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return fmt.Errorf("goalguard: encode: %w", err)
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("goalguard: create state directory: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".goal-guard-*")
	if err != nil {
		return fmt.Errorf("goalguard: create temporary state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("goalguard: protect state: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("goalguard: write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("goalguard: sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("goalguard: close state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("goalguard: commit state: %w", err)
	}
	return nil
}

// Evaluate validates live evidence, persists a terminal stop when needed, or
// consumes exactly one continuation. Stop conditions are evaluated before the
// budget so completed/held/expired work never spends another continuation.
func (s *Store) Evaluate(e Evidence) (Decision, error) {
	// The continuation counter is a cross-process compare-and-write. Serialize
	// the read/decision/write sequence so two coordinator restarts cannot both
	// spend the same final continuation.
	fence := lock.NewDirLock(s.path + ".lock.d")
	if err := fence.Acquire(context.Background(), 0, "goalguard evaluate"); err != nil {
		return Decision{}, fmt.Errorf("goalguard: acquire evaluation lock: %w", err)
	}
	defer fence.Release()
	g, err := s.Load()
	if err != nil {
		return Decision{}, err
	}
	if err := validateEvidence(g, e); err != nil {
		return Decision{}, err
	}
	now := e.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if g.ExpiresAt != nil && !now.Before(g.ExpiresAt.UTC()) {
		return Decision{Reason: "expired", Continuations: g.Continuations}, nil
	}
	if g.Stop.Completed || e.Completed {
		return Decision{Reason: "completed", Continuations: g.Continuations}, nil
	}
	if g.Stop.LeaseLost || !e.LeaseHeld {
		return Decision{Reason: "lease_lost", Continuations: g.Continuations}, nil
	}
	if g.Stop.Held || e.Held {
		return Decision{Reason: "held", Continuations: g.Continuations}, nil
	}
	if g.Stop.WindDown || e.WindDown {
		return Decision{Reason: "wind_down", Continuations: g.Continuations}, nil
	}
	if g.MaxContinuations > 0 && g.Continuations >= g.MaxContinuations {
		return Decision{Reason: "max_continuations", Continuations: g.Continuations}, nil
	}
	g.Continuations++
	g.UpdatedAt = now
	if err := s.Set(g); err != nil {
		return Decision{}, err
	}
	return Decision{Continue: true, Reason: "goal_active", Continuations: g.Continuations}, nil
}

func validate(g Goal) error {
	// MaxContinuations == 0 means unbounded: the goal runs until a stop
	// condition (completed/held/lease/expiry) is met, never a budget.
	if g.SchemaVersion != SchemaVersion || strings.TrimSpace(g.Lane) == "" || strings.TrimSpace(g.Task) == "" || strings.TrimSpace(g.Owner) == "" || g.Generation < 1 || g.MaxContinuations < 0 || g.Continuations < 0 || (g.MaxContinuations > 0 && g.Continuations > g.MaxContinuations) || g.CreatedAt.IsZero() || g.UpdatedAt.IsZero() {
		return fmt.Errorf("goalguard: incomplete or contradictory goal")
	}
	if g.ExpiresAt != nil && g.ExpiresAt.IsZero() {
		return fmt.Errorf("goalguard: invalid expiry")
	}
	return nil
}

func validateEvidence(g Goal, e Evidence) error {
	if strings.TrimSpace(e.Lane) == "" || strings.TrimSpace(e.Task) == "" || strings.TrimSpace(e.Owner) == "" || e.Generation < 1 || e.Now.IsZero() {
		return fmt.Errorf("%w: incomplete evidence", ErrStale)
	}
	if e.Lane != g.Lane || e.Task != g.Task || e.Owner != g.Owner || e.Generation != g.Generation {
		return fmt.Errorf("%w: goal binding changed", ErrStale)
	}
	return nil
}
