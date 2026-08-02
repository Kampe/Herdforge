package router

import (
	"context"
	"testing"
	"time"
)

func TestSelectProvider_AllOnCooldown(t *testing.T) {
	primary := &ModelCandidate{Name: "p1", Type: ProviderAnthropic, Model: "claude-3-7-sonnet", CooldownUntil: time.Now().Add(10 * time.Minute)}
	secondary := &ModelCandidate{Name: "p2", Type: ProviderGoogle, Model: "gemini-2.5-flash", CooldownUntil: time.Now().Add(10 * time.Minute)}

	r := NewModelRouter([]*ModelCandidate{primary, secondary})
	_, err := r.SelectProvider(context.Background())
	if err == nil {
		t.Fatal("expected error when all providers on cooldown")
	}
}

func TestRecordTokenBurn_UnknownName(t *testing.T) {
	primary := &ModelCandidate{Name: "known", Type: ProviderAnthropic, Model: "claude"}
	r := NewModelRouter([]*ModelCandidate{primary})

	// Should not panic
	r.RecordTokenBurn("unknown", 100, 50)
	p, c, reqs := primary.Tracker.Stats()
	if p != 0 || c != 0 || reqs != 0 {
		t.Errorf("expected no token burn for unknown name, got p=%d c=%d reqs=%d", p, c, reqs)
	}
}

func TestReportRateLimit_UnknownName(t *testing.T) {
	primary := &ModelCandidate{Name: "known", Type: ProviderAnthropic, Model: "claude"}
	r := NewModelRouter([]*ModelCandidate{primary})

	// Should not panic
	r.ReportRateLimit("unknown", 5*time.Minute)

	selected, err := r.SelectProvider(context.Background())
	if err != nil || selected.Name != "known" {
		t.Fatalf("expected 'known' still selectable, got %v (err: %v)", selected, err)
	}
}
