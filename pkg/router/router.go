package router

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type ProviderType string

const (
	ProviderAnthropic ProviderType = "anthropic"
	ProviderGoogle    ProviderType = "google"
	ProviderOpenAI    ProviderType = "openai"
	ProviderOllama    ProviderType = "ollama"
)

type ModelCandidate struct {
	Name            string
	Type            ProviderType
	Model           string
	CooldownUntil   time.Time
}

type ModelRouter struct {
	mu         sync.RWMutex
	candidates []*ModelCandidate
}

func NewModelRouter(candidates []*ModelCandidate) *ModelRouter {
	return &ModelRouter{
		candidates: candidates,
	}
}

// SelectProvider returns the first available model candidate not currently in rate-limit cooldown
func (r *ModelRouter) SelectProvider(ctx context.Context) (*ModelCandidate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	for _, candidate := range r.candidates {
		if now.After(candidate.CooldownUntil) {
			return candidate, nil
		}
	}

	return nil, fmt.Errorf("all AI model providers are currently in rate-limit cooldown")
}

// ReportRateLimit triggers a cooldown period for a specific candidate (e.g. on 429 response)
func (r *ModelRouter) ReportRateLimit(name string, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, candidate := range r.candidates {
		if candidate.Name == name {
			candidate.CooldownUntil = time.Now().Add(duration)
			break
		}
	}
}
