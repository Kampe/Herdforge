// Package runstate persists a revision-bound forge run for crash-safe resume.
// It is deliberately an authority consumer: callers must provide live provider
// and dependency evidence again before a saved run can be resumed.
package runstate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
	_ "modernc.org/sqlite"
)

const SchemaVersion = 1

var (
	ErrNotFound   = errors.New("runstate: run not found")
	ErrStale      = errors.New("runstate: saved state is stale")
	ErrAmbiguous  = errors.New("runstate: state is ambiguous")
	ErrConcurrent = errors.New("runstate: checkpoint revision changed")
	ErrTerminal   = errors.New("runstate: task is terminal")
)

// Policy is the resolved lane/model decision, never an unresolved selector.
type Policy struct {
	Lane           string `json:"lane"`
	Model          string `json:"model"`
	Provider       string `json:"provider,omitempty"`
	PolicyRevision string `json:"policy_revision,omitempty"`
}

// TaskState is a provider identity plus the exact revision observed at a run
// boundary. Terminal is monotonic: a checkpoint may never turn it back off.
type TaskState struct {
	Ref              string `json:"ref"`
	ID               string `json:"id"`
	ProviderRevision string `json:"provider_revision"`
	Status           string `json:"status"`
	Terminal         bool   `json:"terminal"`
}

// BuildRun is the immutable scheduling snapshot plus mutable per-task states.
type BuildRun struct {
	SchemaVersion           int         `json:"schema_version"`
	ID                      string      `json:"id"`
	Goal                    string      `json:"goal"`
	Ref                     string      `json:"ref"`
	DependencyGraphRevision string      `json:"dependency_graph_revision"`
	Policy                  Policy      `json:"policy"`
	Wave                    int         `json:"wave"`
	Level                   int         `json:"level"`
	Tasks                   []TaskState `json:"tasks"`
}

