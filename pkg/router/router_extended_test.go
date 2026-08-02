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

func TestSelectProvider_UsageFuncSkipsExhausted(t *testing.T) {
	primary := &ModelCandidate{Name: "claude-max", Type: ProviderAnthropic, Model: "claude-3-7-sonnet"}
	secondary := &ModelCandidate{Name: "gemini-pro", Type: ProviderGoogle, Model: "gemini-2.5-flash"}

	r := NewModelRouter([]*ModelCandidate{primary, secondary})
	r.WithUsageFunc(func(ctx context.Context, name string) float64 {
		if name == "claude-max" {
			return 1.0
		}
		return 0
	})

	selected, err := r.SelectProvider(context.Background())
	if err != nil || selected.Name != "gemini-pro" {
		t.Fatalf("expected fallback to gemini-pro, got %v (err: %v)", selected, err)
	}
}

func TestSelectProvider_UsageFuncAllExhausted(t *testing.T) {
	primary := &ModelCandidate{Name: "claude-max", Type: ProviderAnthropic, Model: "claude-3-7-sonnet"}
	secondary := &ModelCandidate{Name: "gemini-pro", Type: ProviderGoogle, Model: "gemini-2.5-flash"}

	r := NewModelRouter([]*ModelCandidate{primary, secondary})
	r.WithUsageFunc(func(ctx context.Context, name string) float64 {
		return 1.0
	})

	_, err := r.SelectProvider(context.Background())
	if err == nil {
		t.Fatal("expected error when all providers exhausted by UsageFunc")
	}
}

func TestSelectProvider_UsageFuncNil(t *testing.T) {
	primary := &ModelCandidate{Name: "claude-max", Type: ProviderAnthropic, Model: "claude-3-7-sonnet"}
	r := NewModelRouter([]*ModelCandidate{primary})

	// No usageFunc set — should fall back to cooldown-only behavior
	selected, err := r.SelectProvider(context.Background())
	if err != nil || selected.Name != "claude-max" {
		t.Fatalf("expected claude-max, got %v (err: %v)", selected, err)
	}
}
