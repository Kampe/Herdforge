package main

import (
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/usage"
)

func TestQuotaObservationTimeUnknownIsZeroNotGeneratedAt(t *testing.T) {
	generated := time.Date(2026, 9, 22, 20, 16, 0, 0, time.UTC)
	observed := time.Date(2026, 9, 22, 19, 41, 12, 0, time.UTC)
	snap := &usage.UsageSnapshot{
		GeneratedAt: generated,
		Providers: map[string]usage.ProviderUsage{
			"claude": {ObservedAt: observed},
			"grok":   {ObservedAt: time.Time{}},
		},
	}
	if got := quotaObservationTime(snap, "claude"); !got.Equal(observed) {
		t.Fatalf("claude = %v, want banked %v", got, observed)
	}
	if got := quotaObservationTime(snap, "grok"); !got.IsZero() {
		t.Fatalf("zero ObservedAt became %v (GeneratedAt=%v)", got, generated)
	}
	if got := quotaObservationTime(snap, "codex"); !got.IsZero() {
		t.Fatalf("missing provider became %v (GeneratedAt=%v)", got, generated)
	}
	if got := quotaObservationTime(nil, "claude"); !got.IsZero() {
		t.Fatalf("nil snap became %v", got)
	}
}

func TestQuotaObservationTimeAliasesSnapshotKeys(t *testing.T) {
	agyObs := time.Date(2026, 9, 22, 18, 0, 0, 0, time.UTC)
	ocObs := time.Date(2026, 9, 22, 18, 5, 0, 0, time.UTC)
	snap := &usage.UsageSnapshot{
		GeneratedAt: time.Date(2026, 9, 22, 20, 16, 0, 0, time.UTC),
		Providers: map[string]usage.ProviderUsage{
			"antigravity": {ObservedAt: agyObs},
			"opencode":    {ObservedAt: ocObs},
		},
	}
	if got := quotaObservationTime(snap, "agy"); !got.Equal(agyObs) {
		t.Fatalf("agy -> antigravity key = %v, want %v", got, agyObs)
	}
	if got := quotaObservationTime(snap, "antigravity"); !got.Equal(agyObs) {
		t.Fatalf("antigravity exact key = %v, want %v", got, agyObs)
	}
	if got := quotaObservationTime(snap, "opencode"); !got.Equal(ocObs) {
		t.Fatalf("opencode snapshot key = %v, want %v", got, ocObs)
	}
	if got := quotaObservationTime(snap, "opencode-zen"); !got.IsZero() {
		t.Fatalf("unknown opencode-* alias must stay unknown, got %v", got)
	}
	agyOnly := &usage.UsageSnapshot{GeneratedAt: snap.GeneratedAt, Providers: map[string]usage.ProviderUsage{
		"agy": {ObservedAt: agyObs},
	}}
	if got := quotaObservationTime(agyOnly, "antigravity"); !got.Equal(agyObs) {
		t.Fatalf("antigravity -> agy key = %v, want %v", got, agyObs)
	}
}

func TestOldestProviderObservationSkipsZeroAndGeneratedAt(t *testing.T) {
	generated := time.Date(2026, 9, 22, 20, 16, 0, 0, time.UTC)
	older := time.Date(2026, 9, 22, 19, 41, 12, 0, time.UTC)
	newer := time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)
	snap := &usage.UsageSnapshot{
		GeneratedAt: generated,
		Providers: map[string]usage.ProviderUsage{
			"claude": {ObservedAt: older},
			"grok":   {ObservedAt: time.Time{}},
			"codex":  {ObservedAt: newer},
		},
	}
	if got := oldestProviderObservation(snap); !got.Equal(older) {
		t.Fatalf("oldest = %v, want %v not GeneratedAt %v", got, older, generated)
	}
	empty := &usage.UsageSnapshot{GeneratedAt: generated, Providers: map[string]usage.ProviderUsage{
		"grok": {ObservedAt: time.Time{}},
	}}
	if got := oldestProviderObservation(empty); !got.IsZero() {
		t.Fatalf("all-unknown oldest = %v, want zero not GeneratedAt", got)
	}
}
