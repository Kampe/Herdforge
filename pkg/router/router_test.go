package router

import (
	"context"
	"testing"
	"time"
)

func TestModelRouter_Fallback(t *testing.T) {
	primary := &ModelCandidate{Name: "primary-claude", Type: ProviderAnthropic, Model: "claude-3-7-sonnet"}
	secondary := &ModelCandidate{Name: "secondary-gemini", Type: ProviderGoogle, Model: "gemini-2.5-flash"}

	r := NewModelRouter([]*ModelCandidate{primary, secondary})

	// Initial selection should be primary
	selected, err := r.SelectProvider(context.Background())
	if err != nil || selected.Name != "primary-claude" {
		t.Fatalf("expected primary-claude, got %v (err: %v)", selected, err)
	}

	// Trigger 429 rate limit on primary for 1 minute
	r.ReportRateLimit("primary-claude", 1*time.Minute)

	// Selection should fall back to secondary
	selected, err = r.SelectProvider(context.Background())
	if err != nil || selected.Name != "secondary-gemini" {
		t.Fatalf("expected fallback to secondary-gemini, got %v (err: %v)", selected, err)
	}
}

func TestModelRouter_TokenTracker(t *testing.T) {
	primary := &ModelCandidate{Name: "claude-pro", Type: ProviderAnthropic, Model: "claude-3-7-sonnet"}
	r := NewModelRouter([]*ModelCandidate{primary})

	r.RecordTokenBurn("claude-pro", 1500, 300)
	p, c, reqs := primary.Tracker.Stats()

	if p != 1500 || c != 300 || reqs != 1 {
		t.Errorf("unexpected token usage stats: p=%d, c=%d, reqs=%d", p, c, reqs)
	}
}
