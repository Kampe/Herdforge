package budget

import (
	"fmt"
	"strings"
	"sync"
)

type ModelRate struct {
	InputPer1K  float64
	OutputPer1K float64
}

var DefaultRates = map[string]ModelRate{
	"claude-3-5-sonnet": {InputPer1K: 0.003, OutputPer1K: 0.015},
	"gpt-4o":            {InputPer1K: 0.0025, OutputPer1K: 0.010},
	"gemini-1-5-pro":    {InputPer1K: 0.00125, OutputPer1K: 0.005},
	"deepseek-v3":       {InputPer1K: 0.00014, OutputPer1K: 0.00028},
}

type BudgetManager struct {
	mu           sync.RWMutex
	MaxBudgetUSD float64
	TotalCostUSD float64
	TotalTokens  int64
	Rates        map[string]ModelRate
}

func NewBudgetManager(maxBudgetUSD float64) *BudgetManager {
	return &BudgetManager{
		MaxBudgetUSD: maxBudgetUSD,
		TotalCostUSD: 0.0,
		TotalTokens:  0,
		Rates:        DefaultRates,
	}
}

func (bm *BudgetManager) CalculateCost(model string, inputTokens, outputTokens int) float64 {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	rate, exists := bm.Rates[strings.ToLower(model)]
	if !exists {
		// Fallback rate: $0.002 per 1k input, $0.008 per 1k output
		rate = ModelRate{InputPer1K: 0.002, OutputPer1K: 0.008}
	}

	inputCost := (float64(inputTokens) / 1000.0) * rate.InputPer1K
	outputCost := (float64(outputTokens) / 1000.0) * rate.OutputPer1K
	return inputCost + outputCost
}

func (bm *BudgetManager) RecordUsage(model string, inputTokens, outputTokens int) (float64, error) {
	cost := bm.CalculateCost(model, inputTokens, outputTokens)

	bm.mu.Lock()
	defer bm.mu.Unlock()

	if bm.MaxBudgetUSD > 0 && (bm.TotalCostUSD+cost) > bm.MaxBudgetUSD {
		return cost, fmt.Errorf("budget exceeded: current spend $%.4f + usage $%.4f exceeds limit $%.4f", bm.TotalCostUSD, cost, bm.MaxBudgetUSD)
	}

	bm.TotalCostUSD += cost
	bm.TotalTokens += int64(inputTokens + outputTokens)
	return cost, nil
}

func (bm *BudgetManager) IsExhausted() bool {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	if bm.MaxBudgetUSD <= 0 {
		return false
	}
	return bm.TotalCostUSD >= bm.MaxBudgetUSD
}
