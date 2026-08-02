package router

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type ProviderType string

const (
	ProviderAnthropic ProviderType = "anthropic"
	ProviderGoogle    ProviderType = "google"
	ProviderOpenAI    ProviderType = "openai"
	ProviderXAI       ProviderType = "xai"
	ProviderOllama    ProviderType = "ollama"
)

type TokenTracker struct {
	TotalPromptTokens     int64
	TotalCompletionTokens int64
	TotalRequests         int64
}

func (t *TokenTracker) AddUsage(promptTokens, completionTokens int64) {
	atomic.AddInt64(&t.TotalPromptTokens, promptTokens)
	atomic.AddInt64(&t.TotalCompletionTokens, completionTokens)
	atomic.AddInt64(&t.TotalRequests, 1)
}

func (t *TokenTracker) Stats() (int64, int64, int64) {
	return atomic.LoadInt64(&t.TotalPromptTokens),
		atomic.LoadInt64(&t.TotalCompletionTokens),
		atomic.LoadInt64(&t.TotalRequests)
}

type ModelCandidate struct {
	Name            string
	Type            ProviderType
	Model           string
	ReasoningEffort string // low | medium | high | max
	CooldownUntil   time.Time
	Tracker         TokenTracker
}

type ModelRouter struct {
	mu         sync.RWMutex
	candidates []*ModelCandidate
	usageFunc  UsageFunc
}

// UsageFunc is an optional hook to check quota before provider selection.
// Return a utilization percentage (0.0–1.0) for the named provider/harness,
// or 0 if unknown. A UsageFunc that returns >= 1.0 marks the provider as exhausted.
type UsageFunc func(ctx context.Context, name string) float64

func NewModelRouter(candidates []*ModelCandidate) *ModelRouter {
	return &ModelRouter{
		candidates: candidates,
	}
}

// WithUsageFunc attaches a quota-aware usage function to the router.
func (r *ModelRouter) WithUsageFunc(fn UsageFunc) *ModelRouter {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usageFunc = fn
	return r
}

// SelectProvider returns the first available model candidate not currently in rate-limit cooldown
// and not exhausted by quota (when a UsageFunc is configured).
func (r *ModelRouter) SelectProvider(ctx context.Context) (*ModelCandidate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	for _, candidate := range r.candidates {
		if !now.After(candidate.CooldownUntil) {
			continue
		}
		if r.usageFunc != nil && r.usageFunc(ctx, candidate.Name) >= 1.0 {
			continue
		}
		return candidate, nil
	}

	return nil, fmt.Errorf("all AI model providers are currently exhausted or in rate-limit cooldown")
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

// RecordTokenBurn records token usage stats for a specific candidate provider
func (r *ModelRouter) RecordTokenBurn(name string, promptTokens, completionTokens int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, candidate := range r.candidates {
		if candidate.Name == name {
			candidate.Tracker.AddUsage(promptTokens, completionTokens)
			break
		}
	}
}
