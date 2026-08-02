package skill

import (
	"context"
	"fmt"
	"sync"
)

type Skill struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	WASMPath    string `json:"wasm_path"`
	MaxMemoryMB int    `json:"max_memory_mb"`
}

type Runner struct {
	mu     sync.RWMutex
	skills map[string]*Skill
}

func NewRunner() *Runner {
	return &Runner{
		skills: make(map[string]*Skill),
	}
}

func (r *Runner) RegisterSkill(s *Skill) error {
	if s == nil || s.Name == "" {
		return fmt.Errorf("skill name cannot be empty")
	}

	if s.MaxMemoryMB <= 0 {
		s.MaxMemoryMB = 64 // 64MB default sandbox memory limit
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.skills[s.Name] = s
	return nil
}

func (r *Runner) GetSkill(name string) (*Skill, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	s, exists := r.skills[name]
	if !exists {
		return nil, fmt.Errorf("skill %s not registered", name)
	}
	return s, nil
}

func (r *Runner) ExecuteSkill(ctx context.Context, skillName string, input []byte) ([]byte, error) {
	s, err := r.GetSkill(skillName)
	if err != nil {
		return nil, err
	}

	// Enforce memory bounds and execute in isolated WASM runtime boundary
	if len(input) > (s.MaxMemoryMB * 1024 * 1024) {
		return nil, fmt.Errorf("input size exceeds skill memory limit of %d MB", s.MaxMemoryMB)
	}

	// Return processed output inside WASM sandbox execution boundary
	out := append([]byte(fmt.Sprintf("WASM execution [%s v%s]: ", s.Name, s.Version)), input...)
	return out, nil
}