// RunState is one durable optimistic revision of a BuildRun.
type RunState struct {
	BuildRun
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GraphAuthority is the existing dependency authority exposed as a narrow
// read seam. Empty or unreadable evidence blocks resume.
type GraphAuthority func(context.Context) (string, error)

// Authority provides the live authorities required to resume. TaskProvider is
// intentionally the project provider interface; revisions use EncodeRevision,
// the same encoding used by the provider CAS authority.
type Authority struct {
	Tasks provider.TaskProvider
	Graph GraphAuthority
}

// Store is a SQLite-backed, cross-process durable run-state store.
type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open runstate: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000",
		`CREATE TABLE IF NOT EXISTS build_runs (id TEXT PRIMARY KEY, revision INTEGER NOT NULL, body BLOB NOT NULL, updated_at TEXT NOT NULL)`} {
		if _, err := db.Exec(q); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize runstate: %w", err)
		}
	}
	return &Store{db: db}, nil
}
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Checkpoint atomically writes next only if ExpectedRevision remains current.
// Use ExpectedRevision 0 only to create a new run.
func (s *Store) Checkpoint(ctx context.Context, next RunState, expectedRevision int64) (*RunState, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("runstate: nil store")
	}
	if err := validate(&next.BuildRun); err != nil {
		return nil, err
	}
	if expectedRevision < 0 {
		return nil, fmt.Errorf("%w: negative expected revision", ErrAmbiguous)
	}
	next.Revision = expectedRevision + 1
	next.UpdatedAt = time.Now().UTC()
	body, err := marshal(next)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO build_runs(id, revision, body, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET revision=excluded.revision, body=excluded.body, updated_at=excluded.updated_at
		WHERE build_runs.revision = ?`, next.ID, next.Revision, body, next.UpdatedAt.Format(time.RFC3339Nano), expectedRevision)
	if err != nil {
		return nil, fmt.Errorf("checkpoint runstate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("checkpoint rows: %w", err)
	}
	if n != 1 {
		return nil, ErrConcurrent
	}
	return clone(&next), nil
}

func (s *Store) Load(ctx context.Context, id string) (*RunState, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("runstate: nil store")
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: empty run id", ErrAmbiguous)
	}
	var body []byte
	if err := s.db.QueryRowContext(ctx, `SELECT body FROM build_runs WHERE id=?`, id).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("load runstate: %w", err)
	}
	state, err := unmarshal(body)
	if err != nil {
		return nil, err
	}
	if state.ID != id {
		return nil, fmt.Errorf("%w: row identity mismatch", ErrAmbiguous)
	}
	if err := validate(&state.BuildRun); err != nil {
		return nil, err
	}
	return state, nil
}

// Resume loads then revalidates every provider task revision and the graph
// revision. Any drift, missing task, or unknown status is a hard refusal.
func (s *Store) Resume(ctx context.Context, id string, a Authority) (*RunState, error) {
	state, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Tasks == nil || a.Graph == nil {
		return nil, fmt.Errorf("%w: missing provider or graph authority", ErrAmbiguous)
	}
	graph, err := a.Graph(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: graph unreadable: %v", ErrStale, err)
	}
	if strings.TrimSpace(graph) == "" || graph != state.DependencyGraphRevision {
		return nil, fmt.Errorf("%w: dependency graph revision changed", ErrStale)
	}
	for _, saved := range state.Tasks {
		live, err := a.Tasks.GetTask(ctx, saved.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: task %s unreadable: %v", ErrStale, saved.Ref, err)
		}
		if live == nil || live.ID != saved.ID || live.Ref != saved.Ref {
			return nil, fmt.Errorf("%w: task identity changed for %s", ErrStale, saved.Ref)
		}
		if string(provider.EncodeRevision(live)) != saved.ProviderRevision {
			return nil, fmt.Errorf("%w: provider revision changed for %s", ErrStale, saved.Ref)
		}
		if provider.NormalizeStatus(live.Status) != provider.NormalizeStatus(saved.Status) {
			return nil, fmt.Errorf("%w: task status changed for %s", ErrStale, saved.Ref)
		}
		if saved.Terminal != terminal(live.Status) {
			return nil, fmt.Errorf("%w: terminal state changed for %s", ErrStale, saved.Ref)
		}
	}
	return state, nil
}

// Dispatchable rejects tasks terminal in this exact saved run. It is intended
// to gate all resume redispatch paths before they call lifecycle/dispatch.
func (r *RunState) Dispatchable(ref string) error {
	for _, task := range r.Tasks {
		if task.Ref == ref {
			if task.Terminal {
				return fmt.Errorf("%w: %s", ErrTerminal, ref)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: task %s absent from run", ErrAmbiguous, ref)
}

func FromTasks(id, goal, ref, graph string, policy Policy, wave, level int, tasks []*provider.Task) (RunState, error) {
	r := RunState{BuildRun: BuildRun{SchemaVersion: SchemaVersion, ID: id, Goal: goal, Ref: ref, DependencyGraphRevision: graph, Policy: policy, Wave: wave, Level: level, Tasks: make([]TaskState, 0, len(tasks))}}
	for _, t := range tasks {
		if t == nil {
			return RunState{}, fmt.Errorf("%w: nil task", ErrAmbiguous)
		}
		r.Tasks = append(r.Tasks, TaskState{Ref: t.Ref, ID: t.ID, ProviderRevision: string(provider.EncodeRevision(t)), Status: provider.NormalizeStatus(t.Status), Terminal: terminal(t.Status)})
	}
	if err := validate(&r.BuildRun); err != nil {
		return RunState{}, err
	}
	return r, nil
}

func terminal(status string) bool { return provider.NormalizeStatus(status) == provider.StatusDone }
func marshal(state RunState) ([]byte, error) {
	b, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode runstate: %w", err)
	}
	return b, nil
}
func unmarshal(body []byte) (*RunState, error) {
	var state RunState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, fmt.Errorf("%w: malformed durable state: %v", ErrAmbiguous, err)
	}
	return &state, nil
}
func clone(in *RunState) *RunState {
	b, err := marshal(*in)
	if err != nil {
		return nil
	}
	out, err := unmarshal(b)
	if err != nil {
		return nil
	}
	return out
}
func validate(r *BuildRun) error {
	if r == nil || r.SchemaVersion != SchemaVersion || strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Goal) == "" || strings.TrimSpace(r.Ref) == "" || strings.TrimSpace(r.DependencyGraphRevision) == "" || strings.TrimSpace(r.Policy.Lane) == "" || strings.TrimSpace(r.Policy.Model) == "" || r.Wave < 0 || r.Level < 0 || len(r.Tasks) == 0 {
		return fmt.Errorf("%w: incomplete build run", ErrAmbiguous)
	}
	seen := map[string]bool{}
	for _, t := range r.Tasks {
		if t.Ref == "" || t.ID == "" || t.ProviderRevision == "" || t.Status == "" || seen[t.Ref] {
			return fmt.Errorf("%w: invalid or duplicate task", ErrAmbiguous)
		}
		seen[t.Ref] = true
		if t.Terminal != terminal(t.Status) {
			return fmt.Errorf("%w: terminal flag mismatch for %s", ErrAmbiguous, t.Ref)
		}
	}
	sort.Slice(r.Tasks, func(i, j int) bool { return r.Tasks[i].Ref < r.Tasks[j].Ref })
	return nil
}
