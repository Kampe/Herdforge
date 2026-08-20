package standing

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// LoopMode describes the contract a standing lane is currently running.
// Held is deliberately distinct from idle: a hold clears the loop and must
// not be mistaken for available capacity. OneShot describes a prompt that was
// accepted while held; it does not lift the hold or re-arm the loop.
type LoopMode string

const (
	LoopRunning LoopMode = "running"
	LoopHeld    LoopMode = "held"
	LoopOneShot LoopMode = "one-shot"
)

type PromptDisposition string

const (
	PromptStanding PromptDisposition = "standing"
	PromptOneShot  PromptDisposition = "one-shot"
)

var (
	ErrLoopHeld        = errors.New("standing lane is held")
	ErrLoopContract    = errors.New("standing lane loop contract is incomplete")
	ErrLoopAlreadyHeld = errors.New("standing lane is already held")
)

// LoopState is the inspectable, per-lane loop contract. Goal and Wakeup are
// both required to run; keeping them together makes release validation
// fail-closed instead of restoring only half of the standing contract.
type LoopState struct {
	Lane   string   `json:"lane"`
	Mode   LoopMode `json:"mode"`
	Goal   string   `json:"goal,omitempty"`
	Wakeup string   `json:"wakeup,omitempty"`
}

func (s LoopState) validate() error {
	if strings.TrimSpace(s.Lane) == "" {
		return fmt.Errorf("%w: lane is required", ErrLoopContract)
	}
	if strings.TrimSpace(s.Goal) == "" || strings.TrimSpace(s.Wakeup) == "" {
		return ErrLoopContract
	}
	if s.Mode != LoopRunning && s.Mode != LoopHeld && s.Mode != LoopOneShot {
		return fmt.Errorf("%w: unknown mode %q", ErrLoopContract, s.Mode)
	}
	return nil
}

// LoopRegistry is the serialized transition boundary used by status and
// release callers. A caller must provide both declared values on release;
// validation happens before the state is changed.
type LoopRegistry struct {
	mu    sync.Mutex
	lanes map[string]LoopState
}

func NewLoopRegistry(states []LoopState) (*LoopRegistry, error) {
	r := &LoopRegistry{lanes: make(map[string]LoopState, len(states))}
	for _, state := range states {
		if err := state.validate(); err != nil {
			return nil, err
		}
		if _, exists := r.lanes[state.Lane]; exists {
			return nil, fmt.Errorf("%w: duplicate lane %q", ErrLoopContract, state.Lane)
		}
		r.lanes[state.Lane] = state
	}
	return r, nil
}

func (r *LoopRegistry) State(lane string) (LoopState, error) {
	if r == nil {
		return LoopState{}, ErrLoopContract
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.lanes[strings.TrimSpace(lane)]
	if !ok {
		return LoopState{}, fmt.Errorf("%w: lane %q is unknown", ErrLoopContract, lane)
	}
	return s, nil
}

// Hold clears both re-armable values as well as changing the visible mode.
// The declared values are retained by the registry's release contract so a
// later explicit release can restore them together.
func (r *LoopRegistry) Hold(lane string) error {
	if r == nil {
		return ErrLoopContract
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.TrimSpace(lane)
	s, ok := r.lanes[key]
	if !ok {
		return fmt.Errorf("%w: lane %q is unknown", ErrLoopContract, lane)
	}
	if s.Mode == LoopHeld {
		return ErrLoopAlreadyHeld
	}
	s.Mode, s.Goal, s.Wakeup = LoopHeld, "", ""
	r.lanes[key] = s
	return nil
}

// Release atomically validates and restores the goal and wakeup. If either
// value is missing, the held state is unchanged.
func (r *LoopRegistry) Release(lane, goal, wakeup string) error {
	if r == nil {
		return ErrLoopContract
	}
	if strings.TrimSpace(goal) == "" || strings.TrimSpace(wakeup) == "" {
		return ErrLoopContract
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.TrimSpace(lane)
	s, ok := r.lanes[key]
	if !ok {
		return fmt.Errorf("%w: lane %q is unknown", ErrLoopContract, lane)
	}
	if s.Mode != LoopHeld {
		return fmt.Errorf("%w: lane %q mode=%s", ErrLoopContract, lane, s.Mode)
	}
	s.Mode, s.Goal, s.Wakeup = LoopRunning, strings.TrimSpace(goal), strings.TrimSpace(wakeup)
	r.lanes[key] = s
	return nil
}

// Prompt records a prompt to a held lane as one-shot and leaves the hold in
// place. It never claims to have resumed the standing loop.
func (r *LoopRegistry) Prompt(lane string) (PromptDisposition, error) {
	if r == nil {
		return "", ErrLoopContract
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.TrimSpace(lane)
	s, ok := r.lanes[key]
	if !ok {
		return "", fmt.Errorf("%w: lane %q is unknown", ErrLoopContract, lane)
	}
	if s.Mode == LoopHeld || s.Mode == LoopOneShot {
		s.Mode = LoopOneShot
		r.lanes[key] = s
		return PromptOneShot, nil
	}
	return PromptStanding, nil
}
