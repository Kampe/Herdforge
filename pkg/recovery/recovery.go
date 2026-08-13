// Package recovery records durable, evidence-backed decisions for failed runs.
package recovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Disposition string

const (
	Retry    Disposition = "retry"
	Replan   Disposition = "replan"
	Skip     Disposition = "skip"
	Escalate Disposition = "escalate"
	Abandon  Disposition = "abandon"
)

var (
	ErrInvalid     = errors.New("recovery: invalid value")
	ErrNotFound    = errors.New("recovery: not found")
	ErrCycle       = errors.New("recovery: plan contains a cycle")
	ErrOrphan      = errors.New("recovery: plan contains an orphan")
	ErrMaxAttempts = errors.New("recovery: maximum attempts exceeded")
)

type Decision struct {
	Run         string      `json:"run"`
	Task        string      `json:"task"`
	Actor       string      `json:"actor"`
	Evidence    string      `json:"evidence"`
	Disposition Disposition `json:"disposition"`
}

type Task struct {
	ID       string   `json:"id"`
	Depends  []string `json:"depends,omitempty"`
	Terminal bool     `json:"terminal,omitempty"`
}

type Plan struct {
	Tasks []Task `json:"tasks"`
}

type state struct {
	Decisions []Decision      `json:"decisions"`
	Attempts  map[string]int  `json:"attempts"`
	Plans     map[string]Plan `json:"plans"`
}

type Store struct {
	mu          sync.Mutex
	path        string
	maxAttempts int
	s           state
}

func Open(path string, maxAttempts ...int) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: empty path", ErrInvalid)
	}
	m := 3
	if len(maxAttempts) > 0 {
		m = maxAttempts[0]
	}
	if m < 1 {
		return nil, fmt.Errorf("%w: max attempts must be positive", ErrInvalid)
	}
	x := &Store{path: path, maxAttempts: m, s: state{Attempts: map[string]int{}, Plans: map[string]Plan{}}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return x, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recovery: read store: %w", err)
	}
	if err := json.Unmarshal(b, &x.s); err != nil {
		return nil, fmt.Errorf("recovery: decode store: %w", err)
	}
	if x.s.Attempts == nil {
		x.s.Attempts = map[string]int{}
	}
	if x.s.Plans == nil {
		x.s.Plans = map[string]Plan{}
	}
	return x, nil
}

func (s *Store) Close() error { return nil }

func (s *Store) persist() error {
	b, err := json.MarshalIndent(s.s, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func validDisposition(d Disposition) bool {
	switch d {
	case Retry, Replan, Skip, Escalate, Abandon:
		return true
	}
	return false
}

func validateDecision(d Decision) error {
	if strings.TrimSpace(d.Run) == "" || strings.TrimSpace(d.Task) == "" || strings.TrimSpace(d.Actor) == "" || strings.TrimSpace(d.Evidence) == "" || !validDisposition(d.Disposition) {
		return fmt.Errorf("%w: decision requires run, task, actor, evidence, and a known disposition", ErrInvalid)
	}
	return nil
}

func (s *Store) Decide(d Decision) error {
	if s == nil {
		return fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := validateDecision(d); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.Decisions = append(s.s.Decisions, d)
	if err := s.persist(); err != nil {
		s.s.Decisions = s.s.Decisions[:len(s.s.Decisions)-1]
		return err
	}
	return nil
}

func (s *Store) Decisions(run, task string) []Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Decision
	for _, d := range s.s.Decisions {
		if (run == "" || d.Run == run) && (task == "" || d.Task == task) {
			out = append(out, d)
		}
	}
	return out
}

func (s *Store) Attempt(run, task string) (int, error) {
	if strings.TrimSpace(run) == "" || strings.TrimSpace(task) == "" {
		return 0, fmt.Errorf("%w: attempt key", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := run + "\x00" + task
	if s.s.Attempts[k] >= s.maxAttempts {
		return s.s.Attempts[k], ErrMaxAttempts
	}
	s.s.Attempts[k]++
	if err := s.persist(); err != nil {
		s.s.Attempts[k]--
		return 0, err
	}
	return s.s.Attempts[k], nil
}

func (s *Store) Attempts(run, task string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s.Attempts[run+"\x00"+task]
}

func validatePlan(p Plan) error {
	if len(p.Tasks) == 0 {
		return fmt.Errorf("%w: empty plan", ErrInvalid)
	}
	nodes := map[string]Task{}
	for _, t := range p.Tasks {
		if strings.TrimSpace(t.ID) == "" || nodes[t.ID].ID != "" {
			return fmt.Errorf("%w: duplicate or empty task", ErrInvalid)
		}
		nodes[t.ID] = t
	}
	indeg := map[string]int{}
	for id, t := range nodes {
		for _, dep := range t.Depends {
			if _, ok := nodes[dep]; !ok {
				return fmt.Errorf("%w: %s depends on %s", ErrOrphan, id, dep)
			}
			indeg[id]++
		}
	}
	queue := make([]string, 0)
	for id := range nodes {
		if indeg[id] == 0 {
			queue = append(queue, id)
		}
	}
	seen := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		seen++
		for child, t := range nodes {
			for _, dep := range t.Depends {
				if dep == id {
					indeg[child]--
					if indeg[child] == 0 {
						queue = append(queue, child)
					}
				}
			}
		}
	}
	if seen != len(nodes) {
		return ErrCycle
	}
	return nil
}

func (s *Store) Replan(run string, p Plan) error {
	if strings.TrimSpace(run) == "" {
		return fmt.Errorf("%w: empty run", ErrInvalid)
	}
	if err := validatePlan(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.s.Plans[run]; ok {
		terminal := map[string]bool{}
		for _, t := range old.Tasks {
			if t.Terminal {
				terminal[t.ID] = true
			}
		}
		for _, t := range p.Tasks {
			if terminal[t.ID] && !t.Terminal {
				return fmt.Errorf("%w: terminal binding changed for %s", ErrInvalid, t.ID)
			}
		}
		for id := range terminal {
			found := false
			for _, t := range p.Tasks {
				if t.ID == id {
					found = true
					if !t.Terminal {
						return fmt.Errorf("%w: terminal binding changed for %s", ErrInvalid, id)
					}
				}
			}
			if !found {
				return fmt.Errorf("%w: terminal binding removed for %s", ErrInvalid, id)
			}
		}
	}
	s.s.Plans[run] = p
	if err := s.persist(); err != nil {
		return err
	}
	return nil
}

func (s *Store) Plan(run string) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.s.Plans[run]
	if !ok {
		return Plan{}, ErrNotFound
	}
	return p, nil
}
