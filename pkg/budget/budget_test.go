package budget

import (
	"testing"
)

func TestBudgetManager_CalculateCostAndExhaustion(t *testing.T) {
	bm := NewBudgetManager(0.05) // $0.05 limit

	cost, err := bm.RecordUsage("claude-3-5-sonnet", 1000, 1000) // $0.003 + $0.015 = $0.018
	if err != nil {
		t.Fatalf("expected usage within budget, got err: %v", err)
	}

	if cost < 0.017 || cost > 0.019 {
		t.Errorf("unexpected cost calculation: $%.4f", cost)
	}

	if bm.IsExhausted() {
		t.Errorf("expected budget not to be exhausted yet")
	}

	// Record large usage to trigger budget exhaustion error
	_, err = bm.RecordUsage("gpt-4o", 20000, 10000)
	if err == nil {
		t.Errorf("expected budget exceeded error, got nil")
	}
}

func TestBudgetManager_FallbackRate(t *testing.T) {
	bm := NewBudgetManager(1.0)
	cost := bm.CalculateCost("unknown-model", 1000, 1000)
	if cost != 0.010 { // 0.002 + 0.008
		t.Errorf("expected fallback cost 0.010, got %.4f", cost)
	}
}

func TestIsExhausted_ExactLimit(t *testing.T) {
	bm := NewBudgetManager(0.05)
	bm.TotalCostUSD = 0.05
	if !bm.IsExhausted() {
		t.Error("expected IsExhausted to return true when TotalCostUSD >= MaxBudgetUSD")
	}
}

func TestIsExhausted_NoLimit(t *testing.T) {
	bm := NewBudgetManager(0)
	if bm.IsExhausted() {
		t.Error("expected IsExhausted to return false when MaxBudgetUSD <= 0")
	}
}
