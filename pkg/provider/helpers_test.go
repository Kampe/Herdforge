package provider

import (
	"context"
	"testing"
)

func TestParsePriorityString(t *testing.T) {
	if ParsePriorityString("urgent") != PriorityUrgent {
		t.Errorf("expected PriorityUrgent")
	}
	if ParsePriorityString("high") != PriorityHigh {
		t.Errorf("expected PriorityHigh")
	}
	if ParsePriorityString("unknown") != PriorityMedium {
		t.Errorf("expected PriorityMedium fallback")
	}
}

func TestVerifyProviderContract(t *testing.T) {
	mp := NewMemoryProvider()
	if err := VerifyProviderContract(context.Background(), mp, "proj-1"); err != nil {
		t.Errorf("expected clean contract verification, got: %v", err)
	}
}
